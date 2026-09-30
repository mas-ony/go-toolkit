package document

// Tests for document.go, against a temporary directory: the three Lookup
// passes and their order, the dotted reference that must not be stripped,
// case-insensitivity, ambiguity named by path under the root, the dotfile,
// dot-directory and extension skips, recursion, the size cap read from the
// stat, and the name cap measured in bytes. What depends on the filesystem,
// the platform or the process is in document_integration_test.go.

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/mas-ony/go-toolkit/fileutil"
)

// indexOf builds an Index over dir, failing the test on error.
func indexOf(t *testing.T, dir string, recursive bool) *Index {
	t.Helper()
	idx, err := NewIndex(dir, recursive)
	if err != nil {
		t.Fatalf("NewIndex(%q, %v): %v", dir, recursive, err)
	}
	return idx
}

// withMaxFileBytes sets the package-wide cap for one test and restores it
// afterwards.
//
// It is a global in fileutil, so a test that changed it and left it changed
// would silently rewrite what every later test in this file expects. None
// of these run in parallel, which is what makes borrowing it safe.
func withMaxFileBytes(t *testing.T, n int) {
	t.Helper()
	old := fileutil.MaxFileBytes
	fileutil.MaxFileBytes = n
	t.Cleanup(func() { fileutil.MaxFileBytes = old })
}

// write creates a file under dir with one byte of content, making the parent
// directories as needed. Content is deliberately non-empty: Load and ReadFile
// both refuse a zero-byte file, so a helper that wrote nothing would make
// every other test in this file fail for the wrong reason.
func write(t *testing.T, dir, rel string) string {
	t.Helper()
	path := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", rel, err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
	return path
}

// writeBytes is write with the content supplied, for the tests where the
// SIZE of the file is the thing under test rather than its name.
func writeBytes(t *testing.T, dir, rel string, content []byte) string {
	t.Helper()
	path := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", rel, err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
	return path
}

// Count reports files, not distinct stems. The two differ exactly when one
// document has been scanned twice, which is the case an operator most needs
// to see in the startup log — an index reporting "3 files" for four files on
// disk is the one number that would have named the duplicate.
func TestCountReportsFilesNotStems(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "1.pdf")
	write(t, dir, "1.jpg")
	write(t, dir, "2.pdf")

	if got := indexOf(t, dir, false).Count(); got != 3 {
		t.Errorf("Count: got %d, want 3 (two stems, three files)", got)
	}
}

// The same filename in two subdirectories is likewise two files. Both index
// maps collapse them onto one key, so a Count taken from either map's length
// would report one.
func TestCountReportsDuplicateNamesAcrossSubdirs(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "2019/1.pdf")
	write(t, dir, "2020/1.pdf")

	if got := indexOf(t, dir, true).Count(); got != 2 {
		t.Errorf("Count: got %d, want 2", got)
	}
}

