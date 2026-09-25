package fileutil

import (
	"errors"
	"io/fs"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
)

const testID = 1234

// Magic-byte fixtures for the content-type tests. Real signatures rather than
// invented ones, because the whole point of DetectContentType is that it reads
// what the file actually is — a fixture that only http.DetectContentType's
// table would recognise proves nothing about a scanner's output.
var (
	pdfBytes  = []byte("%PDF-1.7\n1 0 obj\n<< /Type /Catalog >>\nendobj\n")
	pngBytes  = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01")
	jpegBytes = []byte("\xff\xd8\xff\xe0\x00\x10JFIF\x00\x01\x01\x00")
	gifBytes  = []byte("GIF89a\x01\x00\x01\x00\x80\x00\x00")
	bmpBytes  = []byte("BM\x36\x00\x00\x00\x00\x00\x00\x00\x36\x00\x00\x00")
	webpBytes = []byte("RIFF\x24\x00\x00\x00WEBPVP8 \x18\x00\x00\x00")

	// Little-endian TIFF. Go's sniffer has no TIFF signature, which is the
	// entire reason the extension table is consulted as a fallback.
	tiffBytes = []byte("II*\x00\x08\x00\x00\x00\x0e\x00\x00\x01\x03\x00")

	// A ZIP local file header, which is how every OOXML and ODF document
	// begins and therefore how every one of them sniffs.
	zipBytes = []byte("PK\x03\x04\x14\x00\x06\x00\x08\x00\x00\x00!\x00")

	// A file whose NAME says PDF and whose BYTES say HTML — the stored-XSS
	// vector DetectContentType's allowlist exists to close.
	htmlBytes = []byte("<html><script>alert(document.cookie)</script></html>")
)

// ----------------------------------------------------------------------------
// Helpers
// ----------------------------------------------------------------------------

func listDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func assertNoTempFiles(t *testing.T, dir string) {
	t.Helper()
	for _, name := range listDir(t, dir) {
		if strings.HasPrefix(name, ".tmp-") {
			t.Errorf("temporary file %q was left behind in %q", name, dir)
		}
	}
}

// restoreExts snapshots the registration tables and puts them back when the
// test ends, so a test that widens the accepted set does not change what every
// later test is testing.
//
// The caller must not be parallel. The tables are package state, and a
// parallel test mutating them would race the readers rather than merely
// confuse them.
func restoreExts(t *testing.T) {
	t.Helper()

	extMu.Lock()
	exts := maps.Clone(extTypes)
	allowed := maps.Clone(allowedTypes)
	containers := maps.Clone(containerSniffs)
	extMu.Unlock()

	t.Cleanup(func() {
		extMu.Lock()
		defer extMu.Unlock()
		extTypes = exts
		allowedTypes = allowed
		containerSniffs = containers
	})
}

// restoreLimits does the same for the two configurable limits.
func restoreLimits(t *testing.T) {
	t.Helper()

	name, size := MaxNameLen, MaxFileBytes
	t.Cleanup(func() {
		MaxNameLen, MaxFileBytes = name, size
	})
}

// setUmask installs a new process umask and returns the previous one.
//
// It exists so the permission tests can prove that Write's explicit os.Chmod
// defeats a hostile umask, which is the failure the directory Chmod is there
// for: os.MkdirAll applies the umask to every level it creates, os.Chmod does
// not, and under a systemd unit carrying UMask=0077 the difference is a record
// directory the account serving HTTP cannot traverse.
//
// syscall.Umask has no Windows implementation, so this file — and with it
// the package's whole test binary — builds on Unix only. That is
// deliberate: a build tag plus a stub would be two files of ceremony for a
// platform the permission behaviour does not apply to anyway.
func setUmask(mask int) int { return syscall.Umask(mask) }

// ----------------------------------------------------------------------------
// Name sanitisation
// ----------------------------------------------------------------------------

// TestSafeName is the security-relevant table. Each case is a name that can
// reach this function from a multipart upload or a spreadsheet cell, and the
// expectation is the single path element it must collapse to.
func TestSafeName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"ordinary name is unchanged", "scan.pdf", "scan.pdf"},
		{"relative traversal", "../../etc/passwd", "passwd"},
		{"absolute path", "/etc/passwd", "passwd"},
		{"nested path", "a/b/c/scan.pdf", "scan.pdf"},
		{"trailing separator", "dir/", "dir"},
		{"empty input", "", FallbackName},
		{"whitespace only", "   ", FallbackName},
		{"single dot", ".", FallbackName},
		{"double dot", "..", FallbackName},
		{"double dot with path", "foo/..", FallbackName},
		{"separator only", "/", FallbackName},
		{"repeated separators", "///", FallbackName},
		{"surrounding whitespace is trimmed", "  scan.pdf  ", "scan.pdf"},
		{"unicode survives", "dokumen-Ω.pdf", "dokumen-Ω.pdf"},
		{"spaces inside the name survive", "scan 001.pdf", "scan 001.pdf"},

		// Control characters. filepath.Base does not touch these — they
		// contain no separator — so they would otherwise reach a
		// Content-Disposition header intact.
		{"CRLF is dropped", "a\r\nb.pdf", "ab.pdf"},
		{"header injection", "a\r\nX-Injected: 1.pdf", "aX-Injected: 1.pdf"},
		{"NUL is dropped", "scan\x00.pdf", "scan.pdf"},
		{"tab is dropped", "scan\t.pdf", "scan.pdf"},
		{"DEL is dropped", "scan\x7f.pdf", "scan.pdf"},
		{"only control characters", "\r\n\t", FallbackName},

		// The three cases that pin the ORDER of the steps rather than the
		// steps themselves. Each one leaves whitespace on the result under
		// some other ordering, and the stored name then stops matching the
		// one Path recomputes. See the order note on SafeName.
		{"control byte hides a leading space", "\x00 scan.pdf", "scan.pdf"},
		{"base uncovers a leading space", "dir/ scan.pdf", "scan.pdf"},
		{"trailing space after the base", "dir/scan.pdf ", "scan.pdf"},
		{"base uncovers only space", "dir/  ", FallbackName},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := SafeName(tc.input); got != tc.want {
				t.Errorf(
					"SafeName(%q): got %q, want %q",
					tc.input,
					got,
					tc.want)
			}
		})
	}
}

// TestSafeNameInvariants states the four properties every caller relies on,
// checked over the same hostile corpus. A table of specific expectations can
// be updated case by case; these cannot be satisfied by a regression.
func TestSafeNameInvariants(t *testing.T) {
	t.Parallel()

	inputs := []string{
		"scan.pdf", "../../etc/passwd", "/etc/passwd", "", "..", ".", "/",
		"///", "   ", "a\r\nb.pdf", "\x00", "foo/../bar.pdf", "dir/",
		`..\..\windows\system32`, "scan\x7f.pdf", "\r\n\t", "Ω.pdf",
		"\x00 scan.pdf", "dir/ scan.pdf", "dir/scan.pdf ", "a/  ", "  /  ",
	}

	for _, in := range inputs {
		t.Run(strings.ReplaceAll(in, "\x00", "<NUL>"), func(t *testing.T) {
			t.Parallel()
			got := SafeName(in)

			if got == "" || got == "." || got == ".." {
				t.Errorf(
					"SafeName(%q) = %q, which is not usable as a filename",
					in, got)
			}
			if strings.ContainsRune(got, filepath.Separator) ||
				strings.ContainsRune(got, '/') {
				t.Errorf(
					"SafeName(%q) = %q, which still holds a path separator",
					in, got)
			}
			if got != strings.TrimSpace(got) {
				t.Errorf(
					"SafeName(%q) = %q, which carries edge whitespace; Path "+
						"trims it on the way back and stops matching",
					in, got)
			}
			// Idempotency is not cosmetic: Path applies SafeName to a name
			// that Write already passed through it, so the two calls must
			// agree or a stored file becomes unreachable under the name in the
			// database.
			if again := SafeName(got); again != got {
				t.Errorf(
					"not idempotent: SafeName(%q) = %q, SafeName(%q) = %q",
					in, got, got, again)
			}
		})
	}
}

// ----------------------------------------------------------------------------
// Path construction
// ----------------------------------------------------------------------------

func TestDir(t *testing.T) {
	t.Parallel()

	got := Dir("/srv/uploads", 42)
	if want := filepath.Join("/srv/uploads", "42"); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	// Dir must not create anything — callers depend on being able to compute a
	// path for a record that has no file.
	tmp := t.TempDir()
	d := Dir(tmp, 7)
	if _, err := os.Stat(d); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Dir created or found %q; it must be a pure computation", d)
	}
}

