package fileutil

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

// plainText is what Go's sniffer reports for any file it recognises as text
// and has no closer signature for. It is a containerSniffs value, never an
// entry in allowedTypes; see the warning on OfficeExts.
const plainText = "text/plain; charset=utf-8"

// maxCopySuffix bounds the search for a free name in CopyUnique. A directory
// holding ten thousand copies of one document is a runaway loop somewhere
// else, and failing with a message beats spinning inside a syscall.
const maxCopySuffix = 10000

// The zip-backed office media types, named rather than written inline because
// containerSniffs has to key on exactly these values and a second copy of a
// seventy-character string is a typo waiting to become a silent
// application/octet-stream.
const (
	docxType = "application/vnd.openxmlformats-officedocument." +
		"wordprocessingml.document"
	xlsxType = "application/vnd.openxmlformats-officedocument." +
		"spreadsheetml.sheet"
	pptxType = "application/vnd.openxmlformats-officedocument." +
		"presentationml.presentation"
	odtType = "application/vnd.oasis.opendocument.text"
	odsType = "application/vnd.oasis.opendocument.spreadsheet"
	rtfType = "application/rtf"
)

// Permissions used for created directories and files.
//
//   - Directory 0o755 (rwxr-xr-x) — traversable by the account serving HTTP,
//     which is not necessarily the account that wrote the file.
//   - File      0o644 (rw-r--r--) — readable by that same account; writable
//     only by the owner.
const (
	dirPerm  os.FileMode = 0o755
	filePerm os.FileMode = 0o644
)

// FallbackName is what SafeName returns for input that reduces to nothing
// usable. It is a named constant rather than a literal because Remove has to
// be able to tell a SUBSTITUTED fallback from a file the caller genuinely
// stored under this name; see the guard there.
//
// The word is arbitrary and safe to change; it only has to be a legal single
// path element. Changing it does not strand existing files: a row naming the
// OLD fallback still resolves through Path to the file on disk, and Remove's
// guard compares against the current value, so that file stays removable.
// What changes is only what a NEW unusable name folds onto.
//
// It carries no extension, deliberately. HasAllowedExt therefore rejects it,
// so a file that arrived with no usable name cannot pass the extension gate
// on a later round trip — which is the right answer, since nothing known
// about it says what it is.
//
// A caller may genuinely store a file under this exact name; that is what
// Remove's guard is for, and why the word being a common one costs nothing.
const FallbackName = "unnamed"

// errNotDir is the cause carried by the fs.PathError mkdirAll returns for a
// path that exists as something other than a directory. It is unexported
// because the shape is what callers match on (errors.As for *fs.PathError)
// and a second exported sentinel for a state os.MkdirAll already models would
// be API surface for nothing.
var errNotDir = errors.New("not a directory")

// containerSniffs records the coarser type Go's sniffer reports for formats
// whose bytes it can only classify one layer too shallow: a ZIP with a
// manifest inside, or text with a markup convention inside. This is the one
// exception to "recognised but not allowed means refuse to repeat the claim",
// because the sniff is not WRONG about a .docx, and only the extension can
// say which format the container holds.
//
// The map is keyed by the accepted content type rather than by the extension
// so the exception cannot be applied to a name whose registered format is not
// actually container-backed. A .pdf full of ZIP bytes still collapses to
// generic, because containerSniffs has no entry for application/pdf.
//
// rtfType is keyed to plain text rather than to a zip. Without the entry a
// genuine .rtf sniffs as text/plain, which is recognised and not accepted, so
// it collapses to application/octet-stream — the exact half-registration the
// OfficeExts note warns about for .csv. Keying on rtfType keeps it narrow:
// plain text is not admitted to allowedTypes, so no other extension gains
// anything, and HTML named .rtf sniffs as text/html and still collapses.
//
// The OLE2-based .doc/.xls/.ppt need no entry: Go has no OLE2 signature, so
// they sniff as application/octet-stream and reach the extension table by the
// ordinary route.
var containerSniffs = map[string]string{
	docxType: "application/zip",
	xlsxType: "application/zip",
	pptxType: "application/zip",
	odtType:  "application/zip",
	odsType:  "application/zip",
	rtfType:  plainText,
}

// Accepted file types.
//
// The default table is the conservative one, scans and photographs, because a
// package that silently accepts an .xlsx everywhere it is imported is worse
// than one a caller has to widen on purpose. Callers that handle office
// documents call RegisterExts(OfficeExts) at startup.
//
// The mutex is for correctness under the race detector, not contention:
// registration happens once at startup and the reads happen once per upload.
var (
	extMu    sync.RWMutex
	extTypes = map[string]string{
		".pdf":  "application/pdf",
		".jpg":  "image/jpeg",
		".jpeg": "image/jpeg",
		".png":  "image/png",
		".gif":  "image/gif",
		".webp": "image/webp",
		".tif":  "image/tiff",
		".tiff": "image/tiff",
		".bmp":  "image/bmp",
	}
	// allowedTypes is the VALUE side of extTypes, used by DetectContentType
	// to decide whether a sniffed type is one the caller is willing to record
	// and later hand back as a Content-Type header.
	//
	// Derived rather than written out so the two can never drift: registering
	// an extension automatically admits its type here.
	allowedTypes = deriveAllowed(extTypes)
)

