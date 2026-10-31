package fileutil

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/mas-ony/go-toolkit/internal/testutil"
)

// A writable directory passes, and the probes leave nothing behind.
func TestCheckDirWritable(t *testing.T) {
	dir := t.TempDir()
	if err := CheckDir(dir); err != nil {
		t.Fatalf("CheckDir: %v", err)
	}
	left, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Errorf("probes left behind: %v", left)
	}
}

// A path that is missing or is a file fails, naming the path, and nothing
// is created in its place.
func TestCheckDirNotADirectory(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing")
	if err := CheckDir(missing); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("CheckDir(missing) = %v, want fs.ErrNotExist", err)
	}
	if _, err := os.Stat(missing); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("CheckDir created %s", missing)
	}

	file := testutil.WriteFile(t, filepath.Join(dir, "file"), nil)
	err := CheckDir(file)
	var pe *fs.PathError
	if !errors.As(err, &pe) || pe.Path != file ||
		!errors.Is(err, errNotDir) {
		t.Errorf("CheckDir(file) = %v, want a PathError naming it", err)
	}
}

// A directory this process cannot write to fails. Root writes anywhere,
// so the case cannot be staged under it.
func TestCheckDirReadOnly(t *testing.T) {
	testutil.RequireUnprivileged(t)
	dir := t.TempDir()
	testutil.Chmod(t, dir, 0o555)

	if err := CheckDir(dir); !errors.Is(err, fs.ErrPermission) {
		t.Errorf("CheckDir(read-only) = %v, want fs.ErrPermission", err)
	}
}
