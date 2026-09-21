package xlsx

import (
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
)

// winAbs matches an absolute Windows path with a drive letter, in either
// separator style: "C:\Data\scan.pdf" or "C:/Data/scan.pdf".
var winAbs = regexp.MustCompile(`^[A-Za-z]:[\\/]`)

// Errors returned by ResolveLink.
var (
	// ErrNoLink means the cell carries no hyperlink.
	ErrNoLink = errors.New("no hyperlink on the cell")

	// ErrNotAFile means the hyperlink points at a web page, a mailbox, or
	// another cell in the workbook rather than at a document on disk.
	//
	// Callers are expected to branch on this one: it is the difference between
	// "this link is not the sort of thing you were looking for, try your other
	// route" and "this link is broken".
	ErrNotAFile = errors.New("hyperlink does not point at a file")
)

// LinkTarget is a hyperlink resolved into something the caller can act on.
type LinkTarget struct {
	// Path is the local filesystem path to try. Empty when the target is not a
	// file at all.
	//
	// A UNC target keeps the two leading separators that make it a share,
	// and POSIX does not read them that way: on a Unix host
	// "//fileserver/scans/1.pdf" is just an absolute path, resolves under
	// /fileserver, and finds nothing. That is the intended outcome —
	// whether the share is mounted is a deployment question and not this
	// function's — but it is why a UNC link never reports a mount error,
	// only a miss, and why Base matters more for this shape than for any
	// other.
	Path string

	// Base is the filename alone. It is the fallback key when Path names a
	// location that exists on the author's machine but not on this one — an
	// absolute C:\ path, or a share that is not mounted here.
	Base string

	// Foreign reports that the target was ABSOLUTE — a drive letter, a UNC
	// share, or a POSIX root — rather than a path relative to the workbook.
	// It is worth telling the operator, because "the hyperlink was unusable"
	// and "the file has been moved" need different fixes.
	//
	// The name fits the first two shapes and overstates the third. A drive
	// letter and a UNC share name a location on the machine that built the
	// workbook and rarely resolve on the reading host; a POSIX root often
	// does, since "/mnt/archive/6.pdf" is an ordinary local path there. So
	// this is a statement about the SHAPE of the target, not a prediction
	// that it will fail, and a caller reporting it should say "absolute"
	// rather than "from another machine".
	//
	// It is NOT a containment check, and false does not mean "inside
	// baseDir". A relative target is joined onto baseDir and may climb out
	// of it — "..\\..\\archive\\1.pdf" is an ordinary link to find in a
	// workbook and resolves above the workbook directory with Foreign
	// false. A caller that needs the stronger property has to check Path
	// itself; what stops a link from naming anything it likes is the
	// extension gate applied when the file is read, not this field.
	Foreign bool
}

