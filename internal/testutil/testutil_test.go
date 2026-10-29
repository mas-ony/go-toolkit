package testutil

// Tests for testutil.go: a variable set for a test comes back when the
// test ends, whether it was set to a new value or only snapshotted; a
// panic arrives as a value; a call that should panic and returns is
// reported by its name; a recording of one entry hands it over and any
// other count fails; a file lands where it was asked to, with its
// directories; a pattern lists the package's own files and fails naming
// itself when it is malformed or lists none; a source file parses with
// its comments, and one that does not parse fails naming its path; a mode
// set for a test comes back when it ends; the filesystem probe leaves
// nothing behind; the root check skips exactly when it should; and the
// fake token passes config's check of a real one.

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mas-ony/go-toolkit/config"
)

// fatal is a testing.TB whose Fatalf records the message and panics,
// which is how a test can watch a helper fail without failing itself:
// CapturePanic catches the panic, and the message is what the helper
// would have reported. Its Errorf records the message and returns, as an
// error a helper reports lets the test go on.
type fatal struct {
	testing.TB
	msg string
}

// counter is the package state the Set tests change.
var counter = 1

// Helper does nothing; the helper under test calls it first.
func (f *fatal) Helper() {}

// Fatalf records the message and panics with f, stopping the helper the
// way the real Fatalf stops a test.
func (f *fatal) Fatalf(format string, args ...any) {
	f.msg = fmt.Sprintf(format, args...)
	panic(f)
}

// Errorf records the message and returns, leaving the helper to go on the
// way the real Errorf leaves a test to.
func (f *fatal) Errorf(format string, args ...any) {
	f.msg = fmt.Sprintf(format, args...)
}

// Set holds the new value for the life of the test and restores the old
// one when it ends, and Set to the current value restores whatever the
// test changed it to.
func TestSet(t *testing.T) {
	t.Run("a new value", func(t *testing.T) {
		Set(t, &counter, 7)
		if counter != 7 {
			t.Errorf("counter = %d inside the test, want 7", counter)
		}
	})
	if counter != 1 {
		t.Errorf("counter = %d after the test, want 1", counter)
	}

	t.Run("a snapshot", func(t *testing.T) {
		Set(t, &counter, counter)
		counter = 9
	})
	if counter != 1 {
		t.Errorf("counter = %d after the snapshot, want 1", counter)
	}
}

// A panic is returned as the value it carried, and a normal return as nil.
func TestCapturePanic(t *testing.T) {
	t.Parallel()
	if got := CapturePanic(func() { panic("boom") }); got != "boom" {
		t.Errorf("CapturePanic = %v, want boom", got)
	}
	if got := CapturePanic(func() {}); got != nil {
		t.Errorf("CapturePanic = %v, want nil", got)
	}
}

// MustPanic says nothing of a call that panics, and reports one that
// returns by the name it was given, without stopping the test.
func TestMustPanic(t *testing.T) {
	t.Parallel()
	f := &fatal{TB: t}
	MustPanic(f, "boom", func() { panic("boom") })
	if f.msg != "" {
		t.Errorf("MustPanic reported a call that panicked: %q", f.msg)
	}
	MustPanic(f, "quiet", func() {})
	if want := "quiet did not panic"; f.msg != want {
		t.Errorf("MustPanic reported %q, want %q", f.msg, want)
	}
}

// Only hands over the one element of a recording, and fails naming the
// count and what was counted for any other length, empty included.
func TestOnly(t *testing.T) {
	t.Parallel()
	if got := Only(t, "requests", []string{"one"}); got != "one" {
		t.Errorf("Only = %q, want one", got)
	}
	for _, items := range [][]string{nil, {"a", "b"}} {
		f := &fatal{TB: t}
		if got := CapturePanic(func() {
			Only(f, "requests", items)
		}); got != f {
			t.Fatalf("Only of %d items did not fail: %v", len(items), got)
		}
		want := fmt.Sprintf("saw %d requests, want 1", len(items))
		if f.msg != want {
			t.Errorf("Only failed with %q, want %q", f.msg, want)
		}
	}
}