// Upload limits.
//
// They are variables rather than constants because the right numbers differ
// per caller (the name cap in particular mirrors a database column width this
// package cannot see) and because several packages that cannot import each
// other may all need to enforce the same limit.
//
// Assign them in main or an init before serving starts. They are read without
// synchronisation on the hot path, so changing one while requests are in
// flight is a data race.
var (
	// MaxFileBytes caps a single uploaded file. Zero or negative disables
	// the cap.
	//
	// It is deliberately NOT derived from the HTTP framework's body limit,
	// and that limit is not derived from it. A body limit bounds the whole
	// multipart envelope (boundaries, part headers, the filename, any other
	// fields), so a per-file limit computed from it would shift with the
	// length of the filename and stop being a number anyone can publish.
	// This one is exact: an API can promise it, MaxFileLabel prints it, and
	// a handler can answer with it.
	MaxFileBytes = 20 << 20 // 20 MiB

	// MaxNameLen caps a stored filename, mirroring the width of the column
	// the name is written to, so an over-long name fails with a clear
	// message instead of a driver truncation error after the file has
	// already landed on disk. Zero or negative disables the cap.
	//
	// # This number is half of a pair
	//
	// It is only safe because the column is at least this wide. Raising it
	// without widening the column moves the failure from a clean 400 to a
	// truncation error AFTER the file is on disk, which is the exact outcome
	// the limit exists to prevent. So the two move together, in that order:
	// migrate first, then raise this. Lowering it is not a code change alone
	// either: names already stored would exceed the new cap and could not be
	// re-uploaded.
	//
	// # The unit is BYTES, and that is load-bearing
	//
	// Engines differ on how they size a VARCHAR(n). Some count storage bytes
	// under the column's collation codepage, where a rune count is
	// permissive: eighty runes of multi-byte UTF-8 is a hundred and sixty
	// bytes and overflows a column that accepted the count. Others count
	// characters, so the same declaration holds n of them.
	//
	// Go's len() on a UTF-8 string is never smaller than the codepage
	// encoding of the same text, so a byte check is exact against the first
	// kind and merely strict against the second. Nothing here needs to know
	// which engine it is talking to, which is why this is one number rather
	// than two.
	//
	// Use NameTooLong rather than comparing by hand; the rule is easy to get
	// wrong in the permissive direction.
	MaxNameLen = 100
)

// OfficeExts is a ready-made set for services that store correspondence
// rather than scans. Pass it to RegisterExts; it is additive, so the image and
// PDF defaults remain accepted.
//
// Treat it as READ-ONLY. It is an exported map, so it is writable, and
// RegisterExts copies the entries it is handed rather than the map itself, so
// adding to this one is a way to change what the package accepts that leaves
// no RegisterExts call for anybody to find. Build a fresh map and register
// that instead.
//
// Deliberately absent: .txt and .csv. Go's sniffer has no signature for
// either one and reports both as plain text, so registering them takes more
// thought than registering a format with magic bytes, and there are two ways
// to do it that differ in how much they give away.
//
// The narrow one treats plain text as a CONTAINER, which is the same shape as
// a .docx inside a zip: the sniff is not contradicting the name, it is one
// layer too shallow to confirm it, and only the extension can say which text
// format it holds.
//
//	fileutil.RegisterExt(".csv", "text/csv")
//	fileutil.RegisterContainer("text/csv", "text/plain; charset=utf-8")
//
// Both lines are needed. The first alone leaves a genuine CSV sniffing as
// text/plain, which is recognised and not accepted, so it collapses to
// application/octet-stream and the registration achieves nothing beyond
// letting the name through HasAllowedExt. With the second, a real CSV is
// recorded as text/csv, HTML named .csv still collapses to generic, and no
// OTHER extension gains anything.
//
// The wide one registers the sniffed type itself:
//
//	fileutil.RegisterExt(".txt", "text/plain; charset=utf-8")
//
// That admits text/plain into the accepted set, and DetectContentType TRUSTS
// an accepted sniff, so from that point on any file whose bytes are plain
// text is recorded as text/plain whatever it is called, including a .pdf.
// That is honest for a caller that genuinely accepts text files and a silent
// loosening for one that does not, which is why neither is done here.
//
// .rtf is present, and it is the one entry here that needs the narrow
// treatment: RTF is ASCII text, so it sniffs as plain text like a CSV does.
// Its containerSniffs entry, above, is what completes its registration.
var OfficeExts = map[string]string{
	".doc":  "application/msword",
	".docx": docxType,
	".xls":  "application/vnd.ms-excel",
	".xlsx": xlsxType,
	".ppt":  "application/vnd.ms-powerpoint",
	".pptx": pptxType,
	".odt":  odtType,
	".ods":  odsType,
	".rtf":  rtfType,
}

// deriveAllowed returns the set of content types types maps to, which is
// what allowedTypes holds. RegisterExts calls it again after every change,
// so the set cannot fall behind the table.
func deriveAllowed(types map[string]string) map[string]struct{} {
	m := make(map[string]struct{}, len(types))
	for _, ct := range types {
		m[ct] = struct{}{}
	}
	return m
}

// humanBytes renders a byte count in binary units, choosing the largest unit
// that divides it exactly so the label stays truthful. A cap of 20 MiB prints
// "20 MiB"; a cap of 512 KiB prints "512 KiB"; an odd number prints as bytes
// rather than rounding into a figure the check does not enforce.
func humanBytes(n int) string {
	switch {
	case n >= 1<<30 && n%(1<<30) == 0:
		return fmt.Sprintf("%d GiB", n>>30)
	case n >= 1<<20 && n%(1<<20) == 0:
		return fmt.Sprintf("%d MiB", n>>20)
	case n >= 1<<10 && n%(1<<10) == 0:
		return fmt.Sprintf("%d KiB", n>>10)
	default:
		return fmt.Sprintf("%d bytes", n)
	}
}

// stripDataURLPrefix removes a leading data-URL header, and only a data-URL
// header. Anything else is returned untouched so a malformed payload fails at
// the decoder with its own error rather than being quietly cut in half.
func stripDataURLPrefix(payload string) string {
	comma := strings.Index(payload, ",")
	if comma == -1 {
		return payload
	}
	head := strings.ToLower(strings.TrimSpace(payload[:comma]))
	if strings.HasPrefix(head, "data:") ||
		strings.HasSuffix(head, ";base64") ||
		head == "base64" {
		return payload[comma+1:]
	}
	return payload
}

