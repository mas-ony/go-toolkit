package document

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/mas-ony/go-toolkit/fileutil"
)

// An Index is a one-pass index of a directory, keyed for the two ways a
// document is usually referred to: by full filename and by stem.
//
// The zero value is not usable; build one with NewIndex. An Index is read-only
// once built, so Lookup, Load, Count and Skipped are safe to call from any
// number of goroutines at once.
type Index struct {
	// root is the directory that was walked. It is kept only so an ambiguity
	// can be reported as a path relative to it — see resolve.
	root string

	// byName maps the lowercased full filename ("1.pdf") to every path
	// carrying it, which is more than one when a recursive walk finds the
	// same name in two subdirectories.
	byName map[string][]string

	// byStem maps the lowercased name without extension ("1") to every path
	// sharing that stem, so a collision can be reported rather than guessed.
	byStem map[string][]string

	// files counts every indexed file. Neither map length is that number: both
	// collapse several paths onto one key.
	files int

	// skipped counts files ignored for carrying an extension fileutil does not
	// accept. Worth surfacing to an operator: a directory of .PDF_ or .bak
	// files indexes as empty, and reporting the skip count once says so
	// immediately rather than leaving every later reference to report a miss.
	//
	// Dotfiles are NOT counted here, because they are dropped a step earlier
	// as not-documents, and neither is anything inside a skipped
	// dot-directory. A directory holding only editor swap files, macOS ._
	// forks, or fileutil's own ".tmp-*" leftovers from an interrupted upload
	// therefore indexes as zero files AND zero skips — the one shape this
	// counter cannot explain, and the reason it is reported alongside Count
	// rather than instead of it.
	skipped int
}

// Errors returned by Index.Lookup and by Load. They are sentinels rather than
// formatted strings so a caller can decide per-case whether a miss is a
// warning or a failure, without matching on message text.
var (
	// ErrNotFound means the reference names a document the directory does not
	// contain, under any accepted extension and in either case.
	ErrNotFound = errors.New("no matching file in the document directory")

	// ErrAmbiguous means the reference matched more than one file — most
	// often the same stem stored as both a PDF and a JPEG. The wrapped
	// message names the candidates.
	ErrAmbiguous = errors.New("reference matches more than one file")
)

// resolve returns the one path in paths, or ErrAmbiguous naming all of them.
//
// Candidates are named by their path RELATIVE TO THE INDEXED ROOT rather
// than by basename. The two are the same string in a flat directory, which
// is the usual case; they differ exactly when a recursive walk found one
// name twice, and that is the case where a basename says nothing —
// "1.pdf, 1.pdf" tells an operator there is a problem and nothing about
// where. Rel can only fail for a path outside root, which a walk of root
// cannot produce, so the basename stands as a fallback that should never be
// reached.
//
// An empty slice is likewise unreachable — both maps are built by append,
// so a present key has at least one path — but it is answered rather than
// fallen through, because falling through produces an ErrAmbiguous naming
// no candidates at all, which is the least useful sentence this package
// could hand an operator.
func (i *Index) resolve(paths []string) (string, error) {
	switch len(paths) {
	case 0:
		return "", ErrNotFound
	case 1:
		return paths[0], nil
	}
	names := make([]string, 0, len(paths))
	for _, p := range paths {
		name := filepath.Base(p)
		if rel, err := filepath.Rel(i.root, p); err == nil {
			name = rel
		}
		names = append(names, name)
	}
	slices.Sort(names)
	return "", fmt.Errorf("%w: %s", ErrAmbiguous, strings.Join(names, ", "))
}