// A full filename is the more SPECIFIC key, not an exemption from the
// ambiguity rule. An archive foldered by year holds two different documents
// both called "1.pdf", and handing back whichever the walk reached first is
// the same arbitrary answer the stem rule exists to refuse.
//
// The error names each candidate by its path under the indexed root: two
// entries reading "1.pdf, 1.pdf" would tell an operator that something is
// duplicated and nothing at all about where.
func TestLookupAmbiguousFilenameAcrossSubdirs(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "2019/1.pdf")
	write(t, dir, "2020/1.pdf")

	got, err := indexOf(t, dir, true).Lookup("1.pdf")
	if !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("Lookup = %q, %v; want ErrAmbiguous", got, err)
	}
	for _, want := range []string{
		filepath.Join("2019", "1.pdf"),
		filepath.Join("2020", "1.pdf"),
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

// Scans produced on Windows, where "1.PDF" and "1.pdf" name one file, are
// read on Linux, where they do not. Every reference form must resolve.
func TestLookupIsCaseInsensitive(t *testing.T) {
	dir := t.TempDir()
	want := write(t, dir, "35.002.PDF")

	idx := indexOf(t, dir, false)
	refs := []string{"35.002.PDF", "35.002.pdf", "35.002", "  35.002  "}
	for _, ref := range refs {
		got, err := idx.Lookup(ref)
		if err != nil {
			t.Errorf("Lookup(%q): %v", ref, err)
			continue
		}
		if got != want {
			t.Errorf("Lookup(%q): got %q, want %q", ref, got, want)
		}
	}
}

// A full filename is matched before any stem, so a caller that can name the
// file outright never meets ErrAmbiguous even when the stem is shared.
func TestLookupPrefersFullFilenameOverStem(t *testing.T) {
	dir := t.TempDir()
	want := write(t, dir, "1.jpg")
	write(t, dir, "1.pdf")

	got, err := indexOf(t, dir, false).Lookup("1.jpg")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got != want {
		t.Errorf("Lookup: got %q, want %q", got, want)
	}
}

// A bare stem matching two files is refused, and the error names both. Picking
// one would attach an arbitrary document to whatever the caller is filing.
func TestLookupAmbiguousStemNamesCandidates(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "1.pdf")
	write(t, dir, "1.jpg")

	_, err := indexOf(t, dir, false).Lookup("1")
	if !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("Lookup: got %v, want ErrAmbiguous", err)
	}
	for _, want := range []string{"1.jpg", "1.pdf"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

// A reference carrying an extension the directory does not use still resolves
// through the stem: a workbook saying "1.pdf" against a re-scanned "1.tif".
func TestLookupFallsBackToStemOnDifferentExtension(t *testing.T) {
	dir := t.TempDir()
	want := write(t, dir, "1.tif")

	got, err := indexOf(t, dir, false).Lookup("1.pdf")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got != want {
		t.Errorf("Lookup: got %q, want %q", got, want)
	}
}

// The stem fallback strips a trailing extension, and filepath.Ext knows only
// about the last dot — so a dotted identifier is indistinguishable from a
// name carrying an extension. "35.002" reads as a stem of "35" and an
// extension of ".002", and an ungated fallback resolves it to "35.pdf": a
// document with no relation to the reference, returned with no error, which
// is the outcome ErrAmbiguous exists to prevent.
//
// Both halves are here because the gate has to keep the fallback working.
// Stripping ".pdf" is the case it is for; stripping ".002" is the case that
// breaks it, and only an extension fileutil accepts tells the two apart.
func TestLookupDoesNotStripADottedReference(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "35.pdf")

	got, err := indexOf(t, dir, false).Lookup("35.002")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf(
			"Lookup(\"35.002\") = %q, %v; want ErrNotFound — \".002\" is "+
				"part of the identifier, not an extension to strip",
			got, err)
	}
}

// The same stripping reaches ErrAmbiguous rather than a wrong file when the
// truncated stem happens to be a duplicate, which is the shape that makes the
// bug look like a data problem in the operator's report.
func TestLookupDottedReferenceDoesNotReportAmbiguity(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "1.pdf")
	write(t, dir, "1.jpg")

	if _, err := indexOf(t, dir, false).Lookup("1.002"); !errors.Is(
		err, ErrNotFound) {
		t.Errorf(
			"Lookup(\"1.002\"): got %v, want ErrNotFound — the directory "+
				"holds no 1.002, so neither candidate is a match", err)
	}
}

// A blank reference and one naming nothing both come back as ErrNotFound,
// since to a caller iterating rows they are the same outcome.
func TestLookupMissAndBlankAreErrNotFound(t *testing.T) {
	idx := indexOf(t, t.TempDir(), false)
	for _, ref := range []string{"", "   ", "nope"} {
		if _, err := idx.Lookup(ref); !errors.Is(err, ErrNotFound) {
			t.Errorf("Lookup(%q): got %v, want ErrNotFound", ref, err)
		}
	}
}

