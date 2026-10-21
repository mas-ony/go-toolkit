package xlsx

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/mas-ony/go-toolkit/internal/bytesize"
)

// archive is the ZIP package a workbook is stored as, read beside
// excelize for what excelize does not report: what the package would
// expand to, whether it carries macros, where a sheet's XML really
// ends, and which cells the sheet merges.
type archive struct {
	// parts holds the package's entries by name, as partName spells
	// one. Of two entries sharing a name the later is kept, which is
	// the one excelize reads.
	parts map[string]*zip.File

	// sheets lists the workbook's sheets in the workbook's own order.
	sheets []listedSheet

	// macros reports that the package carries a VBA project: the entry
	// one is kept in by custom, xl/vbaProject.bin, or a part under any
	// name that the workbook's relationships say is one.
	macros bool
}

// listedSheet is one sheet the workbook lists: its name, and the entry
// its XML is in, which is nil when the package holds no such entry.
type listedSheet struct {
	name string
	part *zip.File
}

// relationships is the content of a .rels part: what each id used in
// the part beside it points at.
type relationships struct {
	Items []struct {
		ID     string `xml:"Id,attr"`
		Type   string `xml:"Type,attr"`
		Target string `xml:"Target,attr"`
	} `xml:"Relationship"`
}

// sheetScan is fed a sheet's XML from start to end and keeps two things
// of it: the number its last row declares, and the ranges it merges.
//
// # Why the XML is read a second time
//
// Excelize reads a sheet's rows through an XML decoder and takes the
// first error from it for the end of the sheet. One character XML does
// not allow, in one cell, ends the reading there without a word, and
// the rows above it look like the whole sheet. The row the XML really
// ends with is the only thing to hold the reading against, and it is
// in the XML and nowhere else.
//
// The merged ranges are listed after the rows. Excelize hands them over
// only once it has built its model of the whole sheet, a struct for
// every cell, which for a large sheet is gigabytes where the text of its
// cells is a few per cent of that. Reading the list here is what lets
// Open never ask for that model.
//
// # How little of it is looked at
//
// Nothing is parsed. The rows go past as bytes, of which only the tail
// is kept, and the search is for three byte patterns: the end of
// sheetData, the last row start tag before it, and the mergeCell tags
// after it. That costs the unpacking of the part and little more, a
// fraction of what reading the cells took.
//
// Patterns are safe to look for in place of tags because XML cannot
// spell a '<' inside text or an attribute. A CDATA section or a comment
// can, and no writer puts either in a sheet. One that did could hide a
// truncation or a merge from the scan, or show it one that is not
// there, and nothing else: the rows themselves are excelize's reading,
// which the scan never replaces.
//
// The bytes are taken for an ASCII-compatible encoding, which UTF-8 is
// and every common writer produces. A sheet stored as UTF-16 matches no
// pattern: its merged captions go unresolved and its end unchecked.
type sheetScan struct {
	// buf is the XML still being looked at: the tail of the rows until
	// their end is found, and from then on the tag being received.
	buf []byte

	// past reports that the end of sheetData has gone by.
	past bool

	// last is the number the last row declares, or 0 when it cannot be
	// told, and merges are the references of the merged ranges, each as
	// the sheet writes it: "B5:D5".
	last   int
	merges []string
}

// TruncatedError is the refusal of a sheet whose XML stops being
// readable before its end. The rows above the fault were read and the
// rows below it were not, so the grid would have been a part of the
// sheet passed off as all of it.
//
// It is a type, as LayoutError is, so a caller can say how far the
// reading got in words of its own.
type TruncatedError struct {
	// Sheet is the sheet that was being read.
	Sheet string

	// Read is the last row the reading reached, and Last the last row
	// the sheet's XML declares.
	Read, Last int
}

const (
	// sheetTailBytes is how much of the end of a sheet's rows is kept
	// to find the last of them in. A row is a few kilobytes of XML in
	// anything but a sheet thousands of columns wide, so its start tag
	// is well inside that.
	sheetTailBytes = 256 << 10

	// endTagReach is how far before the bytes of a write the search
	// for the end of sheetData starts, so that an end tag two writes
	// share is found: "</sheetData>", or that under a prefix.
	endTagReach = 64

	// maxTagBytes is the longest run of XML after the rows that is
	// held while its closing '>' has not arrived. A mergeCell tag is a
	// few dozen bytes, so a longer run is something else and is let
	// go.
	maxTagBytes = 64 << 10

	// maxPartBytes bounds the reading of the small parts that say
	// which entry holds a sheet.
	maxPartBytes = 16 << 20
)