// read is the tail shared by Load and ReadFile: name the file, check the name
// against the column width, read it, and refuse an empty one.
//
// The extension gate is deliberately not here. Load's candidates passed it at
// index time and re-checking would be dead work; ReadFile's did not and must
// be checked before the stat result is trusted as a document.
//
// Both limit checks go through fileutil's own helpers rather than comparing
// against MaxNameLen and MaxFileBytes directly, because a zero or negative
// cap means NO cap in that package — a caller that disables either limit
// would otherwise have every file here refused for exceeding nothing. Both
// MESSAGES go through fileutil's labels for the matching reason:
// MaxNameLabel names the unit, and a message that printed the bare number
// would say "characters" to whoever read it while the check counted bytes.
//
// The size is taken from a stat rather than from len(data), so an oversized
// file is refused without being read. That costs one extra stat on the
// ReadFile path, which has already stat'd to reject a directory; the
// alternative is passing the FileInfo down and coupling the two functions for
// one syscall per file.
//
// The stat reports an int64 and TooLarge takes an int, which is 32 bits on a
// 32-bit target, where a size past math.MaxInt would wrap into a small
// positive number and pass. Such a size is refused first, on its own terms:
// nothing that large fits in a []byte there, whatever the configured cap.
// On a 64-bit target the first check can never fire.
func read(path string) (name string, data []byte, err error) {
	name = fileutil.SafeName(filepath.Base(path))
	if fileutil.NameTooLong(name) {
		return "", nil, fmt.Errorf(
			"filename %q is %d bytes, over the %s limit",
			name,
			len(name),
			fileutil.MaxNameLabel())
	}

	info, err := os.Stat(path)
	if err != nil {
		return "", nil, fmt.Errorf("reading %s: %w", path, err)
	}
	size := info.Size()
	if size > math.MaxInt {
		return "", nil, fmt.Errorf(
			"%s is %d bytes, too large to read into memory here",
			path,
			size)
	}
	if fileutil.TooLarge(int(size)) {
		return "", nil, fmt.Errorf(
			"%s is %d bytes, over the %s limit",
			path,
			size,
			fileutil.MaxFileLabel())
	}

	data, err = os.ReadFile(path)
	if err != nil {
		return "", nil, fmt.Errorf("reading %s: %w", path, err)
	}
	if len(data) == 0 {
		return "", nil, fmt.Errorf("%s is empty", path)
	}
	return name, data, nil
}

// NewIndex walks dir and indexes every file with an accepted extension.
//
// recursive controls whether subdirectories are descended. Flat is the usual
// case — one directory of documents named for whatever references them.
// Recursive exists for archives organised into folders by year or region,
// where the stems are still unique but the files are spread out.
//
// Dotfiles are skipped: editor swap files and macOS ._ resource forks are not
// documents. In a recursive walk, directories whose names start with a dot are
// skipped too, whole, since what lives in one — a trash folder, a NAS's
// thumbnails — is a copy of a document rather than the document, and one that
// is still there after the original is gone. A subdirectory that cannot be
// read is skipped rather than aborting the walk, on the reasoning that one
// unreadable folder should not fail a run whose files are all elsewhere; the
// references it would have served come back as ErrNotFound, which is visible
// in the caller's report. The ROOT is the exception and fails the call — see
// the walk below.
//
// Symlinks are not followed as directories, because WalkDir does not follow
// them, but a symlink NAMED like a document is indexed and Load reads
// through it. The gate is the link's own name, so a link called "1.pdf"
// pointing anywhere the process can read resolves and is stored. That is
// acceptable for a directory an operator assembled and pointed this tool at,
// and it is not a containment boundary: do not index a tree somebody else
// can write into.
func NewIndex(dir string, recursive bool) (*Index, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("document directory %q: %w", dir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("document directory %q is not a directory", dir)
	}

	idx := &Index{
		root:   dir,
		byName: make(map[string][]string),
		byStem: make(map[string][]string),
	}

	walk := func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// The root is the exception to the skip-and-continue rule, and
			// it has to be, because the rule's justification does not reach
			// it. A subdirectory that cannot be read costs the references it
			// would have served, which come back as ErrNotFound and are
			// visible one by one in the caller's report. A root that cannot
			// be read costs EVERY reference — and the index it produces is a
			// successful one reporting zero files, which reads as an empty
			// archive rather than as a directory this process is not allowed
			// to open. os.Stat above does not catch it: stat succeeds on a
			// directory whose contents are unreadable.
			//
			// d is nil when WalkDir's own Lstat of the root failed, so the
			// path test comes first. WalkDir passes the root string through
			// unchanged, which is what makes the comparison exact.
			if path == dir {
				return err
			}
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			// A dot-directory is skipped for the reason a dotfile is: it
			// holds what tools keep beside documents — a trash folder, a
			// NAS's thumbnails, a repository's metadata — and a document
			// resolved from one is one somebody deleted or never filed. The
			// root is exempt, since the caller named it.
			if path != dir &&
				(!recursive || strings.HasPrefix(d.Name(), ".")) {
				return fs.SkipDir
			}
			return nil
		}
		name := d.Name()
		if strings.HasPrefix(name, ".") {
			return nil
		}
		if !fileutil.HasAllowedExt(name) {
			idx.skipped++
			return nil
		}

		lower := strings.ToLower(name)
		stem := strings.TrimSuffix(lower, strings.ToLower(filepath.Ext(name)))

		idx.byName[lower] = append(idx.byName[lower], path)
		idx.byStem[stem] = append(idx.byStem[stem], path)
		idx.files++
		return nil
	}

	if err := filepath.WalkDir(dir, walk); err != nil {
		return nil, fmt.Errorf("reading document directory %q: %w", dir, err)
	}
	return idx, nil
}