// reserveUnique claims a free filename inside dir by creating it exclusively,
// returning the name it managed to claim.
//
// The suffix is inserted before the extension rather than appended after it,
// so a copied ".docx" stays a ".docx". filepath.Ext returns a suffix of the
// name (or ""), so the slice below is always in range.
//
// Each candidate goes back through SafeName, and that is load-bearing. Write
// sanitises whatever name it is handed, so a candidate SafeName would alter
// is reserved under one spelling and stored under another: the reservation
// survives as a zero-byte file nothing will ever claim, and the name the
// caller persists is not the name on disk. The case that reaches it is a
// desired name that is all extension: filepath.Ext(".pdf") is the whole
// string, so base is empty, the second candidate reads " (2).pdf", and the
// leading space is exactly what SafeName trims.
//
// # The suffix can push a legal name over the column width
//
// This is the one place in the package that MANUFACTURES a filename rather
// than sanitising one it was given, so it is the one place where a name that
// fits MaxNameLen can turn into one that does not: " (2)" is four more bytes
// and " (1000)" is seven, so a desired name sitting on the cap crosses it on
// the second copy. CopyUnique's contract is that the caller persists the
// returned name, so an unchecked overflow here lands as a driver truncation
// error AFTER the bytes are on disk, precisely the outcome MaxNameLen exists
// to prevent.
//
// The check is therefore on the CANDIDATE and not on the desired name, and it
// makes CopyUnique stricter than Write, which stores whatever it is handed
// and leaves the cap to the caller. That asymmetry is deliberate: Write's
// name came from a client the handler has already checked, and this one came
// from here.
//
// The name is refused rather than shortened to make room. Shortening produces
// a name the caller did not ask for and cannot predict, for a case that only
// arises when the desired name was already within a few bytes of the limit:
// four for the second copy, eight for the last one maxCopySuffix allows.
func reserveUnique(dir, desired string) (string, error) {
	name := SafeName(desired)
	ext := filepath.Ext(name)
	base := name[:len(name)-len(ext)]

	for n := 1; n <= maxCopySuffix; n++ {
		candidate := name
		if n > 1 {
			candidate = SafeName(fmt.Sprintf("%s (%d)%s", base, n, ext))
		}
		if NameTooLong(candidate) {
			return "", fmt.Errorf(
				"reserving a name for %q in %q: %q is %d bytes, over the "+
					"%s limit",
				name,
				dir,
				candidate,
				len(candidate),
				MaxNameLabel())
		}
		f, err := os.OpenFile(
			filepath.Join(dir, candidate),
			os.O_CREATE|os.O_EXCL|os.O_WRONLY,
			filePerm)
		if err == nil {
			_ = f.Close()
			return candidate, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return "", fmt.Errorf(
				"reserving %q in %q: %w",
				candidate,
				dir,
				err)
		}
	}
	return "", fmt.Errorf(
		"reserving a name for %q in %q: %d variants already exist",
		name,
		dir,
		maxCopySuffix)
}

// removable reports whether name names a file this package is willing to
// delete: anything except a name SafeName folded onto FallbackName from a
// spelling that was not already the fallback itself. See the fallback guard
// note on Remove.
//
// The second half of the test is a trim rather than another SafeName call,
// which makes it narrower than "SafeName substituted" for most routes to the
// fallback: a name reaching it through a directory prefix, a non-whitespace
// control byte, or an invalid UTF-8 byte is refused even though Path would
// resolve it. All three are beyond anything that reaches a name column, and
// the direction of the error is to leave a file rather than to delete the
// wrong one.
//
// It is NOT narrower for a control byte TrimSpace itself removes. A tab or a
// newline around the fallback trims away and the name is accepted, which is
// the right answer rather than a gap, since Path resolves that name to the
// fallback file, so the row does name the file being deleted.
func removable(name string) bool {
	return SafeName(name) != FallbackName ||
		strings.TrimSpace(name) == FallbackName
}

// mkdirAll is os.MkdirAll with the process umask defeated on every directory
// it creates, and no opinion at all about directories that already exist.
//
// os.MkdirAll applies the umask to each level it creates; os.Chmod does not.
// Chmod'ing only the LEAF therefore keeps the promise in the const block only
// under a permissive umask. Under a restrictive one, with an upload root that
// does not exist yet, it produces:
//
//	0700  <uploadDir>          created by MkdirAll, never chmod'd
//	0755  <uploadDir>/<id>
//	0644  <uploadDir>/<id>/<name>
//
// The account serving HTTP still cannot reach the file: the traversal is
// blocked one level higher than the leaf chmod is looking. Walking the chain
// closes that.
//
// Existing directories are left exactly as they are. <uploadDir> is normally
// a mounted volume the operator provisioned, and silently re-permissioning
// someone else's mount is not this package's business; the fix is only for
// directories that would not exist but for this call.
//
// Two paths still reach the Chmod on a directory this call did not create,
// and both start from a Stat that did not answer. The create race is one:
// the loser of a Mkdir widens what the winner made, which the winner is
// about to do anyway. A Stat that failed for a reason other than absence is
// the other, an unsearchable parent being the realistic one, and there the
// Chmod fails for the same reason the Stat did. Neither reaches a directory
// the caller could have seen before the call.
//
// Each Chmod is best-effort for the reason given at the call site. A failure
// there does not invalidate a write that otherwise succeeded.
func mkdirAll(dir string, perm os.FileMode) error {
	if fi, err := os.Stat(dir); err == nil {
		if fi.IsDir() {
			return nil
		}
		// Present but not a directory. A configured upload directory
		// pointing at a regular file is the realistic case.
		//
		// Not os.Mkdir. Mkdir against a path that already exists answers
		// EEXIST, which prints as "file exists" and reads to whoever is
		// holding the log line as "the directory is already there", the one
		// reading that stops the investigation. os.MkdirAll reports ENOTDIR
		// for this exact state, and the same shape is built here: it names
		// the path, which is what makes the misconfiguration diagnosable
		// from one line, and it says what is actually wrong with it.
		return &fs.PathError{Op: "mkdir", Path: dir, Err: errNotDir}
	}

	if parent := filepath.Dir(dir); parent != dir {
		if err := mkdirAll(parent, perm); err != nil {
			return err
		}
	}

	// os.ErrExist rather than a failure: two concurrent uploads for the same
	// record race here, and the loser has nothing to complain about.
	if err := os.Mkdir(dir, perm); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	_ = os.Chmod(dir, perm)
	return nil
}

