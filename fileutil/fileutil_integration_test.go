//go:build integration && unix

package fileutil

// Integration tests for fileutil.
//
// fileutil_test.go already runs against t.TempDir(), so "integration" here
// does not mean "now with disk". It means the claims that depend on the
// FILESYSTEM, the PROCESS IDENTITY, or on TIMING rather than on this
// code — the ones a unit test would have to assume rather than assert:
//
//   - Case. SafeName does not lowercase, so "Scan.PDF" and "scan.pdf" are
//     two stored files on ext4 and one on APFS. Which it is decides
//     whether Write overwrites or adds.
//   - Timing. Write promises a concurrent reader sees the complete old
//     file or the complete new one and never a truncated prefix. Only a
//     reader actually racing a writer can show that.
//   - Identity. A directory the process may not write to, and one owned by
//     somebody else that Write must not fail on merely because it cannot
//     widen the permissions.
//   - Scale. Every unit test writes a handful of bytes. The temp-sync-
//     chmod-rename-sync path is where a size-dependent mistake would live.
//
// The unix build tag matches fileutil_test.go's reasoning: these assert
// permission bits and use syscall, neither of which means anything on
// Windows.
//
//	go test -tags integration -run Integration ./fileutil
//
// Each test skips rather than fails where the environment cannot produce
// its precondition. A skip says the claim was not checked here; a failure
// would say the code is wrong, and that would be a lie.

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const itID = 4321

// firstError is a one-shot error slot: the first failure wins and the
// reader goroutine stops, so a genuine partial read is reported once
// rather than thousands of times.
type firstError struct {
	mu  sync.Mutex
	msg string
}

// caseSensitiveDir reports whether dir distinguishes two names differing
// only in case, by asking the filesystem rather than guessing from GOOS: a
// Linux host can mount a case-folding volume and a Mac can format a
// case-sensitive one.
func caseSensitiveDir(t *testing.T, dir string) bool {
	t.Helper()
	probe := filepath.Join(dir, ".case-probe")
	if err := os.WriteFile(probe, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Remove(probe) }()
	_, err := os.Stat(filepath.Join(dir, ".CASE-PROBE"))
	return err != nil
}

func requireUnprivileged(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits do not bite")
	}
}

// describe summarises a mismatched read without dumping a megabyte into
// the test log.
func describe(b []byte) string {
	if len(b) == 0 {
		return "0 bytes"
	}
	return fmt.Sprintf("%d bytes, starting %q and ending %q",
		len(b), b[0], b[len(b)-1])
}

func (a *firstError) set(s string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.msg == "" {
		a.msg = s
	}
}

func (a *firstError) get() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.msg
}