// MaxUnzippedBytes caps what a workbook may expand to: the sum of the
// uncompressed sizes its ZIP directory declares, 1 GiB unless a caller
// sets another. Zero or negative disables the cap and leaves
// excelize's own, 16 GB.
//
// Excelize unpacks every part of a workbook, into memory or, for a
// large sheet, into a temporary file. DEFLATE reaches a thousand to
// one, so without a cap a few megabytes of upload can ask for all of
// that. A workbook compresses ten to twenty times, which puts 1 GiB
// far above what one of a size worth sending expands to.
//
// The cap is held against the directory before excelize is handed the
// workbook, and nothing is unpacked for it. The declared sizes can be
// trusted that far: archive/zip, which excelize reads through, fails
// an entry that yields more than it declared.
//
// It is a variable for the reason fileutil's limits are: the right
// number differs per caller. Assign it at startup, since it is read
// without synchronisation.
var MaxUnzippedBytes int64 = 1 << 30

// Errors returned by Open and OpenBytes.
var (
	// ErrNotWorkbook means the content is not a workbook this package
	// reads. It is not a ZIP archive, which is what a legacy .xls, an
	// encrypted workbook and an HTML or CSV export all are not, or it
	// is an archive whose workbook lists no sheet, which is what any
	// other kind of document is.
	ErrNotWorkbook = errors.New("not an .xlsx workbook")

	// ErrTooLarge means the workbook would expand past
	// MaxUnzippedBytes.
	ErrTooLarge = errors.New("workbook too large")
)

// errNoSheet is ErrNotWorkbook for an archive whose workbook lists no
// sheet. inspect returns it when the archive's own listing is empty and
// load when excelize's is, so both readings refuse in one wording.
var errNoSheet = fmt.Errorf("%w: it holds no sheet", ErrNotWorkbook)

// rowNumber matches the r attribute of a row's start tag, which
// carries the row's number.
var rowNumber = regexp.MustCompile(`\sr\s*=\s*["']([0-9]+)["']`)

// mergeRef matches a mergeCell start tag, with or without a prefix, up
// to its ref attribute, which carries the merged range.
var mergeRef = regexp.MustCompile(
	`<(?:[\w.\-]+:)?mergeCell(?:\s[^<>]*?)?\sref\s*=\s*["']([^"']*)["']`)

