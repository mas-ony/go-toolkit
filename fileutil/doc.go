// Package fileutil accepts a user-supplied file, stores it on disk, reads
// it back, and cleans it up.
//
// It depends on nothing outside the standard library and holds no state
// beyond its configuration.
//
// # Layout
//
// One directory per owning record, named for that record's primary key:
//
//	<uploadDir>/<id>/<name>
//
// Only name is persisted. The full path is always reconstructable from the
// row itself, because the row carries the id, so no stored path can go
// stale if uploadDir moves between environments.
//
// A record may hold one file or many; the layout is the same either way.
// Write overwrites a same-named file in place, which is what a single-file
// record wants. Records that accumulate files and must not clobber an
// earlier upload use CopyUnique, or de-duplicate the name before calling
// Write.
//
// Upload DIRECTORIES are deliberately not defined here. They name a
// specific caller's document types and arrive as the uploadDir argument.
//
// # Ordering rule
//
// Write the file BEFORE the database row on upload, and delete the row
// BEFORE the file on removal. Both orders bias every crash window towards
// the same outcome: a file on disk that no row references. That is
// invisible to users, harmless to correctness, and reclaimable by a sweep
// that compares the directory tree against the stored names. The opposite
// orders would leave a row pointing at a file that does not exist, which
// surfaces as a broken download for a record that looks healthy in a list.
//
// Record creation cannot literally write the file first, because the
// directory name is the identity value the INSERT has not produced yet. A
// caller in that position holds the INSERT open in a transaction, writes
// the file, and only then commits, so a failure at any point still ends
// with no row and at worst an orphaned file.
//
// A sweep reclaiming orphans must match the directory against the STORED
// NAMES, never against the ".tmp-" prefix Write uses. SafeName accepts a
// leading dot, so ".tmp-scan.pdf" is a name a client can upload, and a
// prefix match would delete a live file a row still points at.
//
// # Who checks what
//
// The division is deliberate and easy to get backwards. Write stores
// whatever name and however many bytes it is handed; NameTooLong and
// TooLarge are the CALLER's checks, made at the edge where a rejection can
// still be a 400 rather than a half-finished upload.
//
//	caller, at the edge   HasAllowedExt, NameTooLong, TooLarge
//	Write                 SafeName, and nothing else
//	CopyUnique            SafeName, and the name cap on names it INVENTS
//	DetectContentType     what the bytes actually are
//
// CopyUnique is the one exception, and only because it is the one place
// that manufactures a name rather than sanitising one it was given: a
// " (2)" suffix can push a name that fit over the cap, and the caller
// persists what comes back.
//
// Checking the name the caller HOLDS and persisting the one Write RETURNS
// is safe, because SafeName never lengthens a name beyond
// len(FallbackName).
//
// # Names are sanitised, never trusted
//
// SafeName reduces any supplied name to a single path element: control
// bytes dropped, directory prefix stripped, edge whitespace trimmed, and
// the handful of results that are not usable filenames folded onto
// FallbackName. It is idempotent, which Path and Write both depend on —
// a file stored under one spelling and looked up under another is lost.
//
// The threat is not only traversal. A filename reaches a
// Content-Disposition header, and a multipart filename can carry an RFC
// 5987 ext-value whose percent-decoding yields a raw CR or LF. Some HTTP
// stacks strip those at the header layer; a filename is this package's to
// sanitise and should not depend on the framework noticing.
//
// # Content type is decided by the bytes, with two named exceptions
//
// DetectContentType prefers content over both the extension and anything
// the client declared, because the result is stored and later echoed back
// as a Content-Type header. A file called scan.pdf whose bytes are HTML
// must not be recorded as a PDF.
//
// A sniffed type is trusted only if it is an ACCEPTED type. Everything
// that is recognised and not accepted collapses to
// application/octet-stream, rather than repeating a claim the bytes
// contradict. Two cases escape that:
//
//   - An unrecognised sniff falls through to the extension table, which is
//     what a TIFF needs: Go has no TIFF signature.
//   - A CONTAINER sniff is refined by the extension, which is what a .docx
//     needs: it really is a ZIP, and only the name can say which format the
//     container holds.
//
// Both exceptions can be defeated by one careless registration, and the
// failure is silent in each direction. Registering an archive type admits
// application/zip to the accepted set, after which every office document
// is recorded as a zip. Registering the generic type admits
// application/octet-stream, after which every signature-less format stops
// resolving. Those two are the first things to look at if a .docx starts
// coming back as application/zip or a .tif stops coming back as
// image/tiff.
//
// This closes the hole where the type is DECIDED, which is the durable
// place for it. It does not close it on the wire — a static file handler
// may overwrite Content-Type from its own table. What does is
// X-Content-Type-Options: nosniff and Content-Disposition: attachment,
// neither of which this package can set, and neither of which depends on
// the value here being right.
//
// # Configuration is process-wide
//
// Everything tunable is package state, set once at startup, before the
// first request is served, and read-only thereafter:
//
//	fileutil.MaxFileBytes = 10 << 20            // per-file cap
//	fileutil.MaxNameLen   = 50                  // mirror the name column
//	fileutil.RegisterExts(fileutil.OfficeExts)  // widen the accepted types
//
// The two limits are read without synchronisation, so changing either
// while requests are in flight is a data race. The extension table is
// mutex-guarded and so is safe to register into at any time — but a
// registration racing a live upload changes what that upload is allowed to
// be, which is a configuration bug rather than a concurrency one.
//
// One consequence worth naming: two components in one binary cannot have
// different caps or different accepted types. If that is ever needed, the
// limits have to become arguments rather than variables, and that is a
// change to every signature here.
//
// MaxNameLen is half of a pair. It is only safe because the column it
// mirrors is at least that wide, so the two move together and in that
// order: migrate first, then raise this. Its unit is BYTES rather than
// runes, because Go's len() on UTF-8 is never smaller than a column's
// codepage encoding of the same text — exact against engines that size a
// VARCHAR in bytes, merely strict against those that count characters, and
// correct without knowing which one is on the other end.
//
// # Durability
//
// Write goes to a temporary file in the destination directory, syncs it,
// chmods it, renames it into place, and then syncs the DIRECTORY. Within
// one filesystem the rename is atomic, so a reader arriving mid-write sees
// the complete old file or the complete new one and never a truncated
// prefix.
//
// The directory sync is what keeps the ordering rule honest: without it a
// power loss can leave the bytes on the device under no name at all, and
// since the caller updates the row after Write returns, the surviving
// state would be a row naming a file that does not exist.
//
// Only the record's own directory is synced, not its parents. A record
// directory created moments earlier still has its entry in the page cache,
// so a power loss can take the directory and the file inside it together.
// Closing that would cost an open and an fsync per level on every create;
// the exposure left is one entry per record rather than one per write.
//
// # What the tests hold in place
//
// The unit suite covers the name sanitisation table, the content-type
// decision including both exceptions and both ways to defeat them, the
// limits and their labels, the atomic-reservation loop in CopyUnique, and
// the permission behaviour under a hostile umask — which needs
// syscall.Umask, so that file carries a unix build constraint and the unit
// suite runs on Unix only.
//
// fileutil_integration_test.go covers what a unit test cannot promise,
// because it depends on the filesystem, the process identity, or on timing
// rather than on this code: whether the filesystem folds case, whether a
// concurrent reader ever observes a partial write, what happens to a
// directory the process may not write to, and whether a file at the
// megabyte scale survives the temp-and-rename path intact. Each skips
// rather than fails where the environment cannot produce the precondition.
//
//	go test -tags integration -run Integration ./fileutil
//
// It needs no server and no configuration.
package fileutil
