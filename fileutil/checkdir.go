package fileutil

import (
	"fmt"
	"io/fs"
	"os"
)

// CheckDir reports whether uploadDir can hold uploads: it has to be an
// existing directory in which this process can create a record's
// subdirectory and a file. It is a startup check, for main to run on every
// configured upload directory before serving, so a missing mount or a
// read-only volume stops the process with the directory's name rather
// than failing the first upload with a 500.
//
// The probes are real: a directory and a file whose names start with
// ".probe-", each removed again before CheckDir returns. A probe left
// behind by a crash between the two steps is harmless, and no record's
// directory can collide with one, since a record's is named for its id.
//
// A directory that is not there is not created. Creating it would hide a
// missing volume behind an empty directory on the container's own disk,
// where uploads would vanish with the container.
func CheckDir(uploadDir string) error {
	info, err := os.Stat(uploadDir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return &fs.PathError{Op: "check", Path: uploadDir, Err: errNotDir}
	}

	probeDir, err := os.MkdirTemp(uploadDir, ".probe-")
	if err != nil {
		return fmt.Errorf("%s: cannot create a directory: %w",
			uploadDir, err)
	}
	if err := os.Remove(probeDir); err != nil {
		return fmt.Errorf("%s: removing the probe directory: %w",
			uploadDir, err)
	}

	probeFile, err := os.CreateTemp(uploadDir, ".probe-*")
	if err != nil {
		return fmt.Errorf("%s: cannot create a file: %w", uploadDir, err)
	}
	err = probeFile.Close()
	if removeErr := os.Remove(probeFile.Name()); err == nil {
		err = removeErr
	}
	if err != nil {
		return fmt.Errorf("%s: removing the probe file: %w",
			uploadDir, err)
	}
	return nil
}