// namesNoFile reports whether a filepath.Base result names something other
// than a file.
//
// Base is the key a caller searches on when Path names a location that does
// not exist on this machine, so it has to be a filename. Base answers "." for
// an empty path, ".." for a target that only climbs, and a lone separator for
// one that is only separators; a caller looking through the scan directory for
// any of those is looking for something that cannot be there. A target that
// names no file is ErrNotAFile, which is the answer that sends the caller to
// its other route instead.
//
// ".." is the one a reader is most likely to think unreachable. It is not:
// "..\\..\\archive" is an ordinary shape for a hyperlink, and Base reduces
// every climbing target to it. filepath.Join then cleans the Path down to a
// DIRECTORY, so without this case the caller gets a Path it can stat and a
// Base it can never match.
//
// The separator is spelled out both ways rather than as
// string(filepath.Separator). That conversion is a CONSTANT, so on a Windows
// build it folds to `\` and collides with the literal below — a duplicate
// case, which is a compile error there though not on Linux. Both literals are
// needed because Base returns the PLATFORM separator, and neither can reach
// here as a real filename, since every backslash in the target became a
// forward slash before Base ran.
func namesNoFile(base string) bool {
	switch base {
	case "", ".", "..", "/", `\`:
		return true
	}
	return false
}

// foreignTarget builds the LinkTarget for an absolute slashed path — a UNC
// share, a drive letter, or a POSIX root — refusing one whose last element
// names no file.
//
// It exists so the guard is applied on every route that produces an absolute
// target. resolvePlainPath checks before its own switch and could return
// directly; resolveFileURL cannot, because it reaches three of these shapes
// from u.Host and u.Path without ever computing a base of its own — so
// "file:///.." would otherwise reach a caller as a Base of "..".
//
// target is the raw hyperlink, quoted in the error so the message names what
// the operator wrote rather than the path this function derived from it.
func foreignTarget(slashed, target string) (LinkTarget, error) {
	base := filepath.Base(slashed)
	if namesNoFile(base) {
		return LinkTarget{}, fmt.Errorf("%w: %s", ErrNotAFile, target)
	}
	return LinkTarget{
		Path:    filepath.FromSlash(slashed),
		Base:    base,
		Foreign: true,
	}, nil
}

// resolveFileURL handles the file: scheme, which encodes a local path and a
// UNC share differently:
//
//	file:///C:/Data/scan.pdf   → empty host, path "/C:/Data/scan.pdf"
//	file://fileserver/share/x  → host "fileserver", path "/share/x"
//
// The leading slash before a drive letter is an artefact of the URL grammar
// and has to come off; for the UNC case the host has to go back on as the two
// leading separators.
//
// # Decoding happens exactly once, and url.Parse has already done it
//
// This is the difference between this function and resolvePlainPath, and it is
// the one thing to get right here: url.Parse returns u.Path already
// percent-decoded, so decoding it again would corrupt any name whose own text
// survived the first pass as an escape. The plain-path branch decodes because
// nothing has decoded for it; this one must not.
//
// # url.Parse does not get the last word
//
// Two of its outcomes are recoverable rather than final:
//
//   - A target it REFUSES. It rejects a raw "%" that is not valid encoding,
//     and a hand-written target carrying one — "file:///C:/scan/100% raw.pdf"
//     — still names a real file. The scheme comes off and the remainder goes
//     to the plain-path branch, which tolerates exactly that byte. Failing
//     instead would discard a link over a character the other branch keeps.
//   - A target it parses as OPAQUE. "file:C:/Data/scan.pdf" has no authority
//     slashes, so everything lands in u.Opaque and u.Path is empty. It is
//     rare, but an empty path is the shape this function otherwise reports as
//     ErrNotAFile — which would tell the caller to stop looking for a file
//     that exists.
func resolveFileURL(target, baseDir string) (LinkTarget, error) {
	u, err := url.Parse(target)
	if err != nil {
		// Sliced by length rather than trimmed by prefix. ResolveLink matched
		// the scheme against a LOWERCASED copy, so a target written "FILE:"
		// arrives here with its prefix intact and a case-sensitive TrimPrefix
		// leaves it in place — after which the remainder has no scheme
		// and no leading separator, so the plain-path branch reads it as
		// relative and joins it onto the workbook directory. The result
		// is a Path of "<baseDir>/FILE:/C:/scan/100% raw.pdf" with
		// Foreign false, which is the corruption this recovery branch
		// exists to prevent, applied to exactly the targets it exists to
		// rescue. The prefix is five bytes whatever its case, and the
		// branch above has already established that those five are
		// "file:".
		rest := target[len("file:"):]
		if strings.HasPrefix(rest, "///") {
			// Empty authority. Dropping two of the three slashes leaves the
			// path, and the third comes off in front of a drive letter for
			// the same reason it does on the parsed route. A UNC share —
			// exactly two slashes — is left alone: the pair IS the share.
			rest = rest[2:]
			if winAbs.MatchString(rest[1:]) {
				rest = rest[1:]
			}
		}
		if rest == "" || rest == "/" {
			return LinkTarget{}, fmt.Errorf("%w: %s", ErrNotAFile, target)
		}
		return resolvePlainPath(decodeURI(rest), baseDir)
	}

	// u.Path, not decodeURI(u.Path). A file called "50%2B.pdf" is written by
	// Excel as "50%252B.pdf", which Parse returns correctly as "50%2B.pdf"; a
	// second decode turns it into "50+.pdf" and the lookup misses a file that
	// is sitting right there.
	path := u.Path

	// u.Opaque, in contrast, is the RAW form and has not been decoded.
	if path == "" && u.Opaque != "" {
		return resolvePlainPath(decodeURI(u.Opaque), baseDir)
	}

	// "/" as well as "": url.Parse gives "file:///" a path of "/", so a check
	// for the empty string alone lets an authority-only URL through and
	// returns a LinkTarget naming a root directory rather than a file.
	//
	// It sits ABOVE the host branch because the host branch is what an
	// authority-only URL actually reaches. "file://fileserver" parses to a
	// non-empty host and an empty path, so a guard placed after it never
	// runs, and filepath.Base("") returns "." — which then becomes Base, the
	// key the caller's by-filename recovery searches on. Looking for a file
	// called "." is worse than reporting that this was never a file.
	if path == "" || path == "/" {
		return LinkTarget{}, fmt.Errorf("%w: %s", ErrNotAFile, target)
	}

	if u.Host != "" && u.Host != "localhost" {
		// UNC share. filepath.Join would collapse the two leading separators
		// that make it a UNC path, so it is assembled by hand.
		//
		// An authority carrying a PORT ("file://server:445/share/1.pdf") keeps
		// it, producing a share name nothing resolves. Base is still the
		// filename a caller recovers on, which is the same trade this file
		// makes for a scheme it does not know.
		return foreignTarget("//"+u.Host+path, target)
	}

	trimmed := strings.TrimPrefix(path, "/")
	if winAbs.MatchString(trimmed) {
		return foreignTarget(trimmed, target)
	}
	// An absolute POSIX path in a file URL. Absolute is absolute — it is used
	// as given rather than joined onto the workbook directory.
	return foreignTarget(path, target)
}

// resolvePlainPath handles a bare path with no scheme, which is what a link
// created by browsing to a nearby file looks like.
func resolvePlainPath(target, baseDir string) (LinkTarget, error) {
	// Every backslash becomes a forward slash before any test below runs, so
	// the prefix checks and filepath.Base see one separator style whatever the
	// workbook was written on. filepath.ToSlash is deliberately NOT also
	// called: on Windows it would repeat exactly this replacement, and on Unix
	// it is a no-op, so it can only ever look like it is doing something.
	slashed := strings.ReplaceAll(target, `\`, "/")
	base := filepath.Base(slashed)

	if namesNoFile(base) {
		return LinkTarget{}, fmt.Errorf("%w: %s", ErrNotAFile, target)
	}

	// The three absolute shapes — a UNC share, a drive letter, and a POSIX
	// root — share an outcome rather than a reason: each names a location on
	// the author's machine, so it is used as written and flagged Foreign
	// instead of being joined onto the workbook directory.
	switch {
	case strings.HasPrefix(slashed, "//"),
		winAbs.MatchString(slashed),
		strings.HasPrefix(slashed, "/"):
		return foreignTarget(slashed, target)
	}

	// Relative: resolve against the workbook's own directory, which is what
	// Excel does when the link is followed.
	return LinkTarget{
		Path: filepath.Join(baseDir, filepath.FromSlash(slashed)),
		Base: base,
	}, nil
}

// decodeURI percent-decodes a target, returning it unchanged when it is not
// valid encoding.
//
// Leaving a malformed value alone rather than failing is deliberate: a raw
// Windows path containing a percent sign — legal, and not rare in scanned
// filenames — is not URI-encoded at all, and rejecting it would lose a link
// that works perfectly well as written.
//
// The decision is all-or-nothing, which matters for the one target that
// mixes the two: PathUnescape fails on the FIRST bad escape and this returns
// the whole string raw, so "100% raw%20(2).pdf" keeps its literal "%20" as
// well as its "%". Both routes into this function reach that shape — the
// plain-path branch directly, and resolveFileURL's recovery branch after
// url.Parse refused the target for the same bad escape — and it ends as a
// Base with "%20" in it that no scan directory holds. Decoding what is
// decodable would fix that case and would need a hand-rolled scanner,
// which is more machinery than the case is worth.
func decodeURI(s string) string {
	if decoded, err := url.PathUnescape(s); err == nil {
		return decoded
	}
	return s
}

// ResolveLink turns the raw hyperlink stored in the workbook into a local path
// to try, relative to baseDir.
//
// baseDir must be the directory holding the WORKBOOK, not the working
// directory: that is what a relative target is stored relative to, and what
// Excel itself resolves against when the link is followed.
//
// # The shapes a hyperlink target actually takes
//
// Excel writes the target into the sheet's relationship file, and what it
// writes depends on how the link was created and where the workbook was when
// it was saved. All of these are ordinary:
//
//	scans/1.pdf                relative to the workbook — the portable case
//	..\scan\1.pdf              relative, Windows separators
//	1%20(2).pdf                relative, percent-encoded because URIs cannot
//	                           hold a raw space
//	file:///C:/Data/scan/1.pdf absolute, as a file URL
//	C:\Data\scan\1.pdf         absolute, raw
//	\\fileserver\share\1.pdf   a UNC share
//	https://drive.../view      not a file at all
//
// Only the relative forms survive being moved to another machine. The
// absolute ones name a path that existed on whoever built the workbook, and
// on the reading host they usually do not resolve — which is why Base is
// returned alongside Path rather than the function simply failing: the file
// is very often present under a different root, and matching on the filename
// recovers it.
//
// # A scheme this function does not know falls through, on purpose
//
// The list above is not the whole vocabulary. "smb://fileserver/share/1.pdf"
// and its relatives carry a scheme none of the branches below match, so they
// reach the plain-path branch, which reads the whole thing as relative and
// produces a Path of "<baseDir>/smb:/fileserver/share/1.pdf" — a location
// nothing will ever be found at.
//
// That reads like a bug and is the better of the two outcomes available. Base
// is still "1.pdf", which is the key a caller's by-filename recovery searches
// on, and a scan directory is exactly where a share that was never mounted
// here turns out to be. Reporting ErrNotAFile instead would be tidier and
// would throw that away: the caller stops looking for a file, or falls back to
// a different reference key that may resolve to a different row's document.
//
// So the useless Path is the price of a usable Base. A caller that treats Path
// as advisory and Base as the fallback — which is what the two fields are
// for — behaves correctly for every scheme, including the ones the switch
// below does not know about.
//
// Percent-decoding is applied to every form. Excel URI-encodes targets, so a
// scan called "35.002 (rev 2).pdf" is stored as "35.002%20(rev%202).pdf" and a
// literal lookup would miss it.
//
// Decoding unconditionally is right for every target an encoder produced and
// wrong for the rare one a person typed. A file genuinely NAMED
// "scan%2Freport.pdf" — a legal name, and "%2F" is a valid escape — decodes
// to "scan/report.pdf", which then reads as a subdirectory: Path gains a
// path element the workbook never named and Base becomes "report.pdf". This
// is the opposite failure to the one decodeURI tolerates, which is a percent
// that is not an escape at all, and unlike that one it cannot be told apart
// from a target the encoder wrote. Decoding is kept because the encoded form
// is what Excel actually stores.
func ResolveLink(target, baseDir string) (LinkTarget, error) {
	// TrimSpace, not this package's own Trim: Trim rewrites a non-breaking
	// space into an ordinary one, which is right for a cell somebody typed and
	// wrong for a path, where the byte is part of a filename that exists on
	// disk exactly as written.
	target = strings.TrimSpace(target)
	if target == "" {
		return LinkTarget{}, ErrNoLink
	}

	// A location-only hyperlink ("Sheet2!A1") points inside the workbook. It
	// carries no scheme and no separator, and treating it as a relative
	// filename would produce a confusing "file not found" for something that
	// was never a file.
	//
	// The shape is the whole test, and it has a known false positive: a
	// separator-less filename carrying a "!" — "urgent!.pdf", sitting beside
	// the workbook — is reported as an internal reference. A test written
	// against Excel's own grammar would separate the two, quoted sheet names
	// included, and the reason this one is a shape test instead is cost rather
	// than correctness. The false positive costs one document the operator can
	// relink; the opposite error hands a cell address to the filesystem.
	//
	// It also sits ABOVE the scheme switch, so a separator-less scheme whose
	// remainder contains a "!" is reported here rather than there:
	// "mailto:a!b@x.example" comes back as an internal reference. The error is
	// ErrNotAFile either way and callers branch on that, so the only cost is
	// the noun in a message nobody acts on. Moving the test below the switch
	// would fix the wording and is safe — every scheme matched there
	// carries a separator or a colon this test does not look at — but it
	// buys a wording change and nothing else.
	if strings.Contains(target, "!") && !strings.ContainsAny(target, `/\`) {
		return LinkTarget{}, fmt.Errorf(
			"%w: internal reference %q", ErrNotAFile, target)
	}

	lower := strings.ToLower(target)
	switch {
	case strings.HasPrefix(lower, "http://"),
		strings.HasPrefix(lower, "https://"),
		strings.HasPrefix(lower, "mailto:"),
		strings.HasPrefix(lower, "ftp://"):
		return LinkTarget{}, fmt.Errorf("%w: %s", ErrNotAFile, target)

	case strings.HasPrefix(lower, "file:"):
		return resolveFileURL(target, baseDir)
	}

	return resolvePlainPath(decodeURI(target), baseDir)
}