// SafeName deliberately preserves case — a stored name is handed back to a
// browser and "Report.PDF" should not become "report.pdf". The cost is
// that whether two spellings are one file is the filesystem's decision,
// not this package's, and a caller that treats names as unique keys is
// relying on whichever one it happens to be deployed on.
func TestIntegrationCaseIsTheFilesystemsDecision(t *testing.T) {
	dir := t.TempDir()

	lower, err := Write(dir, itID, "scan.pdf", []byte("lower"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	upper, err := Write(dir, itID, "SCAN.PDF", []byte("upper"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if lower == upper {
		t.Fatalf("both spellings stored as %q; SafeName lowercased", lower)
	}

	entries, err := os.ReadDir(Dir(dir, itID))
	if err != nil {
		t.Fatal(err)
	}

	if caseSensitiveDir(t, Dir(dir, itID)) {
		if len(entries) != 2 {
			t.Errorf("%d files, want 2 on a case-sensitive filesystem",
				len(entries))
		}
		got, err := Read(dir, itID, "scan.pdf")
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "lower" {
			t.Errorf("scan.pdf = %q, want the first write intact", got)
		}
		return
	}

	// Case-folding: the second Write replaced the first, so the record
	// holds one file whose NAME is the first spelling and whose BYTES are
	// the second. A caller storing both names has one row pointing at
	// content it did not write.
	if len(entries) != 1 {
		t.Errorf("%d files, want 1 on a case-folding filesystem",
			len(entries))
	}
	got, err := Read(dir, itID, "scan.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "upper" {
		t.Errorf("scan.pdf = %q, want the second write to have replaced "+
			"it", got)
	}
}

// Write's headline claim: the temp-then-rename means a reader arriving
// mid-write sees one complete version or the other, never a prefix.
//
// Writing directly to the destination would fail this within a few
// iterations, which is what makes it worth running rather than asserting.
func TestIntegrationWriteIsAtomicForAConcurrentReader(t *testing.T) {
	dir := t.TempDir()

	old := bytes.Repeat([]byte("A"), 1<<20)
	fresh := bytes.Repeat([]byte("B"), 1<<20)
	if _, err := Write(dir, itID, "doc.pdf", old); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var bad firstError

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			got, err := Read(dir, itID, "doc.pdf")
			if err != nil {
				// The name never disappears: the rename replaces it in
				// one step, so a miss here is itself a failure.
				bad.set("read failed mid-write: " + err.Error())
				return
			}
			if !bytes.Equal(got, old) && !bytes.Equal(got, fresh) {
				bad.set("reader saw neither version whole: " +
					describe(got))
				return
			}
		}
	}()

	for i := 0; i < 50; i++ {
		payload := fresh
		if i%2 == 0 {
			payload = old
		}
		if _, err := Write(dir, itID, "doc.pdf", payload); err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
	}
	close(stop)
	wg.Wait()

	if msg := bad.get(); msg != "" {
		t.Error(msg)
	}
}

// The promised modes, checked as modes rather than assumed. The unit suite
// proves the umask is defeated; this proves the numbers in the const block
// are the ones that end up on disk.
func TestIntegrationStoredPermissions(t *testing.T) {
	dir := t.TempDir()
	if _, err := Write(dir, itID, "scan.pdf", []byte("x")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	for _, c := range []struct {
		what string
		path string
		want os.FileMode
	}{
		{"record directory", Dir(dir, itID), dirPerm},
		{"stored file", Path(dir, itID, "scan.pdf"), filePerm},
	} {
		info, err := os.Stat(c.path)
		if err != nil {
			t.Fatalf("%s: %v", c.what, err)
		}
		if got := info.Mode().Perm(); got != c.want {
			t.Errorf("%s has mode %o, want %o", c.what, got, c.want)
		}
	}
}

// Write RE-WIDENS a record directory it owns, even one deliberately made
// read-only, and that is the documented intent rather than a gap: the
// chmod is there so a single run under a narrow umask cannot make a record
// permanently unreadable.
//
// It is worth pinning because it is surprising in the moment — a
// mode-0555 directory looks like it should refuse a write, and it does not
// when the process owns it, because chmod by the owner ignores the mode
// bits it is changing.
func TestIntegrationWriteRewidensADirectoryItOwns(t *testing.T) {
	requireUnprivileged(t)

	root := t.TempDir()
	recordDir := Dir(root, itID)
	if err := os.MkdirAll(recordDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(recordDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(recordDir, 0o755) })

	if _, err := Write(root, itID, "scan.pdf", []byte("x")); err != nil {
		t.Fatalf("Write: %v — an owned directory should be widened", err)
	}
	info, err := os.Stat(recordDir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != dirPerm {
		t.Errorf("directory mode %o after Write, want %o", got, dirPerm)
	}
}

// The genuinely unwritable case, which is the realistic misconfiguration:
// an upload ROOT the process may not write into — a volume mounted
// read-only, or one owned by another account. mkdirAll cannot create the
// record directory and there is nothing to chmod, so this must fail rather
// than silently store nothing.
func TestIntegrationUnwritableUploadRootFails(t *testing.T) {
	requireUnprivileged(t)

	parent := t.TempDir()
	root := filepath.Join(parent, "uploads")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o755) })

	_, err := Write(root, itID, "scan.pdf", []byte("x"))
	if err == nil {
		t.Fatal("Write succeeded under a read-only upload root")
	}
	if !errors.Is(err, fs.ErrPermission) {
		t.Errorf("err = %v, want it to wrap fs.ErrPermission", err)
	}
	// The error has to name the directory, or the misconfiguration is not
	// diagnosable from the one line a caller logs.
	if !strings.Contains(err.Error(), root) {
		t.Errorf("err = %v, want it to name %q", err, root)
	}
}

// An upload ROOT that is not a directory is the misconfiguration mkdirAll
// builds a specific error for, and the reason it does not use os.Mkdir:
// EEXIST would print as "file exists" and read as "the directory is
// already there".
func TestIntegrationUploadRootIsAFile(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "uploads")
	if err := os.WriteFile(root, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Write(root, itID, "scan.pdf", []byte("x"))
	if err == nil {
		t.Fatal("Write succeeded against a regular file as upload root")
	}
	var pathErr *fs.PathError
	if !errors.As(err, &pathErr) {
		t.Fatalf("err = %v, want an *fs.PathError naming the path", err)
	}
	if !errors.Is(err, errNotDir) {
		t.Errorf("err = %v, want it to carry errNotDir", err)
	}
}

// Every unit test writes a few bytes. This one writes at the scale the
// package is actually configured for, through the whole temp-sync-chmod-
// rename-sync path, and reads it back byte for byte.
func TestIntegrationMegabyteRoundTrip(t *testing.T) {
	dir := t.TempDir()

	// A repeating pattern rather than zeroes, so a sparse-file or
	// truncation bug cannot pass by accident.
	data := make([]byte, 8<<20)
	for i := range data {
		data[i] = byte(i % 251)
	}

	name, err := Write(dir, itID, "big.pdf", data)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	info, err := Stat(dir, itID, name)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size() != int64(len(data)) {
		t.Fatalf("stored %d bytes, want %d", info.Size(), len(data))
	}

	got, err := Read(dir, itID, name)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Error("the bytes read back differ from the bytes written")
	}

	// No temp file survived the rename.
	entries, err := os.ReadDir(Dir(dir, itID))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("%d entries, want 1 — a temp file survived", len(entries))
	}
}