// Count returns the number of indexed files. Files sharing a stem are counted
// separately, so Count can exceed the number of resolvable references.
func (i *Index) Count() int { return i.files }

// Skipped returns the number of files ignored for carrying an extension
// fileutil does not accept.
func (i *Index) Skipped() int { return i.skipped }

// Lookup resolves a reference to a single file path.
//
// The reference is matched in three passes, most specific first:
//
//  1. As a complete filename, which is the specific case: "35.002.pdf"
//     resolves even when "35.002" alone would be ambiguous.
//  2. As a stem, which is what a bare reference like "1" is.
//  3. As a stem again, after stripping a trailing extension, for a reference
//     naming an extension the directory does not use — "1.pdf" against a
//     re-scanned "1.tif".
//
// The third pass fires only when that trailing run is an extension fileutil
// accepts, because a reference may itself be dotted — an identifier like
// "35.002" — and stripping it down to "35" would resolve to a file that has
// nothing to do with it.
//
// The third pass can report an ambiguity in terms the reference never used.
// "1.pdf" against a directory holding "1.tif" and "1.jpg" strips to the stem,
// finds two, and names both — neither of which is a PDF. That is the right
// answer and a confusing one to read, so a caller surfacing ErrAmbiguous
// should print the reference alongside the candidates.
//
// An empty or whitespace-only reference is ErrNotFound rather than a distinct
// error: to a caller iterating rows, a blank cell and a missing file are the
// same outcome, and separating them only moves the check.
func (i *Index) Lookup(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", ErrNotFound
	}
	lower := strings.ToLower(ref)

	if paths, ok := i.byName[lower]; ok {
		return i.resolve(paths)
	}

	paths, ok := i.byStem[lower]
	if !ok {
		// The reference may carry an extension the directory does not use.
		// A reference to "1.pdf" against a directory holding "1.PDF" is
		// already caught by the case-insensitive byName map above, but one
		// to "1.pdf" against a re-scanned "1.tif" is not. Falling back to
		// the stem recovers it.
		//
		// The fallback is gated on the trailing run being an ACCEPTED
		// extension, and the gate is the whole safety of the thing.
		// filepath.Ext knows only about the last dot, and a dotted
		// identifier looks the same to it: it reads "35.002" as a stem of
		// "35" and an extension of ".002". Ungated, a reference to "35.002"
		// against a directory holding "35.pdf" and no 35.002 at all would
		// strip the identifier down to "35" and return that file — the
		// arbitrary-document outcome the ambiguity rule above exists to
		// refuse, arrived at through the back door and without even an
		// error to show for it. ".pdf" is in fileutil's table and ".002" is
		// not, so the gate separates the case this fallback is for from the
		// case it breaks.
		if fileutil.HasAllowedExt(ref) {
			ext := strings.ToLower(filepath.Ext(ref))
			if stem := strings.TrimSuffix(lower, ext); stem != lower {
				paths, ok = i.byStem[stem]
			}
		}
		if !ok {
			return "", ErrNotFound
		}
	}
	return i.resolve(paths)
}

// Load resolves a reference and reads the file it names.
//
// The returned name is the file's own basename, not the reference: the
// extension is what tells a download path and a browser what the bytes are,
// and a bare stem carries none. It is passed through fileutil.SafeName and
// capped at fileutil.MaxNameLen.
//
// An empty file is an error. A zero-byte document is never a document, and
// storing one produces a row that points at nothing downloadable. So is one
// over fileutil.MaxFileBytes, which is refused from the stat rather than read
// and then rejected.
func (i *Index) Load(ref string) (name string, data []byte, err error) {
	path, err := i.Lookup(ref)
	if err != nil {
		return "", nil, err
	}
	return read(path)
}

// ReadFile reads a document from an explicit path, for callers that already
// know the file outright — a hyperlink target, say — rather than searching
// for it. No Index is needed.
//
// The extension gate still applies, and so does the size cap. A path is only a
// statement about where a file is, not about what it is, and one that turns
// out to point at a spreadsheet, an executable, or a two-gigabyte disk image
// should be refused here rather than read into memory and pushed at a service
// that will refuse it anyway.
func ReadFile(path string) (name string, data []byte, err error) {
	if path == "" {
		return "", nil, ErrNotFound
	}

	info, err := os.Stat(path)
	if err != nil {
		return "", nil, fmt.Errorf("reading %s: %w", path, err)
	}
	if info.IsDir() {
		return "", nil, fmt.Errorf("%s is a directory", path)
	}
	if !fileutil.HasAllowedExt(filepath.Base(path)) {
		return "", nil, fmt.Errorf("%q is not an accepted file type (%s)",
			filepath.Base(path), strings.Join(fileutil.AllowedExts(), " "))
	}
	return read(path)
}