// HasAllowedExt reports whether name carries one of the accepted extensions.
//
// This is an extension check only, a deliberate first gate rather than the
// whole story. It rejects the obvious mistakes (an .exe, a .zip, a stray
// binary) at the edge of the system before any bytes are written, while
// DetectContentType decides what the file actually is once the content is in
// hand.
func HasAllowedExt(name string) bool {
	extMu.RLock()
	defer extMu.RUnlock()
	_, ok := extTypes[strings.ToLower(filepath.Ext(name))]
	return ok
}

// NameTooLong reports whether name exceeds MaxNameLen.
//
// The comparison is len(name) and never len([]rune(name)), since the rune
// form passes names that then truncate in the driver, and a zero or negative
// cap means no cap at all, which a hand-written `len(name) > MaxNameLen` gets
// backwards by rejecting every name.
func NameTooLong(name string) bool {
	return MaxNameLen > 0 && len(name) > MaxNameLen
}

// TooLarge reports whether a payload of n bytes exceeds MaxFileBytes.
//
// n is an int, so a caller holding an int64 size (a multipart part header, an
// os.FileInfo) converts before calling. On a 32-bit build that conversion
// wraps, and a size that wraps to a small positive number passes a check it
// should fail. Compare against MaxFileBytes on the int64 first where the
// width is in doubt.
func TooLarge(n int) bool {
	return MaxFileBytes > 0 && n > MaxFileBytes
}

// MaxFileLabel renders MaxFileBytes for an error message, derived from the
// current value so the number a client is told cannot drift from the number
// enforced. A hand-written "20 MB" against a 20 MiB cap is a five percent
// lie, and nothing would catch it.
//
// It is a function rather than a package-level string because MaxFileBytes is
// configurable: a string computed at init would describe the default forever.
//
// A DISABLED cap renders as "0 bytes", or as the negative value it was set
// to, and that reads as "nothing is allowed" while meaning the opposite.
// Nothing here prints it in that state, because the label only appears inside
// a TooLarge branch a disabled cap never enters. A caller publishing the
// limit on its own has to special-case it.
func MaxFileLabel() string {
	return humanBytes(MaxFileBytes)
}

// MaxNameLabel renders MaxNameLen for an error message, for the same reason
// and with one addition: it names the UNIT. A message reading "at most 50
// characters" for a limit counted in bytes is a promise the check does not
// keep.
//
// A disabled cap misreads the same way MaxFileLabel's does.
func MaxNameLabel() string {
	return fmt.Sprintf("%d bytes", MaxNameLen)
}

// Dir returns the directory holding the files for one record.
// It does not create the directory or check that it exists.
func Dir(uploadDir string, id int) string {
	return filepath.Join(uploadDir, strconv.Itoa(id))
}

// Path returns the full path of a stored file.
//
// name is passed through SafeName first, so a value that somehow reached the
// database with a directory component in it still resolves inside the
// record's own directory.
func Path(uploadDir string, id int, name string) string {
	return filepath.Join(Dir(uploadDir, id), SafeName(name))
}

// SafeName reduces a supplied filename to a single path element that is safe
// to join onto an upload directory.
//
// filepath.Base strips any directory prefix:
//
//	"../../etc/passwd" → "passwd"   (traversal attempt)
//	"/etc/passwd"      → "passwd"   (absolute path)
//	"scan.pdf"         → "scan.pdf" (normal case, unchanged)
//
// Base never returns an empty string, but it does pass through three values
// that must not be used as filenames, and all three fold onto FallbackName:
//
//   - "."  — returned for empty input. The switch also carries "" itself, as
//     a zero-cost guard for a case Base makes unreachable.
//   - ".." — returned unchanged. filepath.Join would then clean "<dir>/.."
//     down to the parent, and a write against a directory fails with EISDIR.
//     Not a traversal risk (Base reduces "../../x" to "x", and a lone ".."
//     can only collapse one level inside Join), but it has to be caught.
//   - "/"  — returned when the input is only separators; Join would drop it,
//     making the destination the directory itself.
//
// # Control characters
//
// ASCII control bytes are dropped before Base runs, and so is utf8.RuneError.
// Base does not remove them ("a\nb.pdf" has no path separator, so it survives
// intact) and the name goes on to be interpolated into a Content-Disposition
// header by a download handler, which typically strips only the double quote.
// A CR or LF there is a response splitting attempt.
//
// It is reachable: a multipart filename can carry an RFC 5987 ext-value whose
// percent-decoding yields the raw bytes —
//
//	filename*=UTF-8''a%0D%0AX-Injected:%201.pdf
//
// Some HTTP stacks strip newlines at the header layer, which makes this
// defence in depth rather than a live hole under them. But a filename is this
// package's to sanitise, and should not depend on the framework noticing.
//
// # The order of the three steps is what makes this idempotent
//
// Path applies SafeName to a name Write already passed through it, so the two
// calls must agree; if they do not, a file is stored under one name and
// looked up forever after under another. The order is what holds that:
// dropping control bytes must come FIRST, or a dropped byte uncovers
// whitespace a later pass would trim ("\x00 scan.pdf"), and the trim must
// come AFTER Base, or it misses what Base uncovers ("dir/ scan.pdf").
//
// So: drop control bytes, take the base, and trim last. What comes back has
// no separators to expose and no edge whitespace to strip, so running it
// again is a no-op, which is the property Path and Write depend on and the
// one to pin over a hostile corpus of names.
//
// # Dropping rather than substituting, and why RuneError goes too
//
// A substitution would have to be a character; a character can be whitespace
// or a separator, and it can be LONGER than what it replaced. Both are
// reasons to drop.
//
// The length is the one that bites. strings.Map decodes an invalid UTF-8 byte
// to utf8.RuneError, and writing that back puts three bytes where one was
// read. A caller follows the contract on Write (check the name at the edge
// with NameTooLong, store what Write returns) and a name sitting on the cap
// with one invalid byte in it passes the check and comes back two bytes over.
// That is a driver truncation AFTER the file is on disk, reached from inside
// the package that defines the cap. Dropping RuneError closes it at the point
// the growth would happen.
//
// A name genuinely containing U+FFFD loses it too, because strings.Map cannot
// tell the two apart. Nothing is lost that was worth keeping: the character
// means "something was unrepresentable here".
//
// Every step shortens the name or leaves it alone, so the function does as
// well, with one bounded exception: the fallback replaces at most two bytes
// with len(FallbackName). So a name that passed NameTooLong still passes
// afterwards, for any MaxNameLen at or above that length.
func SafeName(name string) string {
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == utf8.RuneError {
			return -1
		}
		return r
	}, name)

	name = strings.TrimSpace(filepath.Base(name))
	switch name {
	case "", ".", "..", string(filepath.Separator):
		return FallbackName
	}
	return name
}