// partName returns the name of a ZIP entry as entries are matched:
// with forward slashes, no leading one, and in lower case.
func partName(name string) string {
	name = strings.ReplaceAll(name, `\`, "/")
	return strings.ToLower(strings.TrimPrefix(name, "/"))
}

// decodePart decodes the XML of a ZIP entry into v and reports whether
// it could.
func decodePart(part *zip.File, v any) bool {
	if part == nil {
		return false
	}
	rc, err := part.Open()
	if err != nil {
		return false
	}
	defer func() { _ = rc.Close() }()

	limited := io.LimitReader(rc, maxPartBytes)
	return xml.NewDecoder(limited).Decode(v) == nil
}

// isNameByte reports whether c can be part of an XML name, a prefix
// here. Every byte of a character beyond ASCII counts.
func isNameByte(c byte) bool {
	return c == '_' || c == '-' || c == '.' || c >= 0x80 ||
		'0' <= c && c <= '9' ||
		'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z'
}

// isSpace reports whether c is white space as XML counts it.
func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n'
}

// tagStart returns where the tag opens whose name starts at b[i]: the
// index of its '<', past a namespace prefix and, in an end tag, the
// slash. end reports an end tag. The index is -1 when the name is not
// the name of a tag at all, which is how the word reads inside a
// cell's text.
func tagStart(b []byte, i int) (start int, end bool) {
	if i > 0 && b[i-1] == ':' {
		i--
		for i > 0 && isNameByte(b[i-1]) {
			i--
		}
	}
	switch {
	case i > 1 && b[i-1] == '/' && b[i-2] == '<':
		return i - 2, true
	case i > 0 && b[i-1] == '<':
		return i - 1, false
	}
	return -1, false
}

// sheetDataEnd finds where the rows of a sheet end in b, looking no
// earlier than from. It returns the index the closing tag opens at and
// the index past it, or -1 and -1 while b holds none.
//
// The end is the end tag of sheetData, or the whole of a sheetData
// element written with no content, which is how a sheet without a row
// is stored.
func sheetDataEnd(b []byte, from int) (int, int) {
	const name = "sheetData"
	for at := from; ; {
		i := bytes.Index(b[at:], []byte(name))
		if i < 0 {
			return -1, -1
		}
		i += at
		at = i + len(name)

		start, end := tagStart(b, i)
		if start < 0 {
			continue
		}
		rest := at
		for rest < len(b) && isSpace(b[rest]) {
			rest++
		}
		switch {
		case end && rest < len(b) && b[rest] == '>':
			return start, rest + 1
		case !end && bytes.HasPrefix(b[rest:], []byte("/>")):
			return start, rest + 2
		}
	}
}

// lastRowNumber returns the number the last row start tag in b
// declares. It returns 0 when b holds no such tag, or the tag carries
// no number.
func lastRowNumber(b []byte) int {
	const name = "row"
	for end := len(b); ; {
		i := bytes.LastIndex(b[:end], []byte(name))
		if i < 0 {
			return 0
		}
		end = i

		// The whole of the name, and of a start tag: "rowBreaks" and
		// the end tag of a row are neither.
		after := i + len(name)
		if after >= len(b) || !isSpace(b[after]) &&
			b[after] != '>' && b[after] != '/' {
			continue
		}
		if start, closing := tagStart(b, i); start < 0 || closing {
			continue
		}
		// A tag the XML ends inside of is not a row that is there.
		stop := bytes.IndexByte(b[i:], '>')
		if stop < 0 {
			continue
		}
		m := rowNumber.FindSubmatch(b[i : i+stop])
		if m == nil {
			return 0
		}
		n, err := strconv.Atoi(string(m[1]))
		if err != nil {
			return 0
		}
		return n
	}
}

// inspect reads the ZIP directory of a workbook and the three small
// parts that say which sheets it holds and where, and nothing past them.
//
// It fails with ErrNotWorkbook for content that is no ZIP archive or
// whose workbook lists no sheet, and with ErrTooLarge for an archive
// that would expand past MaxUnzippedBytes. The cap is settled first, on
// the directory alone, so an archive over it is refused before any of
// it is unpacked. The refusal names the cap through bytesize.Label, the
// rule fileutil names its upload cap by, so a cap no unit divides is
// given in bytes rather than rounded.
//
// A size past what an int64 holds is refused whatever the cap. Excelize
// adds the sizes up as int64, where such a size is negative: it passes
// excelize's own limit, and then sizes an allocation that panics.
func inspect(r io.ReaderAt, size int64) (*archive, error) {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotWorkbook, err)
	}

	limit := uint64(math.MaxInt64)
	if MaxUnzippedBytes > 0 {
		limit = uint64(MaxUnzippedBytes)
	}
	a := &archive{parts: make(map[string]*zip.File, len(zr.File))}
	var unzipped uint64
	for _, f := range zr.File {
		// Compared before it is added, so a declared size near the top
		// of uint64 cannot wrap the sum back under the cap.
		if f.UncompressedSize64 > limit-unzipped {
			return nil, fmt.Errorf("%w: it expands to more than %s",
				ErrTooLarge, bytesize.Label(int64(limit)))
		}
		unzipped += f.UncompressedSize64

		name := partName(f.Name)
		a.parts[name] = f
		if name == "xl/vbaproject.bin" {
			a.macros = true
		}
	}

	a.sheets = a.listSheets()
	if len(a.sheets) == 0 {
		return nil, errNoSheet
	}
	return a, nil
}

// listSheets reads which sheets the workbook lists and where each is
// kept, the way excelize finds them: the workbook part the package's
// root relationships point at, the sheets that part names, and the part
// the workbook's own relationships give each sheet's id. It returns
// none when the package does not say.
//
// The same relationships name the workbook's VBA project, and macros is
// set when they do.
func (a *archive) listSheets() []listedSheet {
	var root relationships
	if !decodePart(a.parts["_rels/.rels"], &root) {
		return nil
	}
	book := ""
	for _, rel := range root.Items {
		if strings.HasSuffix(rel.Type, "/officeDocument") {
			book = partName(rel.Target)
			break
		}
	}

	var workbook struct {
		Sheets []struct {
			Name string `xml:"name,attr"`
			ID   string `xml:"id,attr"`
		} `xml:"sheets>sheet"`
	}
	if !decodePart(a.parts[book], &workbook) {
		return nil
	}
	// A workbook that cannot say where its sheets are still lists
	// them, so its relationships failing to read is not the end of it.
	var rels relationships
	dir, base := path.Split(book)
	decodePart(a.parts[dir+"_rels/"+base+".rels"], &rels)

	// A package may keep its VBA project under any name. xl/vbaProject.bin
	// is the custom and nothing holds a writer to it: what makes a part
	// the project is the workbook calling it that.
	for _, rel := range rels.Items {
		if strings.HasSuffix(rel.Type, "/vbaProject") {
			a.macros = true
		}
	}

	out := make([]listedSheet, 0, len(workbook.Sheets))
	for _, s := range workbook.Sheets {
		sheet := listedSheet{name: s.Name}
		for _, rel := range rels.Items {
			if rel.ID != s.ID {
				continue
			}
			// A target is relative to the workbook part's folder
			// unless it starts at the package's root.
			target := rel.Target
			if !strings.HasPrefix(target, "/") {
				target = dir + target
			}
			sheet.part = a.parts[partName(path.Clean(target))]
			break
		}
		out = append(out, sheet)
	}
	return out
}

// part returns the entry holding the named sheet, whatever the letter
// case of the name, or nil when the package holds none.
func (a *archive) part(sheet string) *zip.File {
	for _, s := range a.sheets {
		if strings.EqualFold(s.name, sheet) {
			return s.part
		}
	}
	return nil
}

// scan reads the XML of the named sheet for the number of its last row
// and the ranges it merges; see sheetScan.
//
// The row is 0 and the ranges none when the package does not say which
// entry holds the sheet. The caller then has nothing to hold its
// reading against, and takes it as it is.
func (a *archive) scan(sheet string) (int, []string, error) {
	part := a.part(sheet)
	if part == nil {
		return 0, nil, nil
	}
	rc, err := part.Open()
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = rc.Close() }()

	var s sheetScan
	if _, err := io.Copy(&s, rc); err != nil {
		return 0, nil, err
	}
	s.finish()
	return s.last, s.merges, nil
}

// finish settles what the scan found once the XML has ended. XML that
// never closed its sheetData ends with whatever row came last.
func (s *sheetScan) finish() {
	if !s.past {
		s.last = lastRowNumber(s.buf)
	}
	s.buf = nil
}

// Write takes the next stretch of the sheet's XML. It never fails.
func (s *sheetScan) Write(p []byte) (int, error) {
	seen := len(s.buf)
	s.buf = append(s.buf, p...)

	if !s.past {
		start, end := sheetDataEnd(s.buf, max(0, seen-endTagReach))
		if start < 0 {
			// Still among the rows: all that matters of them is the
			// last, so only the tail is carried forward.
			if len(s.buf) > 2*sheetTailBytes {
				tail := s.buf[len(s.buf)-sheetTailBytes:]
				s.buf = append(s.buf[:0], tail...)
			}
			return len(p), nil
		}
		s.last = lastRowNumber(s.buf[:start])
		s.past = true
		s.buf = append(s.buf[:0], s.buf[end:]...)
	}

	// Past the rows. Whatever lies before the last '>' is whole tags,
	// and what follows it is a tag still being received.
	if end := bytes.LastIndexByte(s.buf, '>'); end >= 0 {
		for _, m := range mergeRef.FindAllSubmatch(s.buf[:end+1], -1) {
			s.merges = append(s.merges, string(m[1]))
		}
		s.buf = append(s.buf[:0], s.buf[end+1:]...)
	}
	if len(s.buf) > maxTagBytes {
		s.buf = s.buf[:0]
	}
	return len(p), nil
}

// Error says how far the reading got.
func (e *TruncatedError) Error() string {
	return fmt.Sprintf("only %d of the %d rows of sheet %q could be "+
		"read: its XML is malformed after that",
		e.Read, e.Last, e.Sheet)
}
