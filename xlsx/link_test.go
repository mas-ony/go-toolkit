package xlsx

// Tests for link.go: every shape a hyperlink target takes — relative,
// absolute, UNC, a file URL, a scheme that is not a file — resolved against
// a fixed workbook directory, with percent-decoding done exactly once and
// never a Base that names something other than a file.

import (
	"errors"
	"path/filepath"
	"testing"
)

// A relative target resolves against the workbook's directory, not the
// working directory, and percent-encoding is decoded so a name with a space
// is found.
func TestResolveLinkRelative(t *testing.T) {
	cases := []struct{ in, wantBase string }{
		{"scans/1.pdf", "1.pdf"},
		{`scans\2.PDF`, "2.PDF"},
		{"scans/3%20(rev).pdf", "3 (rev).pdf"},
		{`..\archive\4.pdf`, "4.pdf"},
	}
	for _, c := range cases {
		got, err := ResolveLink(c.in, "/data/workbooks")
		if err != nil {
			t.Errorf("ResolveLink(%q): %v", c.in, err)
			continue
		}
		if got.Base != c.wantBase {
			t.Errorf(
				"ResolveLink(%q).Base = %q, want %q",
				c.in, got.Base, c.wantBase)
		}
		if got.Foreign {
			t.Errorf("ResolveLink(%q): relative target marked foreign", c.in)
		}
		if !filepath.IsAbs(got.Path) {
			t.Errorf(
				"ResolveLink(%q).Path = %q, want it joined onto the workbook "+
					"directory", c.in, got.Path)
		}
	}
}

// An absolute target from another machine must still yield a filename,
// because that is what makes a by-filename recovery path work at all.
func TestResolveLinkForeign(t *testing.T) {
	cases := []struct{ in, wantBase string }{
		{"file:///C:/Data/scans/4.pdf", "4.pdf"},
		{`C:\Data\scans\4.pdf`, "4.pdf"},
		{`\\fileserver\scans\5.pdf`, "5.pdf"},
		{"file://fileserver/scans/5.pdf", "5.pdf"},
		{"/mnt/archive/6.pdf", "6.pdf"},
	}
	for _, c := range cases {
		got, err := ResolveLink(c.in, "/data/workbooks")
		if err != nil {
			t.Errorf("ResolveLink(%q): %v", c.in, err)
			continue
		}
		if got.Base != c.wantBase {
			t.Errorf(
				"ResolveLink(%q).Base = %q, want %q",
				c.in,
				got.Base,
				c.wantBase)
		}
		if !got.Foreign {
			t.Errorf(
				"ResolveLink(%q): absolute target should be marked foreign",
				c.in)
		}
	}
}

// A link that is not a file at all must be reported as such and NOT as a
// missing file: the caller branches on it to fall through to its other route.
func TestResolveLinkNonFile(t *testing.T) {
	for _, in := range []string{
		"https://drive.example/view",
		"http://intranet/scans",
		"mailto:a@b.c",
		"Sheet2!A1",
	} {
		if _, err := ResolveLink(in, "/data"); !errors.Is(err, ErrNotAFile) {
			t.Errorf("ResolveLink(%q) error = %v, want ErrNotAFile", in, err)
		}
	}
}

// A blank target is ErrNoLink: the cell carries no hyperlink at all.
func TestResolveLinkEmpty(t *testing.T) {
	if _, err := ResolveLink("   ", "/data"); !errors.Is(err, ErrNoLink) {
		t.Errorf("ResolveLink(blank) error = %v, want ErrNoLink", err)
	}
}

// A percent sign in a raw Windows path is legal and not rare in scanned
// filenames. Decoding must not throw the link away when it turns out not to
// be URI-encoded at all.
func TestResolveLinkKeepsAMalformedEncoding(t *testing.T) {
	got, err := ResolveLink(`scans\100% raw.pdf`, "/data")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Base != "100% raw.pdf" {
		t.Errorf("Base = %q, want the name kept verbatim", got.Base)
	}
}

// Excel percent-encodes what it writes, so a file whose own name contains a
// percent escape is stored double-encoded. url.Parse undoes one layer;
// undoing a second turns "50%2B.pdf" into "50+.pdf" and the lookup misses a
// file that is sitting right there.
//
// Only the file: branch is pinned here, and the plain-path branch is
// deliberately not held to the same standard: a bare Windows path is
// indistinguishable from an encoded target, so decodeURI reads any escape it
// finds and a raw "50%2B.pdf" does come back as "50+.pdf". That is the
// tradeoff decodeURI documents. It is also the reason a file: URL — which
// says outright that it is encoded — must not be put through the same
// guesswork a second time.
func TestResolveLinkDecodesFileURLExactlyOnce(t *testing.T) {
	cases := []struct{ in, wantBase string }{
		{"file:///C:/Data/50%252B.pdf", "50%2B.pdf"},
		{"file:///C:/Data/scan%20(2).pdf", "scan (2).pdf"},
		{"file://fileserver/scans/50%252B.pdf", "50%2B.pdf"},
	}
	for _, c := range cases {
		got, err := ResolveLink(c.in, "/data")
		if err != nil {
			t.Errorf("ResolveLink(%q): %v", c.in, err)
			continue
		}
		if got.Base != c.wantBase {
			t.Errorf(
				"ResolveLink(%q).Base = %q, want %q",
				c.in, got.Base, c.wantBase)
		}
	}
}