// Files fileutil would refuse are counted, not indexed. A directory of .PDF_
// files must index as empty and say how many it passed over, rather than
// leaving every later reference to report a miss.
func TestNewIndexSkipsUnacceptedExtensions(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "1.pdf")
	write(t, dir, "2.PDF_")
	write(t, dir, "3.tmp")
	write(t, dir, "notes.xlsx")

	idx := indexOf(t, dir, false)
	if idx.Count() != 1 {
		t.Errorf("Count: got %d, want 1", idx.Count())
	}
	if idx.Skipped() != 3 {
		t.Errorf("Skipped: got %d, want 3", idx.Skipped())
	}
}

// Dotfiles are editor swap files and macOS resource forks, never documents —
// and ._1.pdf carries an accepted extension, so only the dot check stops it.
func TestNewIndexSkipsDotfiles(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "._1.pdf")
	write(t, dir, ".hidden.pdf")

	idx := indexOf(t, dir, false)
	if idx.Count() != 0 {
		t.Errorf("Count: got %d, want 0", idx.Count())
	}
	if idx.Skipped() != 0 {
		t.Errorf(
			"Skipped: got %d, want 0 (dotfiles are not skipped extensions)",
			idx.Skipped())
	}
}

// A flat index sees only the top level; a recursive one descends.
func TestNewIndexRecursion(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "1.pdf")
	write(t, dir, "2019/2.pdf")

	if got := indexOf(t, dir, false).Count(); got != 1 {
		t.Errorf("flat: got %d files, want 1", got)
	}
	if got := indexOf(t, dir, true).Count(); got != 2 {
		t.Errorf("recursive: got %d files, want 2", got)
	}
	if _, err := indexOf(t, dir, true).Lookup("2"); err != nil {
		t.Errorf("recursive Lookup(2): %v", err)
	}
}

// A directory that does not exist, and a path naming a file, both fail
// NewIndex rather than indexing nothing.
func TestNewIndexRejectsMissingAndNonDirectory(t *testing.T) {
	dir := t.TempDir()
	file := write(t, dir, "1.pdf")

	if _, err := NewIndex(filepath.Join(dir, "absent"), false); err == nil {
		t.Error("expected an error for a directory that does not exist")
	}
	if _, err := NewIndex(file, false); err == nil {
		t.Error("expected an error for a path that is a file")
	}
}

// Load returns the file's own basename rather than the reference, because the
// extension is what tells a download path and a browser what the bytes are and
// a bare stem carries none.
func TestLoadReturnsBasenameNotReference(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "35.002.pdf")

	name, data, err := indexOf(t, dir, false).Load("35.002")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if name != "35.002.pdf" {
		t.Errorf("name: got %q, want %q", name, "35.002.pdf")
	}
	if len(data) == 0 {
		t.Error("data is empty")
	}
}

// A zero-byte file is refused rather than returned as a document.
func TestLoadRejectsEmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "1.pdf")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, _, err := indexOf(t, dir, false).Load("1"); err == nil {
		t.Error("expected a zero-byte file to be refused")
	}
}

// The boundary: MaxNameLen bytes fits, one more does not. Both sides are
// checked because a check written with the wrong comparison passes the
// rejecting half on its own.
func TestLoadNameLengthBoundary(t *testing.T) {
	dir := t.TempDir()
	// + ".pdf" makes exactly MaxNameLen bytes.
	fits := strings.Repeat("a", fileutil.MaxNameLen-4)
	write(t, dir, fits+".pdf")

	if _, _, err := indexOf(t, dir, false).Load(fits); err != nil {
		t.Errorf("a name of exactly MaxNameLen bytes must be read: %v", err)
	}

	over := t.TempDir()
	stem := strings.Repeat("a", fileutil.MaxNameLen-3)
	write(t, over, stem+".pdf") // MaxNameLen + 1 bytes

	_, _, err := indexOf(t, over, false).Load(stem)
	if err == nil {
		t.Fatal("expected an over-long filename to be refused")
	}
	if !strings.Contains(err.Error(), "bytes") {
		t.Errorf("error %q should say the limit is in bytes", err)
	}
}