// AllowedExts returns the accepted extensions, sorted, for use in error
// messages and CLI help text so the list never drifts out of sync with the
// table that enforces it.
func AllowedExts() []string {
	extMu.RLock()
	exts := make([]string, 0, len(extTypes))
	for ext := range extTypes {
		exts = append(exts, ext)
	}
	extMu.RUnlock()

	slices.Sort(exts)
	return exts
}

// DecodeBase64File decodes a base64-encoded file payload and returns the raw
// bytes. It is for JSON APIs that carry the file inline; multipart uploads
// arrive as bytes already and go straight to Write.
//
// Accepted inputs:
//
//	SGVsbG8gV29ybGQ=                              plain, padded
//	SGVsbG8gV29ybGQ                               plain, unpadded
//	data:application/pdf;base64,SGVsbG8gV29ybGQ=  data URL
//	SGVsbG8g\nV29ybGQ=                            line-wrapped
//
// The tolerance is not gold-plating. A browser FileReader produces a data
// URL; some clients hand back the value with the prefix already stripped;
// several languages' encoders omit padding or wrap at 76 columns per MIME;
// and base64.StdEncoding.DecodeString rejects the last two outright. Doing
// this once here is what stops each caller from normalising by hand,
// differently.
//
// The data-URL prefix is only stripped when the text before the first comma
// actually looks like one: a bare "data:" scheme or a ";base64" suffix.
// Cutting at the first comma unconditionally would be harmless for valid
// base64, whose alphabet has no comma, and would silently truncate anything
// else, turning a malformed payload into a confusing decode error instead of
// an honest one.
//
// The URL-safe alphabet is detected by the presence of '-' or '_', neither of
// which is legal in the standard alphabet, so no valid standard payload is
// misread.
//
// # Size
//
// The encoded length is checked against MaxFileBytes BEFORE decoding. Four
// base64 characters are three bytes, so the decoded size is known within a
// byte or two from the string alone, and a 400 MiB payload that would be
// rejected after decoding is a 400 MiB allocation the process never needed to
// make. The decoded length is then checked again, because the pre-check is an
// estimate.
func DecodeBase64File(payload string) ([]byte, error) {
	payload = stripDataURLPrefix(payload)
	payload = strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\r', '\n':
			return -1
		}
		return r
	}, payload)

	if payload == "" {
		return nil, errors.New("decoding base64 file: payload is empty")
	}

	if approx := len(payload) / 4 * 3; TooLarge(approx) {
		return nil, fmt.Errorf(
			"decoding base64 file: payload encodes about %d bytes, over "+
				"the %s limit",
			approx,
			MaxFileLabel())
	}

	enc := base64.StdEncoding
	if strings.ContainsAny(payload, "-_") {
		enc = base64.URLEncoding
	}
	if len(payload)%4 != 0 {
		enc = enc.WithPadding(base64.NoPadding)
	}

	decoded, err := enc.DecodeString(payload)
	if err != nil {
		return nil, fmt.Errorf("decoding base64 file: %w", err)
	}
	if TooLarge(len(decoded)) {
		return nil, fmt.Errorf(
			"decoding base64 file: %d bytes, over the %s limit",
			len(decoded),
			MaxFileLabel())
	}
	return decoded, nil
}

