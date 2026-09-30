//go:build integration

package document

// Integration tests for document.
//
// Everything in document_test.go runs against t.TempDir() and is already
// hitting a real filesystem, so "integration" here does not mean "now
// with disk". It means the claims this package makes about the
// ENVIRONMENT rather than about its own logic — claims a unit test cannot
// assert because the answer depends on which filesystem the directory
// sits on and which user the process runs as.
//
// Four of them:
//
//   - Case. The package exists because "1.PDF" and "1.pdf" are one file
//     on Windows and two on Linux. A test that creates both and expects
//     an ambiguity is asserting a property of the filesystem, and on
//     macOS's default APFS or on NTFS it would fail for the right reason
//     and look like a bug in Lookup.
//   - Permissions. An unreadable subdirectory is skipped and an
//     unreadable ROOT fails the call — a distinction argued at length in
//     NewIndex and reachable only by removing permission bits, which does
//     nothing to a process running as root.
//   - Symlinks. A link named like a document is indexed and read through,
//     which the package documents as a deliberate non-boundary. Creating
//     one needs a filesystem and a platform that allows it.
//   - The snapshot. A file added after the walk is invisible and one
//     removed after it still resolves, which needs the tree to change
//     under an Index already built.
//
// Each skips rather than fails where the environment cannot produce the
// precondition. A skipped test says the claim was not checked here; a
// failing one would say the code is wrong, and that would be a lie.
//
//	go test -tags integration -run Integration ./document

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// requireUnprivileged skips when permission bits will not bite.
func requireUnprivileged(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("running as root: a mode-000 directory is still readable")
	}
}

// caseSensitiveDir reports whether dir distinguishes two names that differ
// only in case, by asking the filesystem rather than by guessing from
// runtime.GOOS — a Linux box can mount a case-insensitive volume and a Mac
// can format a case-sensitive one.
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

// writeFile creates dir/rel with some content and returns its full path.
func writeFile(t *testing.T, dir, rel string) string {
	t.Helper()
	path := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	err := os.WriteFile(path, []byte("content of "+rel), 0o644)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// The premise of the whole package: a directory carrying the same stem in
// two cases holds two files, and Lookup must refuse to choose between
// them rather than returning whichever the walk reached first.
func TestIntegrationCaseVariantsAreTwoFiles(t *testing.T) {
	dir := t.TempDir()
	if !caseSensitiveDir(t, dir) {
		t.Skip("this filesystem folds case; both spellings are one file")
	}

	writeFile(t, dir, "1.pdf")
	writeFile(t, dir, "1.PDF")

	idx, err := NewIndex(dir, false)
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	if got := idx.Count(); got != 2 {
		t.Fatalf("Count = %d, want 2", got)
	}

	// Both the full name and the stem have to report the ambiguity. The
	// full name is the interesting one: it is the pass that normally
	// disambiguates, and here it cannot, because the index lowercases
	// every key precisely so that case never decides an answer.
	for _, ref := range []string{"1", "1.pdf", "1.PDF"} {
		_, err := idx.Lookup(ref)
		if !errors.Is(err, ErrAmbiguous) {
			t.Errorf("Lookup(%q) = %v, want ErrAmbiguous", ref, err)
		}
	}
}

// On a case-folding filesystem the two spellings are one file, so the
// index holds one entry and every reference resolves. Asserting that is
// worth as much as the test above: it is the configuration a Windows or
// macOS developer actually runs, and it must not error.
func TestIntegrationCaseFoldingFilesystemResolves(t *testing.T) {
	dir := t.TempDir()
	if caseSensitiveDir(t, dir) {
		t.Skip("this filesystem distinguishes case")
	}

	writeFile(t, dir, "1.pdf")
	writeFile(t, dir, "1.PDF") // the same file, rewritten

	idx, err := NewIndex(dir, false)
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	if got := idx.Count(); got != 1 {
		t.Fatalf("Count = %d, want 1 on a case-folding filesystem", got)
	}
	for _, ref := range []string{"1", "1.pdf", "1.PDF"} {
		if _, err := idx.Lookup(ref); err != nil {
			t.Errorf("Lookup(%q) = %v, want it to resolve", ref, err)
		}
	}
}

// The asymmetry NewIndex argues for: one unreadable subdirectory costs
// only the references it would have served, so the walk continues and the
// rest of the archive still indexes.
func TestIntegrationUnreadableSubdirectoryIsSkipped(t *testing.T) {
	requireUnprivileged(t)

	dir := t.TempDir()
	writeFile(t, dir, "visible.pdf")
	writeFile(t, dir, "locked/hidden.pdf")

	locked := filepath.Join(dir, "locked")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	// Restore before TempDir's own cleanup, which cannot remove a
	// directory it is not allowed to read.
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	idx, err := NewIndex(dir, true)
	if err != nil {
		t.Fatalf("NewIndex = %v, want the walk to continue", err)
	}
	if got := idx.Count(); got != 1 {
		t.Errorf("Count = %d, want 1 (the readable file)", got)
	}
	if _, err := idx.Lookup("visible"); err != nil {
		t.Errorf("Lookup(visible) = %v, want it to resolve", err)
	}
	// The cost of the skip, and the reason it is acceptable: the
	// reference is a miss rather than a failure, visible one by one in
	// whatever report the caller writes.
	if _, err := idx.Lookup("hidden"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Lookup(hidden) = %v, want ErrNotFound", err)
	}
}

// The other half: an unreadable ROOT must fail the call rather than
// produce a successful index reporting zero files, which would read as an
// empty archive instead of a directory this process cannot open.
//
// os.Stat succeeds on such a directory, so the guard cannot live in
// NewIndex's opening check — it is the walk callback's path == dir branch
// that catches it, and this is the only test that reaches that branch.
func TestIntegrationUnreadableRootFailsTheCall(t *testing.T) {
	requireUnprivileged(t)

	parent := t.TempDir()
	root := filepath.Join(parent, "archive")
	writeFile(t, root, "1.pdf")
	if err := os.Chmod(root, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o755) })

	idx, err := NewIndex(root, false)
	if err == nil {
		t.Fatalf("NewIndex succeeded with Count=%d, want an error",
			idx.Count())
	}
	if !strings.Contains(err.Error(), root) {
		t.Errorf("err = %v, want it to name the directory", err)
	}
}