// TestPathContainsHostileNames is the containment property, and the reason
// Path re-applies SafeName rather than trusting the database.
//
// A row can carry a name written by a different tool, or one stored before the
// current sanitisation. Whatever it holds, the resolved path must sit directly
// inside that record's own directory — never a sibling, never the parent,
// never outside uploadDir at all.
func TestPathContainsHostileNames(t *testing.T) {
	t.Parallel()

	uploadDir := "/srv/uploads"
	recordDir := Dir(uploadDir, testID)

	hostile := []string{
		"../../etc/passwd",
		"/etc/passwd",
		"..",
		"../9999/scan.pdf",
		"",
		"/",
		"a/b/c.pdf",
		"dir/ scan.pdf",
	}

	for _, supplied := range hostile {
		t.Run(supplied, func(t *testing.T) {
			t.Parallel()

			p := Path(uploadDir, testID, supplied)
			if got := filepath.Dir(p); got != recordDir {
				t.Errorf(
					"Path(%q) resolved into %q, want %q",
					supplied, got, recordDir)
			}
			if p == recordDir {
				t.Errorf("Path(%q) resolved to the directory itself", supplied)
			}
			prefix := recordDir + string(filepath.Separator)
			if !strings.HasPrefix(filepath.Clean(p), prefix) {
				t.Errorf("Path(%q) = %q escaped %q", supplied, p, recordDir)
			}
		})
	}
}

// TestPathFindsWhatWriteStored is the end-to-end form of SafeName's
// idempotency, and the failure it guards is silent in the worst way: the
// upload succeeds, the row is written, and the download 404s forever after.
//
// Write returns the name the caller persists; Read, Stat and Remove all reach
// the file by handing that name back to Path, which sanitises it a SECOND
// time. Any name where the two passes disagree is stored once and never found
// again.
func TestPathFindsWhatWriteStored(t *testing.T) {
	t.Parallel()

	names := []string{
		"scan.pdf",
		"  scan.pdf  ",
		"\x00 scan.pdf",
		"dir/ scan.pdf",
		"dir/scan.pdf ",
		"../../etc/passwd",
		"a\r\nb.pdf",
		"",
	}

	for _, supplied := range names {
		t.Run(strings.ReplaceAll(supplied, "\x00", "<NUL>"), func(t *testing.T) {
			t.Parallel()

			uploadDir := t.TempDir()
			stored, err := Write(uploadDir, testID, supplied, []byte("x"))
			if err != nil {
				t.Fatalf("Write(%q): %v", supplied, err)
			}
			if _, err := Stat(uploadDir, testID, stored); err != nil {
				t.Fatalf(
					"Write(%q) returned %q, which Path cannot resolve: %v",
					supplied, stored, err)
			}
			if _, err := Read(uploadDir, testID, stored); err != nil {
				t.Errorf("Read(%q): %v", stored, err)
			}
		})
	}
}

// ----------------------------------------------------------------------------
// Write / Read / Stat
// ----------------------------------------------------------------------------

func TestWriteReadStatRoundTrip(t *testing.T) {
	t.Parallel()

	uploadDir := t.TempDir()
	data := []byte("file contents")

	name, err := Write(uploadDir, testID, "scan.pdf", data)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if name != "scan.pdf" {
		t.Errorf("returned name: got %q, want %q", name, "scan.pdf")
	}

	got, err := Read(uploadDir, testID, name)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(got) != string(data) {
		t.Errorf("Read: got %q, want %q", got, data)
	}

	info, err := Stat(uploadDir, testID, name)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size() != int64(len(data)) {
		t.Errorf("Stat size: got %d, want %d", info.Size(), len(data))
	}
}

