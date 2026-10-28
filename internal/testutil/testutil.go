package testutil

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"testing"
)

// BotToken is a Telegram bot token in the shape BotFather issues — the
// bot's numeric id, a colon, then letters, digits, underscores and
// hyphens — that belongs to no bot. config's validation and the telegram
// package's sends are both tested with it, so one value satisfies both.
const BotToken = "123456789:AAFakeTokenForTests_0123456789abcdefg"

// Only returns the one element of items, and fails the test unless there
// is exactly one. what names the elements as the caller counts them,
// "requests" or "connections", so a fake server that recorded two where
// one was expected says so in its own words.
func Only[T any](t testing.TB, what string, items []T) T {
	t.Helper()
	if len(items) != 1 {
		t.Fatalf("saw %d %s, want 1", len(items), what)
	}
	return items[0]
}

// Set assigns v to *p until the test ends, then puts back what *p held
// before. Set(t, &x, x) changes nothing and only arranges the restore, for
// a test that changes x through the API it is testing.
//
// It is for package state, so a test that calls it must not run in
// parallel with one that reads the same variable. Go starts the parallel
// tests only once every sequential one has returned, which is what makes a
// sequential test that sets and restores safe.
func Set[T any](t testing.TB, p *T, v T) {
	t.Helper()
	old := *p
	*p = v
	t.Cleanup(func() { *p = old })
}

// CapturePanic runs fn and returns what it panicked with, or nil when it
// returned normally.
func CapturePanic(fn func()) (recovered any) {
	defer func() { recovered = recover() }()
	fn()
	return nil
}

// Glob returns the files in the test's working directory that match
// pattern, and fails the test when the pattern is malformed or matches
// nothing.
//
// It is the listing half of a suite that holds its package to a property
// of the package's own source: go test runs a package's tests in that
// package's directory, so the pattern sees exactly the package's files,
// and ParseFile reads each one. A suite that checks every file it lists
// would pass vacuously over none, which is why an empty match is a
// failure rather than an answer.
func Glob(t testing.TB, pattern string) []string {
	t.Helper()
	files, err := filepath.Glob(pattern)
	if err != nil {
		t.Fatalf("glob %q: %v", pattern, err)
	}
	if len(files) == 0 {
		t.Fatalf("no file matches %q", pattern)
	}
	return files
}

// WriteFile writes data to path, creating the directories above it, and
// returns path. It fails the test on any error.
func WriteFile(t testing.TB, path string, data []byte) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// TempFile is WriteFile under name in a directory of the test's own,
// removed when the test ends, for a file nothing has to sit beside.
func TempFile(t testing.TB, name string, data []byte) string {
	t.Helper()
	return WriteFile(t, filepath.Join(t.TempDir(), name), data)
}

// ParseFile parses the Go source file at path, comments included, and
// returns it with the file set its positions are counted in. It fails the
// test on any error.
//
// It is for a suite that holds its package to a property of the package's
// own source, which go test lets it read, since a package's tests run in
// that package's directory. Each file is parsed on its own, which keeps
// such a suite off parser.ParseDir, deprecated because it ignores build
// tags.
func ParseFile(t testing.TB, path string) (*ast.File, *token.FileSet) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	return f, fset
}

// RequireUnprivileged skips t when the process runs as root, for whom
// permission bits do not bite: a mode-000 file or directory is still
// readable, so the case where it is not cannot be produced.
func RequireUnprivileged(t testing.TB) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits do not bite")
	}
}

// Chmod sets the mode of path until the test ends, then puts back the mode
// it had. Its restore runs before the removal of any t.TempDir the test
// asked for first, since cleanups run last first, so a directory the test
// locked is unlocked again before something has to delete it.
func Chmod(t testing.TB, path string, mode os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod %s: %v", path, err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, info.Mode()) })
}

// CaseSensitiveDir reports whether dir tells apart two names that differ
// only in case. It asks the filesystem rather than guessing from
// runtime.GOOS, because a Linux host can mount a case-folding volume and a
// Mac can format a case-sensitive one.
func CaseSensitiveDir(t testing.TB, dir string) bool {
	t.Helper()
	probe := filepath.Join(dir, ".case-probe")
	if err := os.WriteFile(probe, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Remove(probe) }()
	_, err := os.Stat(filepath.Join(dir, ".CASE-PROBE"))
	return err != nil
}