// DetectContentType determines the MIME type of a file from its content,
// falling back to its extension.
//
// Content is preferred over the extension, and over anything the client
// declared, because both are trivially wrong: a browser sends whatever the OS
// associates with the extension, and a file renamed from .jpg to .pdf keeps
// reporting application/pdf all the way into the database and back out as a
// Content-Type header the browser then honours.
//
// http.DetectContentType examines at most the first 512 bytes and always
// returns something, application/octet-stream when it recognises nothing,
// which is where the extension table takes over. data may therefore be a
// prefix rather than the whole file, which is what a caller holding a stream
// it has not buffered should pass. The sniffer also appends a charset to
// text types ("text/plain; charset=utf-8"); that is left intact, since it is
// valid in a Content-Type header and truncating it would lose real
// information.
//
// EMPTY input is the exception, and the reason for the len(data) > 0 guard
// below. http.DetectContentType(nil) returns "text/plain; charset=utf-8", not
// the generic type: the sniffer's text rule is "no disqualifying bytes", and
// no bytes at all trivially satisfies it. Passing that through would classify
// a zero-byte file as recognised-but-not-allowed and collapse it to
// application/octet-stream, so it would never reach the extension table.
// Removing the guard therefore breaks a case Write explicitly supports (it
// stores an empty file rather than rejecting it) and the symptom would be a
// correctly-named .pdf served as a download.
//
// An empty result is never returned: the final fallback is
// application/octet-stream, which is the honest answer for bytes nothing
// recognises. The extension table cannot undercut that, because RegisterExts
// refuses an entry with an empty content type; without that guard the "named"
// branch below would hand one straight back.
//
// # A sniffed type is trusted only if it is an accepted type
//
// Preferring content over extension is right for correctness and wrong as a
// security boundary on its own, because the result is stored and echoed back
// as a Content-Type header. HasAllowedExt gates the NAME; nothing gates the
// sniff. So a file called scan.pdf whose bytes are
//
//	<html><script>alert(document.cookie)</script></html>
//
// passes the extension gate, sniffs as text/html, and is recorded as such,
// and a download handler serving with Content-Disposition: inline turns that
// into a stored cross-site scripting hole against the API's own origin.
//
// The outcomes, in order:
//
//	sniff is in allowedTypes         -> trust it (a real PDF, JPEG, PNG, ...)
//	sniff is this format's container -> trust the extension (see below)
//	sniff recognised but not allowed -> generic; the bytes are not what the
//	                                    name claims, so do not repeat it
//	sniff is generic (unrecognised)  -> fall back to the extension table
//
// The last case is not a weakness, it is what TIFF needs. Go's sniffer has no
// TIFF signature, so a genuine scanner TIFF reports application/octet-stream
// and only the extension can name it.
//
// # The container exception
//
// An OOXML or ODF document IS a ZIP file, and Go sniffs it as one; an RTF is
// ASCII text, and Go sniffs it as text. Without the second rule the third
// would catch both, and every .docx and .rtf stored would be recorded as
// application/octet-stream. That is not the "refuse to repeat a false claim"
// case the rule was written for: the sniff is not contradicting the name, it
// is one layer too shallow to confirm it.
//
// The exception is keyed on the format rather than the extension, so it stays
// narrow: it fires only when the extension's REGISTERED type is one declared
// to live in the sniffed container. A .pdf full of ZIP bytes matches nothing
// and still collapses to generic, and a .docx full of HTML sniffs as
// text/html, which is not its container, so the XSS case is untouched.
//
// One registration would defeat it, and quietly: RegisterExt(".zip",
// "application/zip") puts application/zip into allowedTypes, at which point
// the FIRST rule matches every office document and each one is recorded as a
// zip. Registering an archive type is the decision to look at if a .docx ever
// starts coming back as application/zip. Registering text/plain does the same
// to .rtf, and to a great deal else besides; see OfficeExts.
//
// # The generic type must stay out of the accepted set
//
// The fourth rule has the same weakness in mirror image, and it is easier to
// reach by accident. RegisterExt(".bin", "application/octet-stream") puts the
// generic type into allowedTypes, so the FIRST rule matches every file whose
// bytes nothing recognises and returns generic before the extension table is
// consulted at all. A genuine scanner TIFF would then be recorded as
// application/octet-stream, and the fallback the fourth rule exists for stops
// working for every signature-less format at once, silently, since generic is
// also what that path returns when it fails honestly. Registering the generic
// type is the decision to look at if a .tif stops resolving to image/tiff.
//
// This closes the hole at the point the type is decided, which is the durable
// place for it. It does NOT by itself fix a download path: a static file
// handler may overwrite Content-Type from its own extension table after the
// handler set it. So the value recorded here is authoritative in the database
// and advisory on the wire.
//
// What closes it on the wire is two headers this package cannot set:
// X-Content-Type-Options: nosniff, which stops a browser second-guessing
// whatever Content-Type it was sent, and Content-Disposition: attachment for
// anything that is not being deliberately rendered inline. Neither depends on
// the value below being right, which is the point: they hold even for the
// formats this function has to trust an extension for.
func DetectContentType(name string, data []byte) string {
	const generic = "application/octet-stream"

	sniffed := generic
	if len(data) > 0 {
		sniffed = http.DetectContentType(data)
	}

	extMu.RLock()
	defer extMu.RUnlock()

	// Exact comparison is sufficient: no default value carries parameters,
	// and http.DetectContentType only appends a charset to the text types.
	if _, ok := allowedTypes[sniffed]; ok {
		return sniffed
	}

	ct, named := extTypes[strings.ToLower(filepath.Ext(name))]

	if sniffed != generic {
		// Recognised, and not an accepted type. The one exception is a
		// container sniff for a format that genuinely lives in that
		// container: a .docx really is a ZIP and a .rtf really is text, and
		// refusing the extension here would record both as
		// application/octet-stream.
		if named && containerSniffs[ct] == sniffed {
			return ct
		}
		return generic
	}

	if named {
		return ct
	}
	return generic
}

// RegisterContainer declares that files of contentType legitimately sniff as
// the coarser sniffedAs, so DetectContentType may refine the sniff using the
// extension instead of collapsing it to application/octet-stream.
//
// Use it only for container formats: an archive or a text encoding holding a
// known payload. It is not a way to make a mismatched file report whatever
// its name claims.
//
// Both values are trimmed, and a pair with either one empty is skipped, as
// RegisterExts does with its own. They are compared exactly, against the
// type registered for an extension and against the sniff, so a stray space
// would make the declaration match nothing, and nothing would say so.
func RegisterContainer(contentType, sniffedAs string) {
	contentType = strings.TrimSpace(contentType)
	sniffedAs = strings.TrimSpace(sniffedAs)
	if contentType == "" || sniffedAs == "" {
		return
	}
	extMu.Lock()
	defer extMu.Unlock()
	containerSniffs[contentType] = sniffedAs
}

// RegisterExts adds extensions to the accepted set, overwriting any that are
// already present. A leading dot is optional and the extension is lowercased,
// so ".PDF", "pdf" and ".pdf" all name the same entry.
//
// The content type is trimmed, and an entry carrying an empty one is skipped
// rather than stored. Both halves matter because this value is not merely a
// key: DetectContentType RETURNS it, and what it returns is written to a
// column and later handed back as a Content-Type header.
//
//   - Untrimmed, "  image/png  " reaches a response header verbatim, where
//     the leading space makes it a malformed field value rather than a type
//     any client will act on.
//   - Empty, it would make DetectContentType answer "" for a correctly named
//     file, breaking the one promise that function makes unconditionally.
//     application/octet-stream is not substituted, because a registration
//     with no type is a mistake in the caller's table and silently accepting
//     it under a different meaning hides it.
//
// Call it during startup, before serving. It is safe to call concurrently,
// but a registration racing a live upload changes what that upload is allowed
// to be, which is a configuration bug rather than a concurrency one.
func RegisterExts(types map[string]string) {
	extMu.Lock()
	defer extMu.Unlock()
	for ext, ct := range types {
		ext = strings.ToLower(strings.TrimSpace(ext))
		ct = strings.TrimSpace(ct)
		if ext == "" || ct == "" {
			continue
		}
		if !strings.HasPrefix(ext, ".") {
			ext = "." + ext
		}
		extTypes[ext] = ct
	}
	allowedTypes = deriveAllowed(extTypes)
}

// RegisterExt adds a single extension. See RegisterExts.
func RegisterExt(ext, contentType string) {
	RegisterExts(map[string]string{ext: contentType})
}

