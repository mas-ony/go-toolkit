// Package document indexes a directory of documents and resolves a loose
// reference to exactly one file inside it.
//
// # The problem
//
// A reference that names a document without naming a file — a bare stem
// like "1" or "35.002", where the file on disk is "1.pdf", "1.PDF", or
// "1.jpg" — cannot be resolved by opening it. Resolving it by probing
// costs one stat per candidate extension per reference, and still gets
// case wrong whenever the documents were produced on Windows, where
// "1.PDF" and "1.pdf" name the same file, and are read on Linux, where
// they do not. Reading the directory once and lowercasing every name
// makes the lookup exact, case-insensitive, and independent of how many
// extensions are worth trying.
//
// # Ambiguity is an error, not a choice
//
// A reference that matches several files returns ErrAmbiguous naming all
// of them. This package will not pick between "1.pdf" and "1.jpg".
// Probing in extension order would silently prefer whichever extension
// happened to be checked first; the index sees both and says so, leaving
// the caller's domain rules to decide whether that is a duplicate to
// clean up or a reference less unique than its source assumed.
//
// Naming the file outright usually settles it — full filenames are
// matched before stems, so "1.pdf" resolves where "1" would not — but it
// is not a way around the rule. A recursive index over an archive
// foldered by year holds two different documents both called "1.pdf", and
// returning whichever the walk reached first would be the same arbitrary
// answer arrived at through a more specific key.
//
// # An Index is a snapshot, not a view
//
// NewIndex walks the directory once and never looks again. Nothing
// watches the tree and nothing re-stats it, so the two ways it can go out
// of date go different ways:
//
//   - A file added after the walk is invisible. Every reference to it is
//     ErrNotFound, and no error says the index is stale.
//   - A file removed after the walk is still indexed. Lookup resolves it
//     to a path, and Load fails at the read with the operating system's
//     own error rather than with ErrNotFound.
//
// That is the right trade for what this is for — an archive somebody
// assembled, read in one pass by a job that imports it — and the wrong
// one for a directory being written to underneath the run. A long-lived
// process serving a live upload directory should build a new Index rather
// than hold one, since building it is one walk and holding it is a
// growing lie.
//
// The same assumption explains a check that is missing: a file is sized
// from the stat taken before the read, and a file that grows in between
// is not re-checked.
//
// # Policy comes from fileutil
//
// Which extensions count is fileutil.HasAllowedExt, so a file the storage
// layer would refuse is never indexed and never returned. Resolved names
// are run through fileutil.SafeName and capped at fileutil.MaxNameLen, so
// a name too long for the column that will hold it fails here, before the
// file is read, rather than as a truncation error from the driver after
// the bytes have already been written.
//
// The size cap is taken from the same place, for the same reason. A file
// over fileutil.MaxFileBytes is one the storage layer will refuse, so
// reading it here buys a multi-megabyte allocation and an upload that
// exists only to be rejected at the far end. It is checked from the stat,
// before the read, so an archive holding one enormous file does not cost
// the memory to discover that.
//
// Both limits are read through fileutil's helpers rather than compared
// against directly, because a zero or negative cap means NO cap in that
// package: comparing against the bare number would refuse every file in a
// deployment that had disabled a limit.
//
// # What this is not
//
// It is not a containment boundary. A symlink named like a document is
// indexed by its own name and read through, wherever it points, because
// WalkDir does not follow links as directories but has no opinion about
// one that looks like a file. Do not index a tree somebody else can write
// into.
//
// It is not a reader for arbitrary paths either, though ReadFile looks
// like one. The extension gate and the size cap apply there too: a path
// states where a file is, not what it is, and one that turns out to name
// a disk image should be refused rather than read into memory.
//
// # Concurrency
//
// An Index is read-only once NewIndex returns, so Lookup, Load, Count and
// Skipped are safe to call concurrently from any number of goroutines.
// Nothing in it is mutated afterwards and no lock is needed. The build
// itself is single-goroutine and the zero value is not usable.
//
// # What the tests hold in place
//
// The unit suite covers the resolution rules against a temporary
// directory: the three Lookup passes and their ordering, the dotted
// reference that must not be stripped, case-insensitivity, ambiguity
// reported by path relative to the root, the dotfile, dot-directory and
// extension skips, recursion, and the name-length boundary measured in
// bytes rather than runes.
//
// document_integration_test.go covers what a temporary directory cannot
// promise, because it is a property of the filesystem, the platform and the
// process rather than of this code: whether the filesystem distinguishes
// "1.pdf" from "1.PDF" at all, what happens to a directory the process is
// not allowed to open, how a symlink named like a document and a symlinked
// directory are treated, and the snapshot contract above, which needs the
// tree to change after the walk. Each is skipped rather than failed where
// the environment cannot produce it — a case-insensitive filesystem cannot
// hold both spellings, a process running as root can read a directory with
// no permission bits set, and not every platform allows a symlink:
//
//	go test -tags integration -run Integration ./document
//
// It needs no server and no configuration.
package document