// CopyUnique claims each name is claimed atomically with O_CREATE|O_EXCL,
// so concurrent copies under one record step to successive suffixes rather
// than overwriting each other. The unit suite covers this; running it at a
// larger width, on a real filesystem, is what would expose a gap that only
// opens under contention.
func TestIntegrationConcurrentCopiesNeverShareAName(t *testing.T) {
	dir := t.TempDir()
	const src = "report.docx"

	// restoreExts is the unit suite's helper, and using it is not optional:
	// the extension table is package state, and a registration left behind
	// here changes what every later test in the same process accepts. The
	// first version of this test omitted it, and passed — because it was only
	// ever run with -run Integration, where no unit test shared its process.
	// Run together, TestHasAllowedExt started accepting .xlsx.
	restoreExts(t)
	RegisterExts(OfficeExts)

	if _, err := Write(dir, itID, src, []byte("original")); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	const workers = 24
	names := make([]string, workers)
	errs := make([]error, workers)
	start := make(chan struct{})

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			names[i], _, errs[i] = CopyUnique(dir, itID, src, "copy.docx")
		}(i)
	}
	close(start)
	wg.Wait()

	seen := make(map[string]int, workers)
	for i, err := range errs {
		if err != nil {
			t.Errorf("worker %d: %v", i, err)
			continue
		}
		seen[names[i]]++
	}
	for name, n := range seen {
		if n > 1 {
			t.Errorf("%d workers were given the name %q", n, name)
		}
	}
	if len(seen) != workers {
		t.Errorf("%d distinct names for %d copies", len(seen), workers)
	}

	// Every copy has to hold the source bytes: a name handed out twice
	// would show up here as a short or doubled file even if the count
	// above happened to line up.
	for name := range seen {
		got, err := Read(dir, itID, name)
		if err != nil {
			t.Errorf("reading %q: %v", name, err)
			continue
		}
		if string(got) != "original" {
			t.Errorf("%q = %q, want the source bytes", name, got)
		}
	}
}

// Remove is best-effort and discards errors, which makes its one guard the
// thing worth checking against a real directory: a name that SafeName
// folds onto FallbackName must not delete a file genuinely stored under
// that name.
func TestIntegrationRemoveDoesNotEatTheFallbackFile(t *testing.T) {
	dir := t.TempDir()

	if _, err := Write(dir, itID, FallbackName, []byte("real")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Each of these resolves through Path to the fallback file and must
	// leave it alone.
	for _, ref := range []string{"", "   ", ".", "..", "/"} {
		Remove(dir, itID, ref)
		if _, err := Stat(dir, itID, FallbackName); err != nil {
			t.Fatalf("Remove(%q) deleted the fallback file: %v", ref, err)
		}
	}

	// Naming it outright still works.
	Remove(dir, itID, FallbackName)
	if _, err := Stat(dir, itID, FallbackName); !errors.Is(
		err, fs.ErrNotExist) {
		t.Errorf("Remove(%q) = %v, want the file gone",
			FallbackName, err)
	}
}

// The directory removal in Remove is unconditional and only succeeds once
// the last file is gone, so a record holding two files loses its directory
// exactly when the second one goes.
func TestIntegrationRecordDirectoryGoesWithTheLastFile(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"a.pdf", "b.pdf"} {
		if _, err := Write(dir, itID, n, []byte(n)); err != nil {
			t.Fatalf("Write %s: %v", n, err)
		}
	}

	Remove(dir, itID, "a.pdf")
	if _, err := os.Stat(Dir(dir, itID)); err != nil {
		t.Fatalf("the directory went with the first file: %v", err)
	}

	Remove(dir, itID, "b.pdf")
	if _, err := os.Stat(Dir(dir, itID)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("directory after the last file: %v, want it gone", err)
	}
}

// A slow filesystem would make the timing test above meaningless if it
// never actually overlapped, so this reports the observed rate once. It
// asserts nothing; it exists so a run that proved little says so.
func TestIntegrationReportWriteThroughput(t *testing.T) {
	dir := t.TempDir()
	data := bytes.Repeat([]byte("x"), 1<<20)

	start := time.Now()
	const n = 20
	for i := 0; i < n; i++ {
		if _, err := Write(dir, itID, "probe.pdf", data); err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
	}
	elapsed := time.Since(start)
	t.Logf("%d MiB writes in %s (%s each) — the atomicity test above "+
		"overlaps a reader with writes of this cost",
		n, elapsed.Round(time.Millisecond),
		(elapsed / n).Round(time.Microsecond))
}
