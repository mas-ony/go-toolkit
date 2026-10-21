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

	"github.com/mas-ony/go-toolkit/internal/testutil"
)

// firstError is a one-shot error slot: the first failure wins and the
// reader goroutine stops, so a genuine partial read is reported once
// rather than thousands of times.
type firstError struct {
	mu  sync.Mutex
	msg string
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

// set records s unless a message is already held.
func (a *firstError) set(s string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.msg == "" {
		a.msg = s
	}
}

// get returns the first message recorded, or "" when there was none.
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

	lower := mustWrite(t, dir, testID, "scan.pdf", []byte("lower"))
	upper := mustWrite(t, dir, testID, "SCAN.PDF", []byte("upper"))
	if lower == upper {
		t.Fatalf("both spellings stored as %q; SafeName lowercased", lower)
	}

	entries := listDir(t, Dir(dir, testID))

	if testutil.CaseSensitiveDir(t, Dir(dir, testID)) {
		if len(entries) != 2 {
			t.Errorf("%d files, want 2 on a case-sensitive filesystem",
				len(entries))
		}
		got, err := Read(dir, testID, "scan.pdf")
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
	got, err := Read(dir, testID, "scan.pdf")
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
	mustWrite(t, dir, testID, "doc.pdf", old)

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
			got, err := Read(dir, testID, "doc.pdf")
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
		if _, err := Write(dir, testID, "doc.pdf", payload); err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
	}
	close(stop)
	wg.Wait()

	if msg := bad.get(); msg != "" {
		t.Error(msg)
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
	testutil.RequireUnprivileged(t)

	root := t.TempDir()
	recordDir := Dir(root, testID)
	if err := os.MkdirAll(recordDir, 0o755); err != nil {
		t.Fatal(err)
	}
	testutil.Chmod(t, recordDir, 0o555)

	if _, err := Write(root, testID, "scan.pdf", []byte("x")); err != nil {
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
	testutil.RequireUnprivileged(t)

	parent := t.TempDir()
	root := filepath.Join(parent, "uploads")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	testutil.Chmod(t, root, 0o555)

	_, err := Write(root, testID, "scan.pdf", []byte("x"))
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

	name := mustWrite(t, dir, testID, "big.pdf", data)

	info, err := Stat(dir, testID, name)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size() != int64(len(data)) {
		t.Fatalf("stored %d bytes, want %d", info.Size(), len(data))
	}

	got, err := Read(dir, testID, name)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Error("the bytes read back differ from the bytes written")
	}

	// No temp file survived the rename.
	if entries := listDir(t, Dir(dir, testID)); len(entries) != 1 {
		t.Errorf("%d entries, want 1 — a temp file survived", len(entries))
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
		if _, err := Write(dir, testID, "probe.pdf", data); err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
	}
	elapsed := time.Since(start)
	t.Logf("%d MiB writes in %s (%s each) — the atomicity test above "+
		"overlaps a reader with writes of this cost",
		n, elapsed.Round(time.Millisecond),
		(elapsed / n).Round(time.Microsecond))
}