// Stat reports on a stored file without reading it.
//
// This is what a download path uses to confirm the file the row names is
// actually present. Reading it would be correct but wasteful in a way that
// scales badly: every GET would pull the whole file into memory, discard it,
// and then hand the path to the HTTP layer, which opens and streams the file
// a second time. At a few megabytes per record that is double the I/O and a
// large transient allocation per request, for a question os.Stat answers from
// metadata alone.
//
// The error is returned unwrapped so callers can use
// errors.Is(err, fs.ErrNotExist).
func Stat(uploadDir string, id int, name string) (os.FileInfo, error) {
	return os.Stat(Path(uploadDir, id, name))
}

// Read returns the bytes of a stored file. The error from os.ReadFile is
// returned unwrapped so callers can test it with
// errors.Is(err, fs.ErrNotExist) to distinguish "the row says there is a file
// but there is not" from a genuine I/O failure.
//
// Use Stat instead when only existence matters; see the note there.
func Read(uploadDir string, id int, name string) ([]byte, error) {
	return os.ReadFile(Path(uploadDir, id, name))
}

// Write stores data under one record and returns the filename actually used
// (SafeName applied to name, never a path).
//
// The per-record directory is created on demand, so neither uploadDir nor any
// subdirectory has to be provisioned ahead of time.
//
// Overwrite behaviour: writing over an existing file of the same name is the
// intended outcome for a record that holds a single file, and the caller's
// problem for a record that holds many. Write does NOT invent unique names;
// CopyUnique does. Replacing a file under a DIFFERENT name leaves the old one
// behind, which is why a caller should call Remove for the previous name
// after a successful write.
//
// NEITHER limit is enforced here. Write stores whatever name and however many
// bytes it is handed; NameTooLong and TooLarge are the caller's checks, made
// at the edge where a rejection can still be a 400 instead of a half-finished
// upload. reserveUnique is the one place in this package that checks a name
// itself, and only because it is also the one place that manufactures one.
//
// Checking the name the caller HOLDS and persisting the one Write RETURNS is
// safe because SafeName never lengthens a name past len(FallbackName); see
// the note there.
//
// The file is written to a temporary name in the same directory and then
// renamed into place. Within a single filesystem, rename is atomic: a reader
// arriving mid-write sees either the complete old file or the complete new
// one, never a truncated prefix. Writing directly to the destination would
// leave a half-written file readable, and permanently stored if the process
// died there, under a name the database says is valid.
func Write(
	uploadDir string,
	id int,
	name string,
	data []byte,
) (string, error) {
	safe := SafeName(name)
	dir := Dir(uploadDir, id)
	if err := mkdirAll(dir, dirPerm); err != nil {
		return "", fmt.Errorf("creating upload directory %q: %w", dir, err)
	}
	// Chmod the record directory even when mkdirAll found it already
	// present. mkdirAll leaves existing directories alone because most of
	// them are the operator's, but this one is ours, created by an earlier
	// Write under whatever umask that process happened to carry, so widening
	// it here is what keeps one narrow run from making the record
	// permanently unreadable.
	//
	// Best-effort, like the chmods inside mkdirAll: the directory may exist
	// under different ownership, where chmod fails with EPERM but the write
	// itself can still succeed.
	_ = os.Chmod(dir, dirPerm)

	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return "", fmt.Errorf("creating temp file in %q: %w", dir, err)
	}
	tmpName := tmp.Name()
	// Any failure from here on removes the temp file. The name is CLEARED
	// once the rename has consumed it, rather than left to a Remove that
	// would usually be a no-op: the rename frees the name, os.CreateTemp
	// draws from a random namespace that can hand the same one to a
	// concurrent call, and the no-op would then delete that call's file
	// instead.
	//
	// A crash removes nothing, so a ".tmp-" file can survive in the record
	// directory. A sweep reclaiming it has to match the directory against
	// the stored names and not against the ".tmp-" prefix: SafeName accepts
	// a leading dot, so ".tmp-scan.pdf" is a filename a client can upload,
	// and a prefix match would delete a live file the row still names.
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("writing %q: %w", tmpName, err)
	}
	// Sync before Close so the bytes are on the device, not merely in the
	// page cache, before the rename publishes the name. Without it, a power
	// loss just after the rename can leave a correctly-named file of zeroes.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("syncing %q: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("closing %q: %w", tmpName, err)
	}
	// CreateTemp uses 0o600; widen to the shared read permission the HTTP
	// process needs before publishing the name.
	if err := os.Chmod(tmpName, filePerm); err != nil {
		return "", fmt.Errorf("chmod %q: %w", tmpName, err)
	}

	dest := filepath.Join(dir, safe)
	if err := os.Rename(tmpName, dest); err != nil {
		return "", fmt.Errorf("publishing %q: %w", dest, err)
	}
	tmpName = ""

	// Sync the DIRECTORY, not just the file. tmp.Sync above made the bytes
	// durable; it said nothing about the directory entry the rename created.
	// Without this a power loss can leave the bytes on the device under no
	// name at all, and since the caller updates the database row after Write
	// returns, the surviving state would be a row naming a file that does
	// not exist — the one outcome the ordering rule in the package doc
	// exists to prevent.
	//
	// Best-effort for portability: opening a directory for Sync is not
	// supported everywhere (Windows returns an error), and the failure does
	// not invalidate a write whose bytes are already synced.
	//
	// Only THIS directory is synced, not its parents. A record directory
	// mkdirAll created moments ago has its own entry in <uploadDir> still in
	// the page cache, so a power loss can take the directory and the file
	// inside it together. Closing that would cost an open and an fsync per
	// level on the create path; the exposure left is one entry per record
	// rather than one per write.
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}

	return safe, nil
}

