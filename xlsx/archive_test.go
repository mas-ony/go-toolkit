package xlsx

// Tests for archive.go: the cap on what a workbook may expand to, what
// is told apart from a workbook, where a sheet's XML is found and what
// the scan makes of it, and the two things the scan is for — a sheet
// that cannot be read to its end, and merged ranges read without
// excelize's model of the sheet.

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/xuri/excelize/v2"
)

// sheetXML is where excelize writes a new file's one sheet.
const sheetXML = "xl/worksheets/sheet1.xml"

// fixtureBytes is the fixture as an upload would hold it: in memory.
func fixtureBytes(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(writeFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// rebuild returns book with every entry passed through edit, which is
// handed the entry's name and content and returns the content to keep.
// Each of extra is added as an entry of its own.
func rebuild(
	t *testing.T, book []byte,
	edit func(name, content string) string, extra ...string,
) []byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(book), int64(len(book)))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	write := func(name, content string) {
		w, err := zw.Create(name)
		if err == nil {
			_, err = io.WriteString(w, content)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, entry := range zr.File {
		rc, err := entry.Open()
		if err != nil {
			t.Fatal(err)
		}
		content, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		write(entry.Name, edit(entry.Name, string(content)))
	}
	for _, name := range extra {
		write(name, "not read")
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// faultAt returns book with a character XML does not allow put in front
// of the start tag of the given row of its sheet.
func faultAt(t *testing.T, book []byte, row int) []byte {
	t.Helper()
	tag := fmt.Sprintf(`<row r="%d"`, row)
	return rebuild(t, book, func(name, content string) string {
		if name != sheetXML {
			return content
		}
		if strings.Count(content, tag) != 1 {
			t.Fatalf("the sheet has no row %d:\n%s", row, content)
		}
		return strings.Replace(content, tag, "\x0b"+tag, 1)
	})
}

// listing is the two parts that say a workbook has a sheet, as declaring
// writes them, and listingBytes what they add to an archive's sizes.
var listing = map[string]string{
	"_rels/.rels": `<Relationships><Relationship Id="rId1" ` +
		`Type="http://example.test/officeDocument" ` +
		`Target="xl/workbook.xml"/></Relationships>`,
	"xl/workbook.xml": `<workbook><sheets><sheet name="Sheet1" ` +
		`sheetId="1" r:id="rId1"/></sheets></workbook>`,
}

var listingBytes = uint64(
	len(listing["_rels/.rels"]) + len(listing["xl/workbook.xml"]))

// declaring builds a workbook's archive in which each sheet declares one
// of sizes as its uncompressed size. The bytes are not there to back
// the claim: inspect holds the cap against the directory alone, and the
// directory is what makes it. The parts that list the sheet are real,
// and count towards the sum with the rest.
func declaring(t *testing.T, sizes ...uint64) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range listing {
		w, err := zw.Create(name)
		if err == nil {
			_, err = io.WriteString(w, content)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	for i, size := range sizes {
		w, err := zw.CreateRaw(&zip.FileHeader{
			Name:   fmt.Sprintf("xl/worksheets/sheet%d.xml", i+1),
			Method: zip.Deflate,

			CompressedSize64:   1,
			UncompressedSize64: size,
		})
		if err == nil {
			_, err = w.Write([]byte{0})
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// scanned feeds xml to a sheetScan step bytes at a time, and returns
// what it found.
func scanned(xml string, step int) (int, []string) {
	var s sheetScan
	for b := []byte(xml); len(b) > 0; {
		n := min(step, len(b))
		_, _ = s.Write(b[:n])
		b = b[n:]
	}
	s.finish()
	return s.last, s.merges
}

// The sum of the declared sizes is held against MaxUnzippedBytes: at the
// cap a workbook passes, one byte over it is refused, in one part or
// across several, and a size near the top of uint64 cannot wrap the sum
// back under the cap.
//
// The last two cases are the reason the check cannot be left to
// excelize. It adds the sizes up as int64, where a size of 2^63 or more
// is negative: the sum goes DOWN, its limit passes, and the allocation
// sized by that number panics.
func TestInspectHoldsTheDirectoryAgainstTheCap(t *testing.T) {
	// room is what the cap leaves the sheets once the parts that list
	// them are counted, and half and rest are that in two.
	room := uint64(MaxUnzippedBytes) - listingBytes
	half := room / 2
	rest := room - half
	for _, c := range []struct {
		name    string
		sizes   []uint64
		refused bool
	}{
		{"at the cap", []uint64{room}, false},
		{"one byte over", []uint64{room + 1}, true},
		{"two parts at the cap", []uint64{half, rest}, false},
		{"two parts and a byte", []uint64{half, rest + 1}, true},
		{"a sum that wraps", []uint64{half, math.MaxUint64 - 9}, true},
		{"a size no int64 holds", []uint64{1 << 63}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			data := declaring(t, c.sizes...)
			_, err := inspect(bytes.NewReader(data), int64(len(data)))
			switch {
			case !c.refused:
				if err != nil {
					t.Errorf("refused: %v", err)
				}
			case !errors.Is(err, ErrTooLarge) ||
				!strings.Contains(err.Error(), "1024 MiB"):
				t.Errorf("answered %v, want ErrTooLarge naming the "+
					"1024 MiB cap", err)
			}
		})
	}
}

// A cap of zero is no cap of this package's own. What stays refused is
// the size excelize cannot add up, which is not a matter of policy.
func TestInspectWithoutACap(t *testing.T) {
	old := MaxUnzippedBytes
	MaxUnzippedBytes = 0
	t.Cleanup(func() { MaxUnzippedBytes = old })

	over := declaring(t, uint64(old)+1)
	if _, err := inspect(
		bytes.NewReader(over), int64(len(over))); err != nil {
		t.Errorf("a workbook over the default cap was refused: %v", err)
	}
	huge := declaring(t, 1<<63)
	_, err := inspect(bytes.NewReader(huge), int64(len(huge)))
	if !errors.Is(err, ErrTooLarge) {
		t.Errorf("a size no int64 holds answered %v, want ErrTooLarge",
			err)
	}
}

// Content that is no ZIP archive is no workbook, whatever its name was:
// nothing at all, a CSV export, and a workbook cut short. So is an
// archive that holds no sheet, which is what a .docx is.
func TestOpenBytesTellsWhatIsNoWorkbook(t *testing.T) {
	book := fixtureBytes(t)
	var docx bytes.Buffer
	zw := zip.NewWriter(&docx)
	if _, err := zw.Create("word/document.xml"); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	for name, data := range map[string][]byte{
		"nothing":            nil,
		"a csv export":       []byte("No,Tanggal\n1,2026-08-31\n"),
		"a truncated upload": book[:len(book)/2],
		"another archive":    docx.Bytes(),
	} {
		t.Run(name, func(t *testing.T) {
			wb, err := OpenBytes(data, fixtureLayout())
			if !errors.Is(err, ErrNotWorkbook) {
				t.Fatalf("answered %v, want ErrNotWorkbook", err)
			}
			if wb != nil {
				t.Error("a File was returned beside the error")
			}
		})
	}
}

// The same refusals reach a caller of Open, under the file's path.
func TestOpenTellsWhatIsNoWorkbook(t *testing.T) {
	path := filepath.Join(t.TempDir(), "export.xlsx")
	if err := os.WriteFile(path, []byte("a,b\n1,2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Open(path, fixtureLayout())
	if !errors.Is(err, ErrNotWorkbook) ||
		!strings.Contains(err.Error(), path) {
		t.Errorf("answered %v, want ErrNotWorkbook naming %s", err, path)
	}

	old := MaxUnzippedBytes
	MaxUnzippedBytes = 16
	t.Cleanup(func() { MaxUnzippedBytes = old })
	if _, err := Open(writeFixture(t), fixtureLayout()); !errors.Is(
		err, ErrTooLarge) {
		t.Errorf("a workbook over the cap answered %v, want "+
			"ErrTooLarge", err)
	}
}

// lastRowNumber reads the number of the last row START tag: an end tag,
// another element whose name begins the same way, the word in a cell's
// text and a tag the XML ends inside of are none of them a row. A last
// row that carries no number is answered with 0, and never with the
// number of the row before it, which is not the last.
func TestLastRowNumber(t *testing.T) {
	for xml, want := range map[string]int{
		``:                                       0,
		`<sheetData>`:                            0,
		`<row r="3"><c/></row><row r="12">`:      12,
		`<row r="3"><c/></row><row r="12"/>`:     12,
		`<row r="3"></row>`:                      3,
		`<x:row r="7" spans="1:2">`:              7,
		`<row spans="1:2" r='5'>`:                5,
		"<row\n\tr = \"8\">":                     8,
		`<row r="4"><c><v>row 99</v></c></row>`:  4,
		`<row r="4"></row><rowBreaks count="1">`: 4,
		`<row r="4"></row><row r="9"`:            4,
		`<row r="4"></row><arrow r="9">`:         4,
		`<row>`:                                  0,
		`<row r="x">`:                            0,
		`<row r="4"></row><row spans="1:2">`:     0,
	} {
		if got := lastRowNumber([]byte(xml)); got != want {
			t.Errorf("lastRowNumber(%q) = %d, want %d", xml, got, want)
		}
	}
}

// The scan has to find the same things however the XML is cut into
// writes, since a tag falls across two of them as a matter of course.
// Each sheet is fed whole, in pieces small enough to split every tag,
// and a byte at a time.
func TestSheetScanIsIndifferentToHowTheXMLArrives(t *testing.T) {
	const head = `<?xml version="1.0"?><worksheet><sheetViews/>`
	for _, c := range []struct {
		name   string
		xml    string
		last   int
		merges []string
	}{
		{"rows, then merges",
			head + `<sheetData><row r="1"><c r="A1"/></row>` +
				`<row r="2"/></sheetData><mergeCells count="2">` +
				`<mergeCell ref="B5:D5"/><mergeCell ref="G5:H5"/>` +
				`</mergeCells><pageMargins left="0.7"/></worksheet>`,
			2, []string{"B5:D5", "G5:H5"}},
		{"under a namespace prefix",
			`<x:worksheet><x:sheetData><x:row r="41"></x:row>` +
				`</x:sheetData ><x:mergeCells><x:mergeCell ` +
				`ref="A1:A2"></x:mergeCell></x:mergeCells>` +
				`</x:worksheet>`,
			41, []string{"A1:A2"}},
		{"a ref that is not the first attribute",
			head + `<sheetData><row r="6"/></sheetData><mergeCells>` +
				"<mergeCell xr:uid=\"{1}\"\n ref = 'C1:C9' />" +
				`</mergeCells></worksheet>`,
			6, []string{"C1:C9"}},
		{"no merges",
			head + `<sheetData><row r="3"/></sheetData>` +
				`<hyperlinks><hyperlink ref="A1" r:id="rId1"/>` +
				`</hyperlinks></worksheet>`,
			3, nil},
		{"a sheet without a row",
			head + `<sheetData/><mergeCells><mergeCell ` +
				`ref="A1:B1"/></mergeCells></worksheet>`,
			0, []string{"A1:B1"}},
		{"XML that ends among its rows",
			head + `<sheetData><row r="1"/><row r="2"><c r="A2">`,
			2, nil},
		{"the name in a cell's text",
			head + `<sheetData><row r="5"><c t="str"><v>see ` +
				`sheetData> and x:sheetData/></v></c></row>` +
				`<row r="6"/></sheetData></worksheet>`,
			6, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, step := range []int{len(c.xml), 7, 1} {
				last, merges := scanned(c.xml, step)
				if last != c.last || !slices.Equal(merges, c.merges) {
					t.Errorf("%d bytes at a time: row %d and %q, "+
						"want row %d and %q", step, last, merges,
						c.last, c.merges)
				}
			}
		})
	}
}

// Only the tail of the rows is kept, so a sheet many times that tail
// has to yield the same last row as a short one — and does so in the
// memory of the tail, not of the sheet. A row longer than the tail is
// the one case the scan cannot answer: its start tag has gone by, and
// 0 says so rather than naming the row before it.
func TestSheetScanKeepsOnlyTheTailOfTheRows(t *testing.T) {
	const rows = 40000
	var xml strings.Builder
	xml.WriteString(`<worksheet><sheetData>`)
	for r := 1; r <= rows; r++ {
		fmt.Fprintf(&xml, `<row r="%d"><c r="A%d" t="str"><v>teks `+
			`baris %d</v></c></row>`, r, r, r)
	}
	xml.WriteString(`</sheetData><mergeCells><mergeCell ref="A1:B1"/>` +
		`</mergeCells></worksheet>`)
	if xml.Len() < 8*sheetTailBytes {
		t.Fatalf("the sheet is %d bytes, too short to be trimmed",
			xml.Len())
	}

	var s sheetScan
	held := 0
	for b := []byte(xml.String()); len(b) > 0; {
		n := min(32<<10, len(b))
		_, _ = s.Write(b[:n])
		held = max(held, len(s.buf))
		b = b[n:]
	}
	s.finish()
	if s.last != rows || !slices.Equal(s.merges, []string{"A1:B1"}) {
		t.Errorf("found row %d and %q, want row %d and [A1:B1]",
			s.last, s.merges, rows)
	}
	if held > 3*sheetTailBytes {
		t.Errorf("the scan held %d bytes of a %d byte sheet, want no "+
			"more than the tail it keeps", held, xml.Len())
	}

	// The tail is cut where a write ends, and a write can end anywhere,
	// the middle of the last row included. What is kept has to reach
	// back past that row's start tag, which is the one the end of the
	// rows is held against.
	var head strings.Builder
	head.WriteString(`<worksheet><sheetData>`)
	for r := 1; head.Len() < 3*sheetTailBytes; r++ {
		fmt.Fprintf(&head, `<row r="%d"><c t="str"><v>teks</v></c>`+
			`</row>`, r)
	}
	head.WriteString(`<row r="99999"><c t="str"><v>sebaris `)
	var cut sheetScan
	_, _ = cut.Write([]byte(head.String()))
	_, _ = cut.Write([]byte(strings.Repeat("panjang ", 40) +
		`</v></c></row></sheetData></worksheet>`))
	cut.finish()
	if cut.last != 99999 {
		t.Errorf("a last row cut by a write read as row %d, want 99999",
			cut.last)
	}

	long := `<worksheet><sheetData><row r="1"/><row r="2"><c t="str">` +
		`<v>` + strings.Repeat("x", 3*sheetTailBytes) + `</v></c>` +
		`</row></sheetData></worksheet>`
	if last, _ := scanned(long, 32<<10); last != 0 {
		t.Errorf("a row longer than the tail read as row %d, want 0",
			last)
	}
}

// The part a sheet is in is found by the sheet's name, through the
// workbook's own relationships, and in any letter case, as excelize
// finds it.
func TestSheetPartIsFoundByName(t *testing.T) {
	f := excelize.NewFile()
	t.Cleanup(func() { _ = f.Close() })
	if _, err := f.NewSheet("Catatan"); err != nil {
		t.Fatal(err)
	}
	for sheet, text := range map[string]string{
		fixtureSheet: "pertama", "Catatan": "kedua",
	} {
		if err := f.SetCellValue(sheet, "A1", text); err != nil {
			t.Fatal(err)
		}
	}
	buf, err := f.WriteToBuffer()
	if err != nil {
		t.Fatal(err)
	}
	arc, err := inspect(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}

	for sheet, want := range map[string]string{
		fixtureSheet: "xl/worksheets/sheet1.xml",
		"Catatan":    "xl/worksheets/sheet2.xml",
		"CATATAN":    "xl/worksheets/sheet2.xml",
	} {
		part := arc.part(sheet)
		if part == nil || part.Name != want {
			t.Errorf("sheet %q was found in %v, want %s",
				sheet, part, want)
		}
	}
	if part := arc.part("Lampiran"); part != nil {
		t.Errorf("a sheet the workbook lacks was found in %s",
			part.Name)
	}
}

// A relationship may give a sheet's part from the root of the package,
// or by a path that leaves the workbook's folder and comes back, and
// excelize follows either. The part has to be found the same way here,
// or such a sheet would be read with nothing to hold the reading against.
func TestSheetPartIsFoundThroughAnyTarget(t *testing.T) {
	const written = `Target="worksheets/sheet1.xml"`
	for _, target := range []string{
		"/xl/worksheets/sheet1.xml", "../xl/worksheets/sheet1.xml",
	} {
		book := rebuild(t, fixtureBytes(t),
			func(name, content string) string {
				if name != "xl/_rels/workbook.xml.rels" {
					return content
				}
				if strings.Count(content, written) != 1 {
					t.Fatalf("the relationships lack %s:\n%s",
						written, content)
				}
				return strings.Replace(content, written,
					`Target="`+target+`"`, 1)
			})

		// The premise: excelize reads the sheet through this target.
		wb, err := OpenBytes(book, fixtureLayout())
		if err != nil {
			t.Fatalf("%s: OpenBytes: %v", target, err)
		}
		rows := wb.RowCount()
		_ = wb.Close()

		arc, err := inspect(bytes.NewReader(book), int64(len(book)))
		if err != nil {
			t.Fatalf("%s: %v", target, err)
		}
		found := "no part"
		if part := arc.part(fixtureSheet); part != nil {
			found = part.Name
		}
		if rows != 12 || found != sheetXML {
			t.Errorf("%s: %d rows were read and the sheet found in %s, "+
				"want 12 rows and %s", target, rows, found, sheetXML)
		}
	}
}

// One character XML does not allow, in front of the sheet's tenth row,
// and excelize takes that for the end of the sheet: GetRows returns the
// nine rows above it and no error. That is the premise, checked first,
// because the day excelize reports it the scan is no longer what stands
// between a caller and a sheet cut short.
//
// Open refuses the workbook instead, by either door, saying how far it
// reads.
func TestOpenRefusesASheetItCannotReadToItsEnd(t *testing.T) {
	book := faultAt(t, fixtureBytes(t), 10)

	f, err := excelize.OpenReader(bytes.NewReader(book))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := f.GetRows(fixtureSheet)
	_ = f.Close()
	if err != nil || len(rows) != 9 {
		t.Fatalf("excelize answered %d rows and %v; the premise of "+
			"this test is 9 rows and no error", len(rows), err)
	}

	path := filepath.Join(t.TempDir(), "cut.xlsx")
	if err := os.WriteFile(path, book, 0o600); err != nil {
		t.Fatal(err)
	}
	_, fromFile := Open(path, fixtureLayout())
	_, fromMemory := OpenBytes(book, fixtureLayout())
	for door, err := range map[string]error{
		"Open": fromFile, "OpenBytes": fromMemory,
	} {
		var te *TruncatedError
		if !errors.As(err, &te) {
			t.Errorf("%s answered %v, want a *TruncatedError", door, err)
			continue
		}
		if te.Sheet != fixtureSheet || te.Read != 9 || te.Last != 12 {
			t.Errorf("%s read %d of %d rows of %q, want 9 of 12 of %q",
				door, te.Read, te.Last, te.Sheet, fixtureSheet)
		}
		const want = `only 9 of the 12 rows of sheet "Sheet1" could ` +
			"be read"
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%s: the message lacks %q:\n%v", door, want, err)
		}
	}
}

// A cell reference that is none fails the row it is in. GetRows stops
// there and returns the rows above as the whole sheet; the reading here
// is refused with excelize's own error.
func TestOpenReportsARowItCannotRead(t *testing.T) {
	book := rebuild(t, fixtureBytes(t), func(name, content string) string {
		if name != sheetXML {
			return content
		}
		const cell = `<c r="B10"`
		if strings.Count(content, cell) != 1 {
			t.Fatalf("the sheet has no cell B10:\n%s", content)
		}
		return strings.Replace(content, cell, `<c r="10B"`, 1)
	})

	wb, err := OpenBytes(book, fixtureLayout())
	if err == nil || wb != nil {
		t.Fatalf("answered %v and %v, want the row's error", wb, err)
	}
	if !strings.Contains(err.Error(), `reading sheet "Sheet1"`) {
		t.Errorf("the error does not name the sheet: %v", err)
	}
}

// The merged ranges the scan reads are the ones excelize reports once
// it has built its model of the sheet, which is what Open never asks it
// for.
func TestScanReadsTheMergesExcelizeReports(t *testing.T) {
	book := fixtureBytes(t)

	f, err := excelize.OpenReader(bytes.NewReader(book))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	cells, err := f.GetMergeCells(fixtureSheet)
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, mc := range cells {
		want = append(want, mc[0])
	}

	arc, err := inspect(bytes.NewReader(book), int64(len(book)))
	if err != nil {
		t.Fatal(err)
	}
	last, got, err := arc.scan(fixtureSheet)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(want)
	slices.Sort(got)
	if len(want) != 3 || !slices.Equal(got, want) {
		t.Errorf("the scan read %q, excelize reports %q", got, want)
	}
	if last != 12 {
		t.Errorf("the scan read %d as the last row, want 12", last)
	}
}

// A package that does not say which part holds the sheet leaves the
// scan with nothing to say, and the reading is taken as it is.
func TestScanOfASheetItCannotFind(t *testing.T) {
	arc := &archive{parts: map[string]*zip.File{}}
	last, merges, err := arc.scan(fixtureSheet)
	if last != 0 || merges != nil || err != nil {
		t.Errorf("answered %d, %q and %v, want nothing", last, merges,
			err)
	}
}