// A file URL that url.Parse refuses must not be thrown away. The plain-path
// branch keeps a raw percent sign deliberately, and a hand-written file: URL
// carrying one names a file just as real.
//
// The uppercase entry is the one that pins how the scheme comes off. A URI
// scheme is case-insensitive and ResolveLink matches it on a lowercased copy,
// so "FILE:" reaches this branch like any other — but a case-sensitive
// TrimPrefix would leave the prefix on, and the remainder would then look like
// a relative path and be joined onto the workbook directory. That spelling
// parses cleanly whenever the rest of the target is well formed, so it is only
// ever seen HERE, in the branch written to rescue the targets url.Parse
// refuses.
func TestResolveLinkRecoversAnUnparseableFileURL(t *testing.T) {
	cases := []struct {
		in       string
		wantBase string
		wantPath string
	}{
		{
			`file:///C:/scan/100% raw.pdf`,
			"100% raw.pdf",
			`C:/scan/100% raw.pdf`,
		},
		{
			`FILE:///C:/scan/100% raw.pdf`,
			"100% raw.pdf",
			`C:/scan/100% raw.pdf`,
		},
		{
			`file://fileserver/scans/100% raw.pdf`,
			"100% raw.pdf",
			`//fileserver/scans/100% raw.pdf`,
		},
	}
	for _, c := range cases {
		got, err := ResolveLink(c.in, "/data")
		if err != nil {
			t.Errorf("ResolveLink(%q): %v", c.in, err)
			continue
		}
		if got.Base != c.wantBase {
			t.Errorf(
				"ResolveLink(%q).Base = %q, want %q",
				c.in, got.Base, c.wantBase)
		}
		if got.Path != filepath.FromSlash(c.wantPath) {
			t.Errorf(
				"ResolveLink(%q).Path = %q, want %q",
				c.in, got.Path, c.wantPath)
		}
		if !got.Foreign {
			t.Errorf("ResolveLink(%q): want Foreign", c.in)
		}
	}
}

// An opaque file URL carries the path in u.Opaque and leaves u.Path empty,
// which is the same shape an authority-only URL has. Reporting ErrNotAFile
// would send the caller off to its fallback route for a file that exists.
func TestResolveLinkOpaqueFileURL(t *testing.T) {
	got, err := ResolveLink("file:C:/Data/scan.pdf", "/data")
	if err != nil {
		t.Fatalf("ResolveLink: %v", err)
	}
	if got.Base != "scan.pdf" {
		t.Errorf("Base = %q, want %q", got.Base, "scan.pdf")
	}
	if !got.Foreign {
		t.Error("a drive-lettered opaque target should be marked foreign")
	}
}

// A file URL naming no path at all is the one case that really is not a file.
//
// The two UNC spellings are the ones that need saying. An authority-only URL
// has a non-empty HOST and an empty path, so it reaches the UNC branch, and
// a guard placed after that branch never sees it. What comes back then is a
// LinkTarget whose Base is filepath.Base("") — "." — or Base("/") — "/" —
// neither of which is a filename, and Base is the key the caller searches on
// when the path itself does not resolve.
func TestResolveLinkEmptyFileURL(t *testing.T) {
	for _, in := range []string{
		"file:///",
		"file://",
		"file://fileserver",
		"file://fileserver/",
	} {
		got, err := ResolveLink(in, "/data")
		if !errors.Is(err, ErrNotAFile) {
			t.Errorf(
				"ResolveLink(%q) = %+v, %v; want ErrNotAFile",
				in, got, err)
		}
	}
}

// Whatever else it returns, ResolveLink must never hand back a Base that is
// not a filename: "." and "/" are what filepath.Base yields for a path naming
// no file, and a caller matching on either searches the scan directory for
// something that cannot exist.
func TestResolveLinkNeverReturnsANonFilenameBase(t *testing.T) {
	targets := []string{
		"file://fileserver",
		"file://fileserver/",
		"file:///",
		"file://",
		"file:///C:",
		"file://localhost/",
		`\\fileserver\scans\5.pdf`,
		"scans/1.pdf",
		"file:///C:/Data/scans/4.pdf",
	}
	for _, in := range targets {
		got, err := ResolveLink(in, "/data/workbooks")
		if err != nil {
			continue // reported as not-a-file, which is the other valid out
		}
		if got.Base == "." || got.Base == "/" || got.Base == "" {
			t.Errorf(
				"ResolveLink(%q).Base = %q, which names no file", in, got.Base)
		}
	}
}