// CopyUnique copies an existing stored file to a NEW file under the same
// record, choosing a filename that does not collide with anything already on
// disk, and returns the stored filename and the number of bytes copied.
//
// srcName is the existing stored filename. desiredName is the preferred name
// for the copy; when it is taken, a numeric suffix is inserted before the
// extension ("report.docx", "report (2).docx", "report (3).docx") until a
// free name is found. The extension is preserved deliberately: a consumer
// that reads a file's format off its extension is handed a name it refuses to
// open if the suffix lands after it.
//
// Unlike Write, which overwrites a same-named file in place, this never
// overwrites, so the source file's bytes are always preserved. The caller
// must persist the returned name and size.
//
// The source is read whole into memory rather than streamed, and MaxFileBytes
// is not consulted: whatever is stored under srcName is copied, including a
// file larger than the current cap. That is a deliberate difference from
// DecodeBase64File, which checks the size precisely to avoid the allocation;
// here the bytes are already stored, and refusing to copy a file still being
// served would be the stranger outcome.
//
// srcName carries no fallback guard, which Remove does have. An empty or
// otherwise unusable srcName resolves through Path to FallbackName, so a
// record that genuinely holds a file under that name has it copied, and the
// call reports success under desiredName. The guard is on Remove because
// deleting the wrong file is unrecoverable and copying it is not, but a
// caller reading a nullable name column should still reject nil before
// calling rather than rely on the read failing.
//
// # The name is claimed atomically
//
// The loop CREATES each candidate with O_CREATE|O_EXCL, which fails if the
// name exists and therefore either claims it or moves on: one syscall, no
// window. A stat-then-write would leave a gap in which a concurrent copy
// under the same record claims the name and the loser's bytes overwrite the
// winner's. Write then renames its temp file over the reservation.
//
// A failure after the reservation removes it, so a copy that RETURNS an error
// does not leave an empty file squatting on a name. A crash between the two
// does: the reservation is a real zero-byte file, nothing sweeps it, and the
// name it holds is taken until an operator deletes it. The next copy under
// that record steps to the following suffix rather than failing, so the cost
// is a gap in the numbering, not a stuck upload.
//
// # It can fail on the name cap where Write would not
//
// The suffix costs four bytes at " (2)" and grows with the digit count, so a
// desired name within four of MaxNameLen fits on the first copy and not on
// the second. That is refused here rather than returned, because the caller
// persists this name and the alternative is a driver truncation error after
// the bytes are on disk. See reserveUnique for why the check is on the
// suffixed candidate.
func CopyUnique(
	uploadDir string,
	id int,
	srcName, desiredName string,
) (string, int, error) {
	src := Path(uploadDir, id, srcName)
	data, err := os.ReadFile(src)
	if err != nil {
		return "", 0, fmt.Errorf("reading source file %q: %w", src, err)
	}

	dir := Dir(uploadDir, id)
	if err := mkdirAll(dir, dirPerm); err != nil {
		return "", 0, fmt.Errorf("creating upload directory %q: %w", dir, err)
	}
	// Widen a record directory an earlier Write left under a narrower umask,
	// best-effort, for the reasons given at the same call in Write. It is
	// repeated here rather than left to the Write below because reserveUnique
	// creates the reservation in this directory first.
	_ = os.Chmod(dir, dirPerm)

	name, err := reserveUnique(dir, desiredName)
	if err != nil {
		return "", 0, err
	}

	// Write renames its temporary over the reservation and returns the name
	// it published. That name is what comes back, rather than the reserved
	// one: reserveUnique normalises each candidate so the two agree, and
	// returning Write's answer makes the agreement a fact the caller can rely
	// on instead of a property two functions have to keep in step.
	stored, err := Write(uploadDir, id, name, data)
	if err != nil {
		_ = os.Remove(filepath.Join(dir, name))
		return "", 0, err
	}
	return stored, len(data), nil
}

// Remove deletes one stored file and then tries to remove the
// now-possibly-empty record directory.
//
// Errors are discarded deliberately, and the discard is written explicitly
// with the blank identifier so it reads as a decision rather than an
// oversight:
//
//  1. Remove is called AFTER the database row or column has already been
//     updated. The operation the caller was asked to perform has succeeded;
//     reporting a file-removal failure would turn a completed request into an
//     error the client cannot act on.
//  2. A file left behind is harmless. Nothing references it, and a sweep
//     comparing <uploadDir>/<id>/ against the stored names can reclaim it
//     later.
//
// The trailing directory removal fails silently when the directory is not
// empty, so calling it unconditionally is safe: it becomes a clean-up only
// once the last file is gone.
//
// # The fallback guard
//
// Path applies SafeName, which maps "", ".", "..", "/" and any all-whitespace
// or all-control-character string onto the single name FallbackName. Without
// a guard, Remove called with any of those would delete the file stored under
// FallbackName, a real file belonging to an upload that arrived without a
// usable filename and one this call was never asked to touch. An empty stored
// name is the realistic route in (a cleared column, a row whose name was
// never set), and the others are one bad migration away.
//
// The condition is deliberately "SafeName SUBSTITUTED the fallback" rather
// than "the name is blank", so a caller that genuinely stored a file under
// FallbackName can still remove it.
func Remove(uploadDir string, id int, name string) {
	if !removable(name) {
		return
	}
	_ = os.Remove(Path(uploadDir, id, name))
	_ = os.Remove(Dir(uploadDir, id))
}

// RemoveFiles deletes several stored files belonging to one record, then
// tries once to remove the now-possibly-empty directory.
//
// names is []*string rather than []string so a nullable name column maps
// straight through: a row with no file attached carries a nil name. Nil
// entries are skipped, so callers can pass the name fields off a slice of
// models without filtering them first.
//
// Each name goes through the same fallback guard as Remove, and errors are
// discarded for the same reasons. The directory removal is attempted once at
// the end rather than after every file: the result is identical (it only
// succeeds when the directory is empty) and it is one syscall instead of n.
func RemoveFiles(uploadDir string, id int, names []*string) {
	dir := Dir(uploadDir, id)
	for _, name := range names {
		if name == nil || !removable(*name) {
			continue
		}
		_ = os.Remove(filepath.Join(dir, SafeName(*name)))
	}
	// Best-effort removal of the now-possibly-empty directory.
	_ = os.Remove(dir)
}

// RemoveDir deletes a record's whole directory and everything in it. Used
// when the record itself is deleted, where individual filenames do not matter
// and any stray file from an interrupted upload should go too.
//
// Errors are discarded for the reasons given on Remove.
func RemoveDir(uploadDir string, id int) {
	_ = os.RemoveAll(Dir(uploadDir, id))
}