// WriteFile creates the directories it needs and returns the path it was
// given; TempFile puts the file in a directory of the test's own.
func TestWriteFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "a", "b", "c.txt")
	if got := WriteFile(t, path, []byte("x")); got != path {
		t.Errorf("WriteFile returned %q, want %q", got, path)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "x" {
		t.Errorf("read back %q, %v", data, err)
	}

	tmp := TempFile(t, "config.yaml", []byte("y"))
	if filepath.Base(tmp) != "config.yaml" {
		t.Errorf("TempFile returned %q, want a config.yaml", tmp)
	}
	if data, err := os.ReadFile(tmp); err != nil || string(data) != "y" {
		t.Errorf("read back %q, %v", data, err)
	}
}

// Glob lists the matching files of the working directory, which go test
// makes this package's own, and fails naming the pattern for one that is
// malformed or matches nothing.
func TestGlob(t *testing.T) {
	t.Parallel()
	got := Glob(t, "testutil*.go")
	want := []string{"testutil.go", "testutil_test.go"}
	if !slices.Equal(got, want) {
		t.Errorf("Glob = %v, want %v", got, want)
	}
	for _, pattern := range []string{"[", "*.nothing"} {
		f := &fatal{TB: t}
		if got := CapturePanic(func() { Glob(f, pattern) }); got != f {
			t.Fatalf("Glob(%q) went on: %v", pattern, got)
		}
		if !strings.Contains(f.msg, strconv.Quote(pattern)) {
			t.Errorf("Glob(%q) failed with %q, want it to name the "+
				"pattern", pattern, f.msg)
		}
	}
}

// ParseFile hands back the file with its comments, at positions the set it
// returns resolves to the path, and fails naming the path for source that
// does not parse.
func TestParseFile(t *testing.T) {
	t.Parallel()
	const src = "package probe\n\n" +
		"// Answer is documented.\n" +
		"const Answer = 42\n"
	path := TempFile(t, "probe.go", []byte(src))
	f, fset := ParseFile(t, path)
	if f.Name.Name != "probe" {
		t.Errorf("package = %q, want probe", f.Name.Name)
	}
	if len(f.Comments) != 1 {
		t.Errorf("parsed %d comments, want the one the source holds",
			len(f.Comments))
	}
	if got := fset.Position(f.Name.Pos()).Filename; got != path {
		t.Errorf("positions name %q, want %q", got, path)
	}

	broken := TempFile(t, "broken.go", []byte("package"))
	ft := &fatal{TB: t}
	if got := CapturePanic(func() { ParseFile(ft, broken) }); got != ft {
		t.Fatalf("ParseFile of source that does not parse went on: %v",
			got)
	}
	if !strings.Contains(ft.msg, broken) {
		t.Errorf("ParseFile failed with %q, want it to name %s",
			ft.msg, broken)
	}
}

// The probe answers and cleans up after itself, whatever the answer is.
func TestCaseSensitiveDirLeavesNothing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	_ = CaseSensitiveDir(t, dir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("the probe left %d entries behind", len(entries))
	}
}

// RequireUnprivileged stops a test run as root and lets any other run on.
func TestRequireUnprivileged(t *testing.T) {
	t.Parallel()
	reached := false
	t.Run("probe", func(t *testing.T) {
		RequireUnprivileged(t)
		reached = true
	})
	if root := os.Geteuid() == 0; reached == root {
		t.Errorf("as uid %d the test went on: %t", os.Geteuid(), reached)
	}
}

// Chmod holds the mode for the life of the test and puts the old one back
// when it ends.
func TestChmod(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are not a mode on Windows")
	}
	dir := filepath.Join(t.TempDir(), "locked")
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	mode := func() os.FileMode {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		return info.Mode().Perm()
	}
	before := mode()

	t.Run("locked", func(t *testing.T) {
		Chmod(t, dir, 0o500)
		if got := mode(); got != 0o500 {
			t.Errorf("mode = %o inside the test, want 500", got)
		}
	})
	if got := mode(); got != before {
		t.Errorf("mode = %o after the test, want %o", got, before)
	}
}

// The fake token passes the check config applies to a real one. The check
// is config's own, asked through Validate, so the token is held to
// whatever config holds a real one to.
func TestBotTokenHasTheShapeOfOne(t *testing.T) {
	t.Parallel()
	c := &config.TelegramConfig{Token: BotToken, Timeout: time.Second}
	if err := c.Validate(); err != nil {
		t.Errorf("BotToken %q fails config's check: %v", BotToken, err)
	}
}