// Documented as a deliberate non-boundary: the gate is the link's own
// name, so a link called "1.pdf" resolves and Load reads through it
// wherever it points. The test pins the behaviour so that tightening it
// later is a decision rather than an accident.
func TestIntegrationSymlinkNamedLikeADocumentIsIndexed(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()

	target := writeFile(t, outside, "real.pdf")
	link := filepath.Join(dir, "1.pdf")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable here: %v", err)
	}

	idx, err := NewIndex(dir, false)
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	if got := idx.Count(); got != 1 {
		t.Fatalf("Count = %d, want 1", got)
	}

	name, data, err := idx.Load("1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// The name is the LINK's, not the target's: it is the reference's own
	// basename that a download path and a browser will see.
	if name != "1.pdf" {
		t.Errorf("name = %q, want 1.pdf", name)
	}
	if want := "content of real.pdf"; string(data) != want {
		t.Errorf("data = %q, want %q", data, want)
	}
}

// A directory symlink is NOT descended, because WalkDir does not follow
// links. Worth pinning beside the case above, since the two together are
// the whole of the package's symlink behaviour and they point in opposite
// directions.
func TestIntegrationSymlinkedDirectoryIsNotDescended(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	writeFile(t, outside, "2.pdf")

	if err := os.Symlink(outside, filepath.Join(dir, "linked")); err != nil {
		t.Skipf("symlinks unavailable here: %v", err)
	}
	writeFile(t, dir, "1.pdf")

	idx, err := NewIndex(dir, true)
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	if got := idx.Count(); got != 1 {
		t.Errorf("Count = %d, want 1 — the linked directory was "+
			"descended", got)
	}
	if _, err := idx.Lookup("2"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Lookup(2) = %v, want ErrNotFound", err)
	}
}

// The snapshot contract in doc.go, both directions. Neither is a defect;
// both are things a caller holding a long-lived Index has to know, and
// neither is visible from the unit suite because both need the tree to
// change after the walk.
func TestIntegrationIndexIsASnapshot(t *testing.T) {
	dir := t.TempDir()
	removed := writeFile(t, dir, "gone.pdf")
	writeFile(t, dir, "stays.pdf")

	idx, err := NewIndex(dir, false)
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}

	// Added after the walk: invisible, with nothing saying the index is
	// stale.
	writeFile(t, dir, "new.pdf")
	if _, err := idx.Lookup("new"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Lookup(new) = %v, want ErrNotFound", err)
	}

	// Removed after the walk: still resolves, and fails at the READ with
	// the operating system's error rather than with ErrNotFound. A caller
	// that treats every Load failure as "missing document" will mislabel
	// this one.
	if err := os.Remove(removed); err != nil {
		t.Fatal(err)
	}
	if _, err := idx.Lookup("gone"); err != nil {
		t.Errorf("Lookup(gone) = %v, want it to still resolve", err)
	}
	_, _, err = idx.Load("gone")
	if err == nil {
		t.Fatal("Load(gone) succeeded after the file was removed")
	}
	if errors.Is(err, ErrNotFound) {
		t.Error("Load(gone) reported ErrNotFound, want the read error")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Load(gone) = %v, want it to wrap os.ErrNotExist", err)
	}
}