// The cap is measured in BYTES, and only a multi-byte name can say so. An
// all-ASCII name has the same byte and rune count, so a test built from one
// passes under either implementation and pins nothing — which is what the
// boundary test above is and all it claims to be.
//
// Fifty "é" plus ".pdf" is 54 runes and 104 bytes. A rune count admits it
// into a 100-byte column; the column does not, and the name arrives as a
// driver truncation error after the file is already on disk.
func TestLoadNameLengthIsMeasuredInBytes(t *testing.T) {
	dir := t.TempDir()
	stem := strings.Repeat("é", 50) // 100 bytes, 50 runes
	write(t, dir, stem+".pdf")      // 104 bytes, 54 runes

	if _, _, err := indexOf(t, dir, false).Load(stem); err == nil {
		t.Error(
			"a 104-byte filename was accepted against a 100-byte cap; the " +
				"check is counting runes and is looser than the column")
	}
}

// A path is a statement about where a file is, not about what it is. A link
// that turns out to point at a spreadsheet must be refused here rather than
// stored and later served with a Content-Type derived from it.
func TestReadFileRejectsDisallowedType(t *testing.T) {
	dir := t.TempDir()
	path := write(t, dir, "notes.xlsx")

	_, _, err := ReadFile(path)
	if err == nil {
		t.Fatal("expected an .xlsx target to be refused")
	}
	if !strings.Contains(err.Error(), "not an accepted file type") {
		t.Errorf("error %q should name the reason", err)
	}
}

// A blank path is ErrNotFound, and a directory is refused.
func TestReadFileRejectsBlankPathAndDirectory(t *testing.T) {
	if _, _, err := ReadFile(""); !errors.Is(err, ErrNotFound) {
		t.Errorf(`ReadFile(""): got %v, want ErrNotFound`, err)
	}
	if _, _, err := ReadFile(t.TempDir()); err == nil {
		t.Error("expected a directory to be refused")
	}
}

// ReadFile reads a file with an accepted extension, a dotted stem
// included, and returns the file's own name.
func TestReadFileAcceptsAllowedType(t *testing.T) {
	dir := t.TempDir()
	path := write(t, dir, "35.002.pdf")

	name, data, err := ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if name != "35.002.pdf" {
		t.Errorf("name: got %q, want %q", name, "35.002.pdf")
	}
	if len(data) == 0 {
		t.Error("data is empty")
	}
}

// The size cap is checked from the STAT, before the read, which is the
// whole reason it is worth having: an archive holding one enormous file
// must not cost the memory to discover that.
//
// Both sides of the boundary are asserted, because a cap that refused the
// file AT the limit would be just as wrong as one that admitted the file
// over it, and only one of the two shows up as an outage.
func TestLoadRefusesAFileOverTheSizeCap(t *testing.T) {
	withMaxFileBytes(t, 64)

	dir := t.TempDir()
	writeBytes(t, dir, "at-limit.pdf", bytes.Repeat([]byte("x"), 64))
	writeBytes(t, dir, "over.pdf", bytes.Repeat([]byte("x"), 65))

	idx := indexOf(t, dir, false)

	if _, data, err := idx.Load("at-limit"); err != nil {
		t.Errorf("a file of exactly MaxFileBytes must be read: %v", err)
	} else if len(data) != 64 {
		t.Errorf("read %d bytes, want 64", len(data))
	}

	_, _, err := idx.Load("over")
	if err == nil {
		t.Fatal("expected an over-sized file to be refused")
	}
	// The message has to carry the limit, since that is the only thing
	// telling an operator whether to raise the cap or fix the archive.
	if !strings.Contains(err.Error(), fileutil.MaxFileLabel()) {
		t.Errorf("error %q does not name the %s limit",
			err, fileutil.MaxFileLabel())
	}
}

// ReadFile applies the same cap. It is a separate path — it stats a caller
// -supplied path rather than one the index resolved — so it needs its own
// assertion rather than inheriting Load's.
func TestReadFileRefusesAFileOverTheSizeCap(t *testing.T) {
	withMaxFileBytes(t, 32)

	dir := t.TempDir()
	path := writeBytes(t, dir, "big.pdf", bytes.Repeat([]byte("x"), 33))

	if _, _, err := ReadFile(path); err == nil {
		t.Error("expected an over-sized file to be refused")
	}
}