// TestWriteCreatesDirectoryOnDemand covers the promise that neither uploadDir
// nor the per-record directory has to be provisioned first. The upload
// directory is deliberately several levels deep and absent.
func TestWriteCreatesDirectoryOnDemand(t *testing.T) {
	t.Parallel()

	uploadDir := filepath.Join(t.TempDir(), "not", "yet", "created")

	_, err := Write(uploadDir, testID, "scan.pdf", []byte("x"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	info, err := os.Stat(Dir(uploadDir, testID))
	if err != nil {
		t.Fatalf("record directory was not created: %v", err)
	}
	if !info.IsDir() {
		t.Error("record path exists but is not a directory")
	}
}

// TestWriteReturnsTheSanitisedName checks that the value handed back for the
// database is the name actually on disk — never the caller's raw input.
//
// Storing the raw name would put a path into the name column, and every later
// Read, Stat and Remove would sanitise it back to something else and miss the
// file.
func TestWriteReturnsTheSanitisedName(t *testing.T) {
	t.Parallel()

	uploadDir := t.TempDir()

	name, err := Write(uploadDir, testID, "../../etc/passwd", []byte("x"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if name != "passwd" {
		t.Fatalf("returned name: got %q, want %q", name, "passwd")
	}

	// The file must be inside the record directory, and nothing may have been
	// written to the parent.
	inside := filepath.Join(Dir(uploadDir, testID), "passwd")
	if _, err := os.Stat(inside); err != nil {
		t.Errorf("file not found inside the record directory: %v", err)
	}
	outside := filepath.Join(uploadDir, "passwd")
	if _, err := os.Stat(outside); !errors.Is(err, fs.ErrNotExist) {
		t.Error("a file escaped into the upload directory root")
	}
}

// TestWriteOverwritesTheSameName pins the documented replace-in-place
// behaviour: a record holding one file must not accumulate copies when that
// file is re-uploaded under the same name.
func TestWriteOverwritesTheSameName(t *testing.T) {
	t.Parallel()

	uploadDir := t.TempDir()

	_, err := Write(uploadDir, testID, "scan.pdf", []byte("first"))
	if err != nil {
		t.Fatalf("first Write: %v", err)
	}
	_, err = Write(uploadDir, testID, "scan.pdf", []byte("second"))
	if err != nil {
		t.Fatalf("second Write: %v", err)
	}

	got, err := Read(uploadDir, testID, "scan.pdf")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(got) != "second" {
		t.Errorf("contents: got %q, want %q", got, "second")
	}
	if entries := listDir(t, Dir(uploadDir, testID)); len(entries) != 1 {
		t.Errorf("directory holds %v, want exactly one file", entries)
	}
}

// TestWriteLeavesNoTempFileOnSuccess checks that the rename consumed the
// temporary and the deferred Remove was the intended no-op.
//
// A stray .tmp-* file is not merely untidy: the directory sweep the package
// doc describes compares entries against the stored names, so leftovers look
// like orphans forever and are never safe to delete automatically.
func TestWriteLeavesNoTempFileOnSuccess(t *testing.T) {
	t.Parallel()

	uploadDir := t.TempDir()
	_, err := Write(uploadDir, testID, "scan.pdf", []byte("x"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	assertNoTempFiles(t, Dir(uploadDir, testID))
}

// TestWriteCleansUpTempFileOnFailure exercises the deferred Remove on the path
// it actually exists for.
//
// The failure is induced at the rename by pre-creating the destination NAME as
// a directory, which is the last possible failure point and the one that
// leaves a fully written temporary behind. Without the defer, every such
// failure leaves a complete copy of the file under a name nothing references.
func TestWriteCleansUpTempFileOnFailure(t *testing.T) {
	t.Parallel()

	uploadDir := t.TempDir()
	dir := Dir(uploadDir, testID)
	if err := os.MkdirAll(filepath.Join(dir, "scan.pdf"), 0o755); err != nil {
		t.Fatalf("setting up the blocking directory: %v", err)
	}

	name, err := Write(uploadDir, testID, "scan.pdf", []byte("x"))
	if err == nil {
		t.Fatalf(
			"Write succeeded (returned %q); the rename onto a directory must "+
				"fail",
			name)
	}
	if name != "" {
		t.Errorf("returned name on failure: got %q, want empty", name)
	}
	assertNoTempFiles(t, dir)
}

// TestWriteFailsWhenTheUploadDirIsNotADirectory covers the mkdirAll branch —
// the misconfiguration where upload_dir points at a regular file.
//
// The assertion is that Write reports it rather than proceeding, since the
// caller writes the database row on success and would otherwise record a file
// that was never stored.
func TestWriteFailsWhenTheUploadDirIsNotADirectory(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	uploadDir := filepath.Join(base, "uploads")
	if err := os.WriteFile(uploadDir, []byte("not a dir"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	name, err := Write(uploadDir, testID, "scan.pdf", []byte("x"))
	if err == nil {
		t.Fatal("Write succeeded with a regular file as the upload directory")
	}
	if name != "" {
		t.Errorf("returned name on failure: got %q, want empty", name)
	}
	// The wrapped error names the directory, which is what makes a
	// misconfigured upload_dir diagnosable from one log line.
	if !strings.Contains(err.Error(), uploadDir) {
		t.Errorf(
			"error %q does not mention the offending directory %q",
			err,
			uploadDir)
	}
}

// TestWriteEmptyData records that a zero-byte file is stored rather than
// rejected. This package takes no view on whether an empty upload is
// meaningful — the handler decides that — and silently succeeding here
// without creating the file would be the harmful outcome.
func TestWriteEmptyData(t *testing.T) {
	t.Parallel()

	uploadDir := t.TempDir()
	name, err := Write(uploadDir, testID, "scan.pdf", nil)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	info, err := Stat(uploadDir, testID, name)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size() != 0 {
		t.Errorf("size: got %d, want 0", info.Size())
	}
}

// ----------------------------------------------------------------------------
// Permissions
// ----------------------------------------------------------------------------

// TestWritePermissions checks the two modes in the const block, and in
// particular that the explicit Chmod on the directory survives the process
// umask.
//
// os.MkdirAll applies the umask; os.Chmod does not. Under a hardened unit
// (systemd UMask=0077) the directory would be 0700 without that call, and
// the account serving HTTP could not traverse into a file it is otherwise
// allowed to read. The test sets a hostile umask so the assertion means
// something, which is also why it reads the POSIX mode bits without
// qualification: this file builds on Unix only, for the reason given at
// setUmask.
func TestWritePermissions(t *testing.T) {
	// Not parallel: umask is process-wide state.
	//
	// Deliberately NOT skipped when running as root. Root bypasses permission
	// CHECKS, but the umask still masks the mode bits a newly created
	// directory is given, so the assertion below is just as meaningful in a
	// container running as uid 0 — which is how CI runs it.
	old := setUmask(0o077)
	defer setUmask(old)

	uploadDir := t.TempDir()
	name, err := Write(uploadDir, testID, "scan.pdf", []byte("x"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	dirInfo, err := os.Stat(Dir(uploadDir, testID))
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != dirPerm {
		t.Errorf(
			"directory mode: got %04o, want %04o — the explicit Chmod must "+
				"defeat the umask",
			got,
			dirPerm)
	}

	fileInfo, err := Stat(uploadDir, testID, name)
	if err != nil {
		t.Fatalf("stat file: %v", err)
	}
	if got := fileInfo.Mode().Perm(); got != filePerm {
		t.Errorf(
			"file mode: got %04o, want %04o — CreateTemp's 0600 must be "+
				"widened before publishing",
			got,
			filePerm)
	}
}

// TestWritePermissionsOnCreatedParents is the half the test above cannot see.
//
// t.TempDir() pre-creates its directory, so only the leaf is ever created
// under the hostile umask there, and chmod'ing the leaf alone is enough to
// pass. With an upload root that does not exist yet — a relative "./uploads"
// on first boot, or a volume mounted a level up — mkdirAll creates the whole
// chain and the umask reaches every level of it:
//
//	0700  <uploadDir>          <- the level a leaf-only chmod misses
//	0755  <uploadDir>/<id>
//	0644  <uploadDir>/<id>/<supplied>
//
// A traversal blocked at the top is indistinguishable, from the HTTP process's
// side, from one blocked at the bottom: the file is unreadable either way.
// Every level this call created must therefore be checked, not just the last.
func TestWritePermissionsOnCreatedParents(t *testing.T) {
	// Not parallel: umask is process-wide state.
	old := setUmask(0o077)
	defer setUmask(old)

	base := t.TempDir()
	uploadDir := filepath.Join(base, "not", "yet", "created")

	name, err := Write(uploadDir, testID, "scan.pdf", []byte("x"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	created := []string{
		filepath.Join(base, "not"),
		filepath.Join(base, "not", "yet"),
		uploadDir,
		Dir(uploadDir, testID),
	}
	for _, dir := range created {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("stat %q: %v", dir, err)
		}
		if got := info.Mode().Perm(); got != dirPerm {
			t.Errorf(
				"%q: mode %04o, want %04o — every directory this "+
					"call created must be traversable",
				dir, got, dirPerm)
		}
	}

	fileInfo, err := Stat(uploadDir, testID, name)
	if err != nil {
		t.Fatalf("stat file: %v", err)
	}
	if got := fileInfo.Mode().Perm(); got != filePerm {
		t.Errorf("file mode: got %04o, want %04o", got, filePerm)
	}
}

// TestWriteLeavesExistingDirectoryModesAlone is the counterweight to the test
// above, and the reason mkdirAll only chmods what it creates.
//
// <uploadDir> is normally a volume the operator provisioned. Walking up and
// re-permissioning it would be this package deciding it knows better than
// the deployment about a directory it did not create — a real risk once the
// root is shared with anything else. Only the per-record directory below it
// is this package's to widen.
func TestWriteLeavesExistingDirectoryModesAlone(t *testing.T) {
	t.Parallel()

	uploadDir := t.TempDir()
	const deliberate os.FileMode = 0o750
	if err := os.Chmod(uploadDir, deliberate); err != nil {
		t.Fatalf("setup: %v", err)
	}

	_, err := Write(uploadDir, testID, "scan.pdf", []byte("x"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	info, err := os.Stat(uploadDir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != deliberate {
		t.Errorf(
			"upload root mode: got %04o, want %04o — an existing directory "+
				"is the operator's, not ours", got, deliberate)
	}
}

// ----------------------------------------------------------------------------
// Reading
// ----------------------------------------------------------------------------

// TestReadMissingIsUnwrappedNotExist backs the doc comment's promise that
// callers can use errors.Is to tell "the row names a file that is not there"
// apart from a genuine I/O failure. Wrapping the error with %w would still
// satisfy errors.Is; returning a fmt.Errorf with %v would not.
func TestReadMissingIsUnwrappedNotExist(t *testing.T) {
	t.Parallel()

	uploadDir := t.TempDir()

	_, err := Read(uploadDir, testID, "absent.pdf")
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Read: got %v, want errors.Is(err, fs.ErrNotExist)", err)
	}
	_, err = Stat(uploadDir, testID, "absent.pdf")
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Stat: got %v, want errors.Is(err, fs.ErrNotExist)", err)
	}
}

// TestStatDoesNotReadTheFile is the reason Stat exists next to Read.
//
// There is no way to observe "did not read" directly, so the assertion is the
// observable proxy: Stat answers correctly for a file far larger than anything
// worth pulling into memory per request, and reports its size from metadata.
func TestStatDoesNotReadTheFile(t *testing.T) {
	t.Parallel()

	uploadDir := t.TempDir()
	dir := Dir(uploadDir, testID)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		t.Fatalf("setup: %v", err)
	}

	// A sparse file: 64 MiB of length, a handful of blocks of storage.
	f, err := os.Create(filepath.Join(dir, "big.pdf"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	const size = 64 << 20
	if err := f.Truncate(size); err != nil {
		f.Close()
		t.Fatalf("truncate: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	info, err := Stat(uploadDir, testID, "big.pdf")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size() != size {
		t.Errorf("size: got %d, want %d", info.Size(), size)
	}
}

// ----------------------------------------------------------------------------
// CopyUnique
// ----------------------------------------------------------------------------

// TestCopyUniqueSuffixesBeforeTheExtension pins the naming rule, which is not
// cosmetic: an in-browser editor keys its format off the extension, so a
// suffix appended AFTER it produces a file the editor refuses to open.
func TestCopyUniqueSuffixesBeforeTheExtension(t *testing.T) {
	t.Parallel()

	uploadDir := t.TempDir()
	_, err := Write(uploadDir, testID, "letter.docx", []byte("v1"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	want := []string{"letter (2).docx", "letter (3).docx", "letter (4).docx"}
	for _, expected := range want {
		name, n, err := CopyUnique(
			uploadDir, testID, "letter.docx", "letter.docx")
		if err != nil {
			t.Fatalf("CopyUnique: %v", err)
		}
		if name != expected {
			t.Fatalf("name: got %q, want %q", name, expected)
		}
		if n != 2 {
			t.Errorf("bytes copied: got %d, want 2", n)
		}
		if got, err := Read(uploadDir, testID, name); err != nil {
			t.Errorf("Read(%q): %v", name, err)
		} else if string(got) != "v1" {
			t.Errorf("contents of %q: got %q, want %q", name, got, "v1")
		}
	}

	// The source is untouched. This is the difference from Write, and the
	// reason CopyUnique exists rather than a second call to it.
	if got, err := Read(uploadDir, testID, "letter.docx"); err != nil {
		t.Errorf("the source file is gone: %v", err)
	} else if string(got) != "v1" {
		t.Errorf("the source was overwritten: got %q", got)
	}
}

// TestCopyUniqueTakesTheDesiredNameWhenFree checks the ordinary case, where no
// suffix is inserted at all.
func TestCopyUniqueTakesTheDesiredNameWhenFree(t *testing.T) {
	t.Parallel()

	uploadDir := t.TempDir()
	_, err := Write(uploadDir, testID, "letter.docx", []byte("v1"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	name, _, err := CopyUnique(uploadDir, testID, "letter.docx", "backup.docx")
	if err != nil {
		t.Fatalf("CopyUnique: %v", err)
	}
	if name != "backup.docx" {
		t.Errorf("name: got %q, want %q", name, "backup.docx")
	}
}

// TestCopyUniqueReturnsTheNameOnDisk is the invariant the caller's file row
// rests on, and the suffix loop is where it can break without anything failing
// loudly.
//
// A desired name that is all extension has an empty base — filepath.Ext of
// ".pdf" is the whole string — so the second candidate reads " (2).pdf", with
// a leading space that Write's own SafeName trims away. A loop that reserves
// the untrimmed spelling stores the bytes under "(2).pdf", strands the
// reservation as a zero-byte file no later copy will ever claim, and returns a
// name that is not the file. Path re-sanitises on the way back, so reads still
// work and nothing complains; only the directory fills up and the stored name
// stops being the truth.
func TestCopyUniqueReturnsTheNameOnDisk(t *testing.T) {
	t.Parallel()

	uploadDir := t.TempDir()
	if _, err := Write(uploadDir, testID, ".pdf", []byte("v1")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	name, n, err := CopyUnique(uploadDir, testID, ".pdf", ".pdf")
	if err != nil {
		t.Fatalf("CopyUnique: %v", err)
	}
	if again := SafeName(name); again != name {
		t.Errorf(
			"CopyUnique returned %q, which SafeName rewrites to %q; the "+
				"persisted name and the stored one disagree",
			name,
			again)
	}
	info, err := Stat(uploadDir, testID, name)
	if err != nil {
		t.Fatalf("Stat(%q): %v", name, err)
	}
	if info.Size() != int64(n) {
		t.Errorf(
			"%q holds %d bytes, CopyUnique reported %d",
			name,
			info.Size(),
			n)
	}
	// The source and the copy, and nothing else. A third entry is an orphaned
	// reservation.
	if got := listDir(t, Dir(uploadDir, testID)); len(got) != 2 {
		t.Errorf("directory holds %v, want the source and one copy", got)
	}
}

// TestCopyUniqueReportsAMissingSource covers the one failure that must not be
// silent: the caller is about to write a file row, and a copy that quietly
// produced nothing would leave that row pointing at an absent file.
func TestCopyUniqueReportsAMissingSource(t *testing.T) {
	t.Parallel()

	uploadDir := t.TempDir()
	name, n, err := CopyUnique(uploadDir, testID, "absent.docx", "copy.docx")
	if err == nil {
		t.Fatal("CopyUnique succeeded with no source file")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("got %v, want an error satisfying fs.ErrNotExist", err)
	}
	if name != "" || n != 0 {
		t.Errorf("got %q and %d bytes on failure, want empty and 0", name, n)
	}
	// Nothing may be left behind for a copy that never happened.
	if _, err := os.Stat(Dir(uploadDir, testID)); err == nil {
		if entries := listDir(t, Dir(uploadDir, testID)); len(entries) != 0 {
			t.Errorf("a failed copy left %v behind", entries)
		}
	}
}

// TestCopyUniqueIsAtomicUnderConcurrency is the reason the loop reserves each
// candidate with O_CREATE|O_EXCL rather than asking os.Stat whether it is
// free.
//
// Stat-then-write has a window: two copies under the same record can both see
// the same name as available, and the loser's bytes land on top of the
// winner's. The failure needs two things to show up — concurrency and the same
// desired name — so it survives every sequential test above.
func TestCopyUniqueIsAtomicUnderConcurrency(t *testing.T) {
	t.Parallel()

	uploadDir := t.TempDir()
	_, err := Write(uploadDir, testID, "letter.docx", []byte("v1"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	const copies = 8
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		names = make(map[string]int, copies)
		errs  []error
	)
	for range copies {
		wg.Add(1)
		go func() {
			defer wg.Done()
			name, _, err := CopyUnique(
				uploadDir, testID, "letter.docx", "letter.docx")
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			names[name]++
		}()
	}
	wg.Wait()

	for _, err := range errs {
		t.Errorf("CopyUnique: %v", err)
	}
	for name, n := range names {
		if n > 1 {
			t.Errorf("%d copies claimed the name %q", n, name)
		}
	}
	// One source plus one file per copy. A lost reservation shows up here as
	// a missing file rather than as a duplicate name.
	if got := len(listDir(t, Dir(uploadDir, testID))); got != copies+1 {
		t.Errorf("directory holds %d files, want %d", got, copies+1)
	}
}

// TestCopyUniqueRefusesASuffixOverTheNameCap covers the one place in this
// package that MANUFACTURES a filename rather than sanitising one it was
// given, and therefore the one place a legal name can be turned into an
// illegal one from the inside.
//
// " (2)" is four more bytes, so a desired name sitting on MaxNameLen crosses
// it on the second copy. CopyUnique's contract is that the caller persists
// the returned name onto a file row, so an unchecked overflow arrives as a
// driver truncation error AFTER the bytes are on disk — the exact outcome
// MaxNameLen exists to prevent.
//
// The first copy must still succeed. The check is on the CANDIDATE, not on
// the desired name, so a name that fits is stored under the name that fits.
//
// Not parallel: it moves MaxNameLen, which is package state. See the note
// above the registration tests for why a serial test that mutates and
// restores cannot overlap with a parallel reader.
func TestCopyUniqueRefusesASuffixOverTheNameCap(t *testing.T) {
	restoreLimits(t)

	MaxNameLen = 20

	uploadDir := t.TempDir()
	// Exactly the cap: 16 bytes of stem plus ".pdf".
	supplied := strings.Repeat("a", MaxNameLen-4) + ".pdf"
	if NameTooLong(supplied) {
		t.Fatalf("fixture is wrong: %q is already over the cap", supplied)
	}
	if _, err := Write(uploadDir, testID, supplied, []byte("v1")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// The desired name is free, so no suffix is inserted and the copy fits.
	first, _, err := CopyUnique(uploadDir, testID, supplied, "backup.pdf")
	if err != nil {
		t.Fatalf("CopyUnique under the cap: %v", err)
	}
	if NameTooLong(first) {
		t.Errorf("CopyUnique returned %q, which is over the cap", first)
	}

	// The desired name is taken, so the loop reaches " (2)" and overflows.
	name, n, err := CopyUnique(uploadDir, testID, supplied, supplied)
	if err == nil {
		t.Fatalf(
			"CopyUnique returned %q, which is %d bytes against a %d-byte "+
				"cap; the caller would persist a name the column cannot hold",
			name,
			len(name),
			MaxNameLen)
	}
	if name != "" || n != 0 {
		t.Errorf("got %q and %d bytes on failure, want empty and 0", name, n)
	}
	if !strings.Contains(err.Error(), MaxNameLabel()) {
		t.Errorf("error %q does not name the limit it enforced", err)
	}

	// The refusal must not leave a reservation squatting on the name it
	// declined to use: the source, and one copy from the call that succeeded.
	if got := listDir(t, Dir(uploadDir, testID)); len(got) != 2 {
		t.Errorf("directory holds %v, want the source and one copy", got)
	}
}

// ----------------------------------------------------------------------------
// Removal
// ----------------------------------------------------------------------------

func TestRemove(t *testing.T) {
	t.Parallel()

	t.Run("removes the file and the emptied directory", func(t *testing.T) {
		t.Parallel()

		uploadDir := t.TempDir()
		name, err := Write(uploadDir, testID, "scan.pdf", []byte("x"))
		if err != nil {
			t.Fatalf("Write: %v", err)
		}

		Remove(uploadDir, testID, name)

		_, err = os.Stat(Path(uploadDir, testID, name))
		if !errors.Is(err, fs.ErrNotExist) {
			t.Error("the file is still present")
		}
		_, err = os.Stat(Dir(uploadDir, testID))
		if !errors.Is(err, fs.ErrNotExist) {
			t.Error("the emptied record directory was not cleaned up")
		}
	})

	t.Run("leaves a directory holding other files", func(t *testing.T) {
		t.Parallel()

		uploadDir := t.TempDir()
		_, err := Write(uploadDir, testID, "scan.pdf", []byte("x"))
		if err != nil {
			t.Fatalf("Write: %v", err)
		}
		_, err = Write(uploadDir, testID, "other.pdf", []byte("y"))
		if err != nil {
			t.Fatalf("Write: %v", err)
		}

		Remove(uploadDir, testID, "scan.pdf")

		other := Path(uploadDir, testID, "other.pdf")
		if _, err := os.Stat(other); err != nil {
			t.Errorf("the surviving file was destroyed: %v", err)
		}
		// os.Remove on a non-empty directory fails, and that failure is
		// discarded — which is what makes the unconditional call safe.
		if _, err := os.Stat(Dir(uploadDir, testID)); err != nil {
			t.Errorf("the non-empty directory was removed: %v", err)
		}
	})

	t.Run("is a no-op for a substituted fallback", func(t *testing.T) {
		t.Parallel()

		uploadDir := t.TempDir()

		// The stored file is deliberately the one SafeName produces for an
		// unnamed upload. That is what makes the guard load-bearing rather
		// than decorative: without it, a row with an empty name column
		// resolves to this exact path and deletes a real file belonging to a
		// different upload.
		name, err := Write(uploadDir, testID, "", []byte("x"))
		if err != nil {
			t.Fatalf("Write: %v", err)
		}
		if name != FallbackName {
			t.Fatalf(
				"fixture assumption broken: SafeName(\"\") is %q, want %q",
				name, FallbackName)
		}

		// The blank cases are the realistic ones — a cleared column, a row
		// written before the name was required. The rest reach the same
		// destination through SafeName and cost nothing to cover, which is
		// why the guard tests what SafeName DID rather than whether the input
		// looked empty.
		folded := []string{
			"", "   ", "\t\n", ".", "..", "/", "///", "foo/..", "\r\n\t",
			"\x00", "dir/  ",
		}
		for _, in := range folded {
			Remove(uploadDir, testID, in)

			if _, err := os.Stat(Path(uploadDir, testID, name)); err != nil {
				t.Fatalf(
					"Remove(%q) deleted the fallback-named file: %v", in, err)
			}
			if _, err := os.Stat(Dir(uploadDir, testID)); err != nil {
				t.Fatalf(
					"Remove(%q) deleted the record directory: %v", in, err)
			}
		}
	})

	t.Run("removes a file named for the fallback", func(t *testing.T) {
		t.Parallel()

		// The guard keys on SUBSTITUTION, not on the resulting name, so an
		// upload that really was called FallbackName stays removable. A guard
		// written as `SafeName(supplied) == FallbackName` alone would strand
		// these files on disk forever, and nothing in the caller would report
		// it.
		uploadDir := t.TempDir()
		name, err := Write(uploadDir, testID, FallbackName, []byte("x"))
		if err != nil {
			t.Fatalf("Write: %v", err)
		}

		// Whitespace is trimmed, which is not a substitution.
		Remove(uploadDir, testID, "  "+FallbackName+"  ")

		_, err = os.Stat(Path(uploadDir, testID, name))
		if !errors.Is(err, fs.ErrNotExist) {
			t.Error("a file genuinely named for the fallback was not removed")
		}
	})

	t.Run("is silent when nothing is there", func(t *testing.T) {
		t.Parallel()

		// Remove is called after the database work has already succeeded, so
		// it has no way to report a failure and must not panic on one either.
		Remove(t.TempDir(), testID, "absent.pdf")
		missing := filepath.Join(t.TempDir(), "no", "such", "tree")
		Remove(missing, testID, "absent.pdf")
	})
}

// TestRemoveFiles covers the multi-file form, whose signature is []*string
// because the name field on a file model is a pointer: a nil entry is a row
// with no file attached, and callers pass the fields straight off a model
// slice without filtering.
func TestRemoveFiles(t *testing.T) {
	t.Parallel()

	uploadDir := t.TempDir()
	for _, supplied := range []string{"a.pdf", "b.pdf", "c.pdf"} {
		if _, err := Write(uploadDir, testID, supplied, []byte("x")); err != nil {
			t.Fatalf("Write(%q): %v", supplied, err)
		}
	}

	a, b := "a.pdf", "b.pdf"
	RemoveFiles(uploadDir, testID, []*string{&a, nil, &b, nil})

	for _, supplied := range []string{"a.pdf", "b.pdf"} {
		_, err := os.Stat(Path(uploadDir, testID, supplied))
		if !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%q survived RemoveFiles", supplied)
		}
	}
	if _, err := os.Stat(Path(uploadDir, testID, "c.pdf")); err != nil {
		t.Errorf("an unnamed file was removed: %v", err)
	}
	// The directory still holds c.pdf, so the trailing removal must fail
	// silently rather than take the survivor with it.
	if _, err := os.Stat(Dir(uploadDir, testID)); err != nil {
		t.Errorf("the non-empty directory was removed: %v", err)
	}

	c := "c.pdf"
	RemoveFiles(uploadDir, testID, []*string{&c})
	_, err := os.Stat(Dir(uploadDir, testID))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Error("the emptied record directory was not cleaned up")
	}
}

func TestRemoveDir(t *testing.T) {
	t.Parallel()

	uploadDir := t.TempDir()
	_, err := Write(uploadDir, testID, "scan.pdf", []byte("x"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	_, err = Write(uploadDir, testID, "other.pdf", []byte("y"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	// A stray temporary from an interrupted upload, which is one of the
	// reasons RemoveDir exists rather than a loop over known names.
	orphan := filepath.Join(Dir(uploadDir, testID), ".tmp-orphan")
	if err := os.WriteFile(orphan, []byte("z"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}

	// A sibling record that must survive.
	_, err = Write(uploadDir, testID+1, "keep.pdf", []byte("k"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	RemoveDir(uploadDir, testID)

	_, err = os.Stat(Dir(uploadDir, testID))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Error("the record directory survived RemoveDir")
	}
	if _, err := os.Stat(Path(uploadDir, testID+1, "keep.pdf")); err != nil {
		t.Errorf("a sibling record was destroyed: %v", err)
	}

	// Idempotent: removing an absent tree is not an error.
	RemoveDir(uploadDir, testID)
}

// ----------------------------------------------------------------------------
// Content type
// ----------------------------------------------------------------------------

// TestDetectContentType walks the documented outcomes over the default table.
func TestDetectContentType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		supplied string
		data     []byte
		want     string
		why      string
	}{
		// The sniff is recognised and allowed, so it is trusted.
		{"pdf", "scan.pdf", pdfBytes, "application/pdf", "sniffed, allowed"},
		{"png", "scan.png", pngBytes, "image/png", "sniffed, allowed"},
		{"jpeg", "scan.jpg", jpegBytes, "image/jpeg", "sniffed, allowed"},
		{"gif", "scan.gif", gifBytes, "image/gif", "sniffed, allowed"},
		{"bmp", "scan.bmp", bmpBytes, "image/bmp", "sniffed, allowed"},
		{"webp", "scan.webp", webpBytes, "image/webp", "sniffed, allowed"},

		// Content beats the name, in both directions.
		{
			"png bytes named pdf", "scan.pdf", pngBytes, "image/png",
			"content is authoritative over the extension",
		},
		{
			"pdf bytes named jpg", "scan.jpg", pdfBytes, "application/pdf",
			"content is authoritative over the extension",
		},

		// Recognised but not allowed — refuse to repeat the claim.
		{
			"html bytes named pdf", "scan.pdf", htmlBytes,
			"application/octet-stream",
			"stored-XSS vector: never echo text/html back as a Content-Type",
		},
		{
			"plain text named pdf", "scan.pdf", []byte("just some text"),
			"application/octet-stream",
			"text/plain is not in the default table",
		},
		{
			"zip bytes named pdf", "scan.pdf", zipBytes,
			"application/octet-stream",
			"application/pdf is not a zip-backed format",
		},

		// An unrecognised sniff falls back to the extension table.
		{
			"tiff", "scan.tiff", tiffBytes, "image/tiff",
			"Go has no TIFF signature; only the extension can name it",
		},
		{
			"tif short extension", "scan.tif", tiffBytes, "image/tiff",
			"both spellings are in the table",
		},
		{
			"empty data uses the extension", "scan.pdf", nil,
			"application/pdf", "no bytes to sniff",
		},
		{
			"uppercase extension", "SCAN.PDF", nil, "application/pdf",
			"the extension lookup is case-insensitive",
		},
		{
			"mixed case extension", "scan.JpEg", nil, "image/jpeg",
			"the extension lookup is case-insensitive",
		},

		// Nothing recognised on either side.
		{
			"unknown bytes and extension", "scan.xyz",
			[]byte{0x00, 0x01, 0x02, 0x03}, "application/octet-stream",
			"the honest answer",
		},
		{
			"no extension at all", "scan", []byte{0x00, 0x01, 0x02, 0x03},
			"application/octet-stream", "the honest answer",
		},
		{
			"empty everything", "", nil, "application/octet-stream",
			"the honest answer",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := DetectContentType(tc.supplied, tc.data); got != tc.want {
				t.Errorf(
					"DetectContentType(%q, %d bytes): got %q, want %q (%s)",
					tc.supplied,
					len(tc.data),
					got,
					tc.want,
					tc.why)
			}
		})
	}
}

// TestDetectContentTypeNeverReturnsEmpty backs the promise that the result is
// always usable both as a Content-Type header and as a column value.
func TestDetectContentTypeNeverReturnsEmpty(t *testing.T) {
	t.Parallel()

	corpus := [][]byte{
		nil, {}, pdfBytes, pngBytes, tiffBytes, htmlBytes, zipBytes,
		{0xFF, 0xFE, 0x00},
	}
	names := []string{"", "scan", "scan.pdf", "scan.exe", ".pdf", "scan."}

	for _, supplied := range names {
		for _, data := range corpus {
			if got := DetectContentType(supplied, data); got == "" {
				t.Errorf(
					"DetectContentType(%q, %d bytes) returned an empty string",
					supplied, len(data))
			}
		}
	}
}

// TestEmptyInputSniffsAsTextSoTheGuardIsNeeded pins the premise behind the
// len(data) > 0 check, which reads like a cheap optimisation and is not one.
//
// http.DetectContentType(nil) returns text/plain, not the generic type: its
// text rule is "no disqualifying bytes", and no bytes trivially satisfies it.
// Without the guard, a zero-byte file sniffs as recognised-but-not-allowed and
// collapses to application/octet-stream, never reaching the extension table —
// so "empty data uses the extension" above breaks, and a zero-byte scan.pdf is
// served as a download instead of rendered.
//
// Write stores empty files rather than rejecting them, so this is a reachable
// state and not a hypothetical.
func TestEmptyInputSniffsAsTextSoTheGuardIsNeeded(t *testing.T) {
	t.Parallel()

	if got := http.DetectContentType(nil); got != "text/plain; charset=utf-8" {
		t.Errorf(
			"http.DetectContentType(nil) now reports %q; the len(data) guard "+
				"may no longer be needed — check before removing it",
			got)
	}
	if got := DetectContentType("scan.pdf", nil); got != "application/pdf" {
		t.Errorf("DetectContentType(nil data): got %q, want the ext type", got)
	}
	empty := DetectContentType("scan.pdf", []byte{})
	if empty != "application/pdf" {
		t.Errorf(
			"DetectContentType(no data): got %q, want the ext type", empty)
	}
}

// TestSniffedTiffIsGenericSoTheFallbackIsNeeded corroborates the premise the
// TIFF case rests on.
//
// If Go ever adds a TIFF signature, this test fails and the extension fallback
// for TIFF becomes dead reasoning rather than a live requirement — worth
// knowing, since the fallback is the only branch that trusts a filename.
func TestSniffedTiffIsGenericSoTheFallbackIsNeeded(t *testing.T) {
	t.Parallel()

	const generic = "application/octet-stream"
	if got := http.DetectContentType(tiffBytes); got != generic {
		t.Errorf(
			"http.DetectContentType now reports %q for TIFF; the extension "+
				"fallback's rationale has changed", got)
	}
}

// TestAllowedTypesIsDerivedFromTheExtensionTable proves the "derived rather
// than written out so the two can never drift" claim.
//
// A hand-maintained allowlist that fell behind would silently downgrade a
// newly accepted format to application/octet-stream — a bug that looks like
// a browser problem, not a table problem.
func TestAllowedTypesIsDerivedFromTheExtensionTable(t *testing.T) {
	t.Parallel()

	extMu.RLock()
	defer extMu.RUnlock()

	for ext, ct := range extTypes {
		if _, ok := allowedTypes[ct]; !ok {
			t.Errorf("%q maps to %q, which is not in allowedTypes", ext, ct)
		}
		// No value may carry parameters: DetectContentType compares the
		// sniffed type exactly, and "image/png; charset=..." never matches.
		if strings.ContainsAny(ct, "; ") {
			t.Errorf(
				"%q maps to %q, which carries parameters; the exact "+
					"comparison in DetectContentType would miss it", ext, ct)
		}
	}
	for ct := range allowedTypes {
		if !slices.Contains(slices.Collect(maps.Values(extTypes)), ct) {
			t.Errorf("allowedTypes holds %q, which no extension maps to", ct)
		}
	}
}

func TestHasAllowedExt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		supplied string
		want     bool
	}{
		{"scan.pdf", true},
		{"scan.PDF", true},
		{"scan.jpeg", true},
		{"scan.JPG", true},
		{"scan.tiff", true},
		{"a.b.c.png", true},
		{"scan.exe", false},
		{"scan.zip", false},
		{"scan.xlsx", false},
		{"scan.svg", false},
		{"scan", false},
		{"pdf", false},
		{"scan.", false},
		{"", false},
		{"archive.tar.gz", false},
		{".pdf", true}, // a dotfile named ".pdf" — filepath.Ext says ".pdf"
	}

	for _, tc := range tests {
		t.Run(tc.supplied, func(t *testing.T) {
			t.Parallel()
			if got := HasAllowedExt(tc.supplied); got != tc.want {
				t.Errorf(
					"HasAllowedExt(%q): got %t, want %t",
					tc.supplied,
					got,
					tc.want)
			}
		})
	}
}

// TestAllowedExts checks the three properties error messages and CLI help
// depend on: sorted, complete, and safe to hand out.
func TestAllowedExts(t *testing.T) {
	t.Parallel()

	got := AllowedExts()

	if !slices.IsSorted(got) {
		t.Errorf("got %v, want a sorted slice", got)
	}
	extMu.RLock()
	want := len(extTypes)
	extMu.RUnlock()
	if len(got) != want {
		t.Errorf(
			"got %d entries, want %d — the list must cover the whole table",
			len(got),
			want)
	}
	for _, ext := range got {
		if !HasAllowedExt("scan" + ext) {
			t.Errorf("%q is advertised but HasAllowedExt rejects it", ext)
		}
	}

	// A fresh slice per call: the result goes into error strings and help
	// text, and a shared backing array would let one caller's sort or append
	// corrupt another's.
	got[0] = ".mutated"
	if again := AllowedExts(); again[0] == ".mutated" {
		t.Error(
			"AllowedExts shares its slice; one caller's edit reached the next")
	}
}

// ----------------------------------------------------------------------------
// Registration
// ----------------------------------------------------------------------------
//
// These tests mutate package-level state, so they are NOT parallel, and the
// same goes for the two limit tests further down. The ordering that makes that
// sufficient is worth stating: Go releases parallel tests only after every
// sequential top-level test has run to completion, so a serial test that
// mutates and restores cannot overlap with a parallel reader. Adding
// t.Parallel to any test in this section breaks that and the failures land
// somewhere else.

// TestRegisterExtsNormalisesTheKey covers the three spellings a caller can
// reasonably pass, all of which must name one entry.
func TestRegisterExtsNormalisesTheKey(t *testing.T) {
	restoreExts(t)

	RegisterExts(map[string]string{
		"HEIC":  "image/heic",
		".AVIF": "image/avif",
		" svg ": "image/svg+xml",
		"":      "ignored",
	})

	for _, supplied := range []string{"a.heic", "a.HEIC", "a.avif", "a.svg"} {
		if !HasAllowedExt(supplied) {
			t.Errorf("HasAllowedExt(%q) is false after registration", supplied)
		}
	}
	if slices.Contains(AllowedExts(), ".") {
		t.Error("an empty extension was registered as \".\"")
	}
	// The derived allowlist has to be rebuilt, or a registered format sniffs
	// correctly and is then discarded as not-allowed.
	if got := DetectContentType("a.avif", nil); got != "image/avif" {
		t.Errorf("DetectContentType: got %q, want %q", got, "image/avif")
	}
}

// TestRegisterExtIsAdditive states the property the doc promises: the defaults
// survive, so a project that widens the table does not have to restate it.
func TestRegisterExtIsAdditive(t *testing.T) {
	restoreExts(t)

	RegisterExt(".csv", "text/csv")

	if !HasAllowedExt("a.pdf") {
		t.Error("registering an extension dropped the defaults")
	}
	if !HasAllowedExt("a.csv") {
		t.Error("the new extension was not registered")
	}
}

// TestRegisterExtsNormalisesTheContentType is the value-side counterpart to
// the key test above, and it matters more, because this value is what
// DetectContentType RETURNS — into a column and back out as a header.
//
// An untrimmed type reaches that header verbatim, where the leading space
// makes it a malformed field value. An empty one would make DetectContentType
// answer "" for a correctly named file, which is the one thing its doc
// promises never happens.
func TestRegisterExtsNormalisesTheContentType(t *testing.T) {
	restoreExts(t)

	RegisterExts(map[string]string{
		".heic": "  image/heic  ",
		".foo":  "",
		".bar":  "   ",
	})

	if got := DetectContentType("a.heic", nil); got != "image/heic" {
		t.Errorf(
			"DetectContentType: got %q, want %q — the registered type must "+
				"be trimmed before it can reach a header",
			got,
			"image/heic")
	}

	for _, supplied := range []string{"a.foo", "a.bar"} {
		if HasAllowedExt(supplied) {
			t.Errorf(
				"HasAllowedExt(%q) is true — an entry with no content type "+
					"must not be registered",
				supplied)
		}
		if got := DetectContentType(supplied, tiffBytes); got == "" {
			t.Errorf(
				"DetectContentType(%q) returned the empty string, which the "+
					"doc promises it never does",
				supplied)
		}
	}
}

// TestRegisteringATextSubtypeNeedsAContainer pins the guidance on OfficeExts,
// which is easy to get half right.
//
// Go's sniffer reports "text/plain; charset=utf-8" for a CSV, so
// registering .csv as text/csv on its own leaves a genuine CSV
// recognised-but-not-accepted and collapsed to application/octet-stream —
// the registration then buys nothing beyond letting the name past
// HasAllowedExt. Declaring plain text as the container is what completes it,
// and it stays narrow: HTML named .csv is still refused, and no other
// extension gains anything.
func TestRegisteringATextSubtypeNeedsAContainer(t *testing.T) {
	restoreExts(t)

	const (
		csv     = "text/csv"
		generic = "application/octet-stream"
	)
	csvBytes := []byte("kode,no_hak\n35.002,12\n")

	RegisterExt(".csv", csv)
	if got := DetectContentType("data.csv", csvBytes); got != generic {
		t.Errorf(
			"DetectContentType before the container declaration: got %q, "+
				"want %q",
			got,
			generic)
	}

	RegisterContainer(csv, "text/plain; charset=utf-8")

	if got := DetectContentType("data.csv", csvBytes); got != csv {
		t.Errorf("DetectContentType: got %q, want %q", got, csv)
	}
	if got := DetectContentType("data.csv", htmlBytes); got != generic {
		t.Errorf(
			"html named .csv: got %q, want %q — the container exception must "+
				"not admit a different text type",
			got,
			generic)
	}
	if got := DetectContentType("scan.pdf", csvBytes); got != generic {
		t.Errorf(
			"plain text named .pdf: got %q, want %q — declaring one "+
				"container must not loosen every other extension",
			got,
			generic)
	}
}

// TestContainerExceptionRefinesAZipSniff is the OOXML case, and the reason
// the second outcome exists at all.
//
// A .docx IS a ZIP, so Go sniffs it as one. Without the exception the
// "recognised but not allowed" rule catches it — application/zip is not an
// accepted type — and every office document stored would be recorded as
// application/octet-stream.
func TestContainerExceptionRefinesAZipSniff(t *testing.T) {
	restoreExts(t)

	RegisterExts(OfficeExts)

	cases := []struct{ supplied, want string }{
		{"letter.docx", OfficeExts[".docx"]},
		{"data.xlsx", OfficeExts[".xlsx"]},
		{"slide.pptx", OfficeExts[".pptx"]},
		{"letter.odt", OfficeExts[".odt"]},
	}
	for _, c := range cases {
		if got := DetectContentType(c.supplied, zipBytes); got != c.want {
			t.Errorf(
				"DetectContentType(%q, zip): got %q, want %q",
				c.supplied, got, c.want)
		}
	}
}

// TestContainerExceptionIsKeyedOnTheFormat is what keeps the exception from
// becoming "trust the extension whenever the sniff disagrees".
//
// It fires only when the extension's REGISTERED type is one declared to live
// in the sniffed container. A .pdf full of ZIP bytes matches nothing, and a
// .docx full of HTML sniffs as text/html — which is not its container — so
// the XSS case is untouched by the office registration.
func TestContainerExceptionIsKeyedOnTheFormat(t *testing.T) {
	restoreExts(t)

	RegisterExts(OfficeExts)

	const generic = "application/octet-stream"
	cases := []struct {
		supplied string
		data     []byte
		why      string
	}{
		{"scan.pdf", zipBytes, "a pdf is not a zip-backed format"},
		{"letter.docx", htmlBytes, "text/html is not the docx container"},
		{"letter.doc", htmlBytes, "legacy office formats have no container"},
	}
	for _, c := range cases {
		if got := DetectContentType(c.supplied, c.data); got != generic {
			t.Errorf(
				"DetectContentType(%q): got %q, want %q (%s)",
				c.supplied, got, generic, c.why)
		}
	}
}

// TestRegisterContainerDeclaresANewContainer covers the exported hook, for a
// project carrying a zip-backed format this package does not know.
func TestRegisterContainerDeclaresANewContainer(t *testing.T) {
	restoreExts(t)

	const epub = "application/epub+zip"
	RegisterExt(".epub", epub)

	// Before the declaration the sniff is one layer too shallow and is
	// refused, which is the conservative default.
	if got := DetectContentType("book.epub", zipBytes); got == epub {
		t.Fatal("an undeclared container was refined from its sniff")
	}

	RegisterContainer(epub, "application/zip")

	if got := DetectContentType("book.epub", zipBytes); got != epub {
		t.Errorf("DetectContentType: got %q, want %q", got, epub)
	}
}

// TestRegisteringAnArchiveTypeDefeatsTheException documents the one
// registration that quietly undoes the exception, so that the behaviour is
// pinned rather than discovered.
//
// Admitting application/zip to the accepted set makes the FIRST rule match
// every office document, and each one is then recorded as a zip. That is a
// consequence of trusting an accepted sniff, not a bug in the exception —
// but it is the thing to look at when a .docx starts coming back as a zip.
func TestRegisteringAnArchiveTypeDefeatsTheException(t *testing.T) {
	restoreExts(t)

	RegisterExts(OfficeExts)
	RegisterExt(".zip", "application/zip")

	const zip = "application/zip"
	if got := DetectContentType("letter.docx", zipBytes); got != zip {
		t.Errorf(
			"DetectContentType: got %q, want %q — an accepted sniff wins",
			got, zip)
	}
}

// ----------------------------------------------------------------------------
// Limits
// ----------------------------------------------------------------------------

// TestNameTooLongMeasuresBytes states the rule callers must apply, and the
// reason the helper exists rather than an exported comparison.
//
// The cap mirrors a VARCHAR(n) column. SQL Server sizes those in storage
// bytes, so a rune count is the LOOSER of the two: half the cap in two-byte
// runes is over the cap in bytes, a rune check passes it, and the driver then
// truncates or errors after the file has already landed on disk.
func TestNameTooLongMeasuresBytes(t *testing.T) {
	t.Parallel()

	// Ω is two bytes in UTF-8, so half the cap plus one rune is one byte over
	// the limit while remaining well under it by rune count.
	supplied := strings.Repeat("Ω", MaxNameLen/2+1)
	if runes := len([]rune(supplied)); runes > MaxNameLen {
		t.Fatalf("fixture is wrong: %d runes already exceeds the cap", runes)
	}
	if len(supplied) <= MaxNameLen {
		t.Fatalf(
			"fixture is wrong: %d bytes does not exceed the cap",
			len(supplied))
	}
	if !NameTooLong(supplied) {
		t.Errorf(
			"a %d-byte name passed a %d-byte cap; the check is counting "+
				"runes and is looser than the column",
			len(supplied),
			MaxNameLen)
	}
}

// TestNameTooLongBoundary checks both sides, because a comparison written with
// the wrong operator passes the rejecting half on its own.
func TestNameTooLongBoundary(t *testing.T) {
	t.Parallel()

	if NameTooLong(strings.Repeat("a", MaxNameLen)) {
		t.Error("a name of exactly the cap must fit")
	}
	if !NameTooLong(strings.Repeat("a", MaxNameLen+1)) {
		t.Error("a name one byte over the cap must be refused")
	}
	if NameTooLong("") {
		t.Error("an empty name is not too long")
	}
}

// TestZeroCapDisablesTheLimit is the half a hand-written comparison gets
// backwards. Zero and negative mean NO cap, and `len(supplied) > MaxNameLen`
// against a zero cap refuses every name there is.
func TestZeroCapDisablesTheLimit(t *testing.T) {
	restoreLimits(t)

	for _, limit := range []int{0, -1} {
		MaxNameLen = limit
		if NameTooLong(strings.Repeat("a", 10_000)) {
			t.Errorf("MaxNameLen = %d must disable the name cap", limit)
		}
		MaxFileBytes = limit
		if TooLarge(1 << 30) {
			t.Errorf("MaxFileBytes = %d must disable the size cap", limit)
		}
	}
}

func TestTooLargeBoundary(t *testing.T) {
	t.Parallel()

	if TooLarge(MaxFileBytes) {
		t.Error("a payload of exactly the cap must fit")
	}
	if !TooLarge(MaxFileBytes + 1) {
		t.Error("a payload one byte over the cap must be refused")
	}
}

// TestLabelsTrackTheirLimits is the whole reason the labels are functions.
// A string computed once describes the value it was computed from forever,
// which is how a client comes to be told a number that is not the one
// enforced.
func TestLabelsTrackTheirLimits(t *testing.T) {
	restoreLimits(t)

	MaxFileBytes = 20 << 20
	if got, want := MaxFileLabel(), "20 MiB"; got != want {
		t.Errorf("MaxFileLabel: got %q, want %q", got, want)
	}
	MaxFileBytes = 5 << 20
	if got, want := MaxFileLabel(), "5 MiB"; got != want {
		t.Errorf("MaxFileLabel: got %q, want %q", got, want)
	}

	MaxNameLen = 50
	if got, want := MaxNameLabel(), "50 bytes"; got != want {
		t.Errorf("MaxNameLabel: got %q, want %q", got, want)
	}
}

// TestMaxNameLabelNamesTheUnit is separate because the unit is the half that
// goes wrong silently. "at most 100 characters" against a byte check is a
// promise that is not kept: a client sending eighty accented characters is
// refused for its byte count and told it sent too many characters.
func TestMaxNameLabelNamesTheUnit(t *testing.T) {
	t.Parallel()

	label := MaxNameLabel()
	if !strings.Contains(label, strconv.Itoa(MaxNameLen)) {
		t.Errorf("MaxNameLabel %q does not state the enforced limit", label)
	}
	if !strings.HasSuffix(label, " bytes") {
		t.Errorf("MaxNameLabel %q must name the unit, which is bytes", label)
	}
}

// TestHumanBytesStaysTruthful pins the rule that the label only uses a larger
// unit when the number divides into it exactly. Rounding would print a figure
// the check does not enforce, which is the drift these labels exist to stop.
func TestHumanBytesStaysTruthful(t *testing.T) {
	t.Parallel()

	cases := []struct {
		n    int
		want string
	}{
		{20 << 20, "20 MiB"},
		{512 << 10, "512 KiB"},
		{1 << 30, "1 GiB"},
		{1024, "1 KiB"},
		{1023, "1023 bytes"},
		{(20 << 20) + 1, "20971521 bytes"},
		{0, "0 bytes"},
	}
	for _, c := range cases {
		if got := humanBytes(c.n); got != c.want {
			t.Errorf("humanBytes(%d): got %q, want %q", c.n, got, c.want)
		}
	}
}

// TestDefaultLimits is a tripwire, not a preference. Both defaults are
// chosen against something outside this package — a column width, an HTTP
// body limit — so changing either here has to mean somebody has already
// changed the thing it mirrors.
func TestDefaultLimits(t *testing.T) {
	t.Parallel()

	if MaxNameLen != 100 {
		t.Errorf(
			"MaxNameLen default: got %d, want 100 to mirror a VARCHAR(100) "+
				"name column; widen the column before raising this",
			MaxNameLen)
	}
	if MaxFileBytes != 20<<20 {
		t.Errorf(
			"MaxFileBytes default: got %d, want %d (20 MiB)",
			MaxFileBytes, 20<<20)
	}
}

// ----------------------------------------------------------------------------
// Base64 payloads
// ----------------------------------------------------------------------------

// TestDecodeBase64File covers each shape a client actually sends. The
// tolerance is the point: a browser FileReader produces a data URL, several
// languages' encoders omit padding or wrap at 76 columns, and
// base64.StdEncoding.DecodeString refuses the last two outright.
func TestDecodeBase64File(t *testing.T) {
	t.Parallel()

	const want = "Hello World"
	cases := []struct {
		name    string
		payload string
	}{
		{"padded", "SGVsbG8gV29ybGQ="},
		{"unpadded", "SGVsbG8gV29ybGQ"},
		{"data url", "data:application/pdf;base64,SGVsbG8gV29ybGQ="},
		{"data url uppercase", "DATA:text/plain;BASE64,SGVsbG8gV29ybGQ="},
		{"line wrapped", "SGVsbG8g\nV29ybGQ="},
		{"spaces", "SGVsbG8g V29ybGQ ="},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := DecodeBase64File(c.payload)
			if err != nil {
				t.Fatalf("DecodeBase64File: %v", err)
			}
			if string(got) != want {
				t.Errorf("got %q, want %q", got, want)
			}
		})
	}
}

// TestDecodeBase64FileURLSafeAlphabet checks the detection rule: '-' and '_'
// are illegal in the standard alphabet, so their presence identifies the
// URL-safe one without a flag from the caller and without misreading any valid
// standard payload.
func TestDecodeBase64FileURLSafeAlphabet(t *testing.T) {
	t.Parallel()

	// 0xFB 0xFF encodes as "+/8=" in the standard alphabet and "-_8=" in the
	// URL-safe one.
	got, err := DecodeBase64File("-_8=")
	if err != nil {
		t.Fatalf("DecodeBase64File: %v", err)
	}
	if want := []byte{0xFB, 0xFF}; string(got) != string(want) {
		t.Errorf("got % x, want % x", got, want)
	}
}

// TestDecodeBase64FileRejectsEmpty separates "no file was sent" from "the file
// failed to decode", which are different problems for the caller.
func TestDecodeBase64FileRejectsEmpty(t *testing.T) {
	t.Parallel()

	for _, payload := range []string{"", "   ", "\n\t", "data:,"} {
		if _, err := DecodeBase64File(payload); err == nil {
			t.Errorf("DecodeBase64File(%q) succeeded, want an error", payload)
		}
	}
}

// TestDecodeBase64FileDoesNotTruncateAtAnyComma is why the data-URL prefix is
// only stripped when the text before the comma looks like one.
//
// Cutting unconditionally is harmless for valid base64, whose alphabet has no
// comma — and it silently halves anything else, turning a malformed payload
// into a confusing decode error instead of an honest one.
func TestDecodeBase64FileDoesNotTruncateAtAnyComma(t *testing.T) {
	t.Parallel()

	_, err := DecodeBase64File("not base64 at all,SGVsbG8gV29ybGQ=")
	if err == nil {
		t.Fatal("a payload with a stray comma decoded; it was cut in half")
	}
	if strings.Contains(err.Error(), "payload is empty") {
		t.Errorf("misleading error for a malformed payload: %v", err)
	}
}

// TestDecodeBase64FileChecksSizeBeforeDecoding is the allocation guard. Four
// base64 characters are three bytes, so an over-limit payload is known to be
// over the limit from the string alone — and decoding it first would be an
// allocation the process never needed to make.
func TestDecodeBase64FileChecksSizeBeforeDecoding(t *testing.T) {
	restoreLimits(t)

	MaxFileBytes = 16

	// Deliberately not valid base64. If the size check ran after the decode,
	// the error would name the alphabet rather than the limit.
	payload := strings.Repeat("!", 1024)
	_, err := DecodeBase64File(payload)
	if err == nil {
		t.Fatal("an over-limit payload decoded")
	}
	if !strings.Contains(err.Error(), MaxFileLabel()) {
		t.Errorf(
			"error %q does not name the %s limit, so the size check ran "+
				"after the decode", err, MaxFileLabel())
	}
}

// TestDecodeBase64FileChecksTheDecodedSize covers the second check, which the
// first cannot replace: the pre-check is an estimate from the encoded length,
// and whitespace and padding make it approximate.
func TestDecodeBase64FileChecksTheDecodedSize(t *testing.T) {
	restoreLimits(t)

	MaxFileBytes = 4

	// The estimate is len/4*3, rounded DOWN, so an unpadded payload of seven
	// characters is read as three bytes and decodes to five. Under a cap of
	// four the pre-check waves it through and only the second check refuses
	// it, which is the case that would survive removing that check.
	const payload = "SGVsbG8" // "Hello", unpadded

	if approx := len(payload) / 4 * 3; approx > MaxFileBytes {
		t.Fatalf(
			"fixture is wrong: the pre-check already refuses %d bytes",
			approx)
	}
	_, err := DecodeBase64File(payload)
	if err == nil {
		t.Fatal("a payload decoding to 5 bytes passed a 4-byte cap")
	}
	if !strings.Contains(err.Error(), "5 bytes") {
		t.Errorf("error %q does not report the decoded length", err)
	}
}