// The case document.go argues for at length: a zero or negative cap means
// NO cap in fileutil, so going through TooLarge rather than comparing
// against MaxFileBytes by hand is what stops a deployment that DISABLED
// the limit from having every file refused for exceeding nothing.
//
// A hand-written `size > fileutil.MaxFileBytes` passes every other test in
// this file and fails only here, which is exactly what makes this worth a
// test rather than a comment.
func TestADisabledSizeCapAdmitsEverything(t *testing.T) {
	for _, limit := range []int{0, -1} {
		t.Run(fmt.Sprintf("cap=%d", limit), func(t *testing.T) {
			withMaxFileBytes(t, limit)

			dir := t.TempDir()
			writeBytes(t, dir, "big.pdf",
				bytes.Repeat([]byte("x"), 4096))

			_, data, err := indexOf(t, dir, false).Load("big")
			if err != nil {
				t.Fatalf("a disabled cap must admit every file: %v",
					err)
			}
			if len(data) != 4096 {
				t.Errorf("read %d bytes, want 4096", len(data))
			}
		})
	}
}

// An Index is documented as read-only once NewIndex returns, and therefore
// safe for concurrent Lookup, Load, Count and Skipped from any number of
// goroutines. Nothing in the type enforces that — the maps are simply
// never written after the build — so the claim rests on a property a
// future change could remove without any compiler complaint.
//
// This test is only meaningful under -race, where a map written during the
// build and read here would be reported. Without it the test still runs
// and still checks that every answer is correct under contention, which is
// the weaker half.
func TestIndexIsSafeForConcurrentReaders(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{
		"1.pdf", "2.pdf", "3.jpg", "sub/4.pdf", "sub/5.png",
	} {
		write(t, dir, name)
	}
	idx := indexOf(t, dir, true)

	const readers = 16
	const rounds = 50

	var wg sync.WaitGroup
	errs := make(chan string, readers*4)

	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				if got := idx.Count(); got != 5 {
					errs <- fmt.Sprintf("Count = %d, want 5", got)
					return
				}
				_ = idx.Skipped()

				// A hit, resolved by stem.
				if _, err := idx.Lookup("1"); err != nil {
					errs <- fmt.Sprintf("Lookup(1): %v", err)
					return
				}
				// A miss, which walks all three passes.
				if _, err := idx.Lookup("nope"); !errors.Is(
					err, ErrNotFound) {
					errs <- fmt.Sprintf("Lookup(nope) = %v", err)
					return
				}
				// A read, which goes to the filesystem.
				name, data, err := idx.Load("4")
				if err != nil {
					errs <- fmt.Sprintf("Load(4): %v", err)
					return
				}
				if name != "4.pdf" || len(data) == 0 {
					errs <- fmt.Sprintf(
						"Load(4) = %q, %d bytes", name, len(data))
					return
				}
			}
		}(r)
	}

	wg.Wait()
	close(errs)
	for msg := range errs {
		t.Error(msg)
	}
}

// A recursive walk skips a dot-directory whole, the way a dotfile is
// skipped, so a copy left in a trash or thumbnail folder never resolves —
// least of all after the original is gone — while an ordinary
// subdirectory is still descended.
func TestNewIndexSkipsDotDirectories(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "1.pdf")
	write(t, dir, "sub/2.pdf")
	write(t, dir, ".Trash/3.pdf")
	write(t, dir, "sub/.thumbs/4.jpg")

	idx := indexOf(t, dir, true)
	if got := idx.Count(); got != 2 {
		t.Errorf("Count = %d, want 2 (1.pdf and sub/2.pdf)", got)
	}
	for _, ref := range []string{"3", "4"} {
		if _, err := idx.Lookup(ref); !errors.Is(err, ErrNotFound) {
			t.Errorf("Lookup(%q) = %v, want ErrNotFound", ref, err)
		}
	}
	if got := idx.Skipped(); got != 0 {
		t.Errorf("Skipped = %d, want 0: a dot-directory is not an "+
			"extension skip", got)
	}
}
