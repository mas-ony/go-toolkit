package xlsx

// Tests for workbook.go, against a real workbook written with excelize: the
// layout resolution, caption folding and matching through merges and the
// group row, the header check, row selection, cell bounds and the reading
// of a column letter, and both hyperlink storage forms. What depends on the
// epoch property, a number format, cached formula results, the working
// directory or contention is in workbook_integration_test.go.

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/mas-ony/go-toolkit/internal/testutil/xlsxtest"
	"github.com/xuri/excelize/v2"
)

// writeFixture builds a workbook with the shape this package exists to
// read, and returns its path:
//
//	     B        C     D     E      F       G        H
//	 5   Location (B5:D5)            Extra   Checks (G5:H5)
//	 6   Cabinet (B6:C6)   No.  Link
//	 7   1        2     3     4
//	 8
//	 9   B-2      24    7     1      -> a real hyperlink
//	10   C-4      25    8            -> a HYPERLINK() formula
//	11                                  (a value in Z and nowhere else)
//	12   D-1      26    9     2
//
// Every path Caption can take is represented: a caption written where it
// is (B and D), one merged across from a neighbour (C), one with no group
// above it (E), one that exists only on the group row (F), and one that is
// both merged and on the group row (G).
func writeFixture(t *testing.T) string {
	t.Helper()
	return xlsxtest.WriteWorkbook(t, "fixture.xlsx", fillFixture)
}

// openWorkbook opens path with layout and closes it when the test ends.
func openWorkbook(t *testing.T, path string, layout Layout) *File {
	t.Helper()
	w, err := Open(path, layout)
	if err != nil {
		t.Fatalf("Open %s: %v", path, err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w
}

// openBytes opens book, a workbook in memory, with layout and closes it
// when the test ends.
func openBytes(t *testing.T, book []byte, layout Layout) *File {
	t.Helper()
	w, err := OpenBytes(book, layout)
	if err != nil {
		t.Fatalf("OpenBytes: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w
}

// openFixture writes the fixture and opens it under layout, closing it when
// the test ends.
func openFixture(t *testing.T, layout Layout) *File {
	t.Helper()
	return openWorkbook(t, writeFixture(t), layout)
}

// fixtureLayout is the geometry every test but the header ones uses.
func fixtureLayout() Layout {
	return Layout{HeaderRow: 6, FirstRow: 9}
}

// fixtureColumns is the contract the fixture satisfies. B and C share a
// caption because they share a merged cell, which is the case a caller
// reading two halves of one heading has to be able to express.
func fixtureColumns() []Column {
	return []Column{
		{Col: "B", Captions: []string{"Cabinet"}, Purpose: "cabinet"},
		{Col: "C", Captions: []string{"Cabinet"}, Purpose: "map"},
		{Col: "D", Captions: []string{"No.", "No"}, Purpose: "number"},
		{Col: "E", Captions: []string{"Link"}, Purpose: "link"},
		{Col: "F", Captions: []string{"Extra"}, Purpose: "extra"},
		{Col: "G", Captions: []string{"Checks"}, Purpose: "checks"},
	}
}

// setLink writes a hyperlink to target into cell of xlsxtest.Sheet, the
// form Hyperlink reads from the sheet's relationships, and fails the test
// if excelize refuses it, as xlsxtest.SetCell does for a value.
func setLink(t *testing.T, f *excelize.File, cell, target string) {
	t.Helper()
	err := f.SetCellHyperLink(xlsxtest.Sheet, cell, target, "External")
	if err != nil {
		t.Fatalf("SetCellHyperLink %s: %v", cell, err)
	}
}

// setFormula writes formula into cell of xlsxtest.Sheet, stored without a
// cached result, and fails the test if excelize refuses it.
func setFormula(t *testing.T, f *excelize.File, cell, formula string) {
	t.Helper()
	if err := f.SetCellFormula(xlsxtest.Sheet, cell, formula); err != nil {
		t.Fatalf("SetCellFormula %s: %v", cell, err)
	}
}

// fillFixture writes the layout writeFixture draws into f.
func fillFixture(t *testing.T, f *excelize.File) {
	t.Helper()

	set := func(cell, value string) {
		t.Helper()
		xlsxtest.SetCell(t, f, cell, value)
	}
	merge := func(from, to, value string) {
		t.Helper()
		set(from, value)
		xlsxtest.MergeCell(t, f, from, to)
	}

	merge("B5", "D5", "Location")
	set("F5", "Extra")
	merge("G5", "H5", "Checks")

	merge("B6", "C6", "Cabinet")
	set("D6", "No.")
	set("E6", "Link")

	set("B7", "1")
	set("C7", "2")
	set("D7", "3")
	set("E7", "4")

	set("B9", "B-2")
	set("C9", "24")
	set("D9", "7")
	set("E9", "1")
	setLink(t, f, "E9", "documents/1.pdf")

	set("B10", "C-4")
	set("C10", "25")
	set("D10", "8")
	setFormula(t, f, "E10", `HYPERLINK("documents/2.pdf","1")`)

	set("Z11", "a note in a column nobody reads")

	set("B12", "D-1")
	set("C12", "26")
	set("D12", "9")
	set("E12", "2")
}

// An empty Layout.Sheet reads the first sheet, and the File reports that
// sheet's name, its rows and the directory the workbook is in.
func TestOpenDefaultsToTheFirstSheet(t *testing.T) {
	wb := openFixture(t, fixtureLayout())

	if got := wb.SheetName(); got != xlsxtest.Sheet {
		t.Errorf("SheetName = %q, want %q", got, xlsxtest.Sheet)
	}
	if got := wb.RowCount(); got < 12 {
		t.Errorf("RowCount = %d, want at least 12", got)
	}
	if wb.Dir() == "" {
		t.Error("Dir must name the directory holding the workbook")
	}
}

// A caption stored once at the top-left of a merged block belongs to every
// column the block covers, and a column with no sub-caption of its own
// takes the group caption above it.
func TestCaptionResolvesThroughMergesAndGroupRow(t *testing.T) {
	wb := openFixture(t, fixtureLayout())

	want := map[string]string{
		"B": "Cabinet", // written in the cell itself
		"C": "Cabinet", // merged across from B6
		"D": "No.",     // its own sub-caption
		"E": "Link",    // its own sub-caption
		"F": "Extra",   // only on the group row
		"G": "Checks",  // merged, and only on the group row
	}
	for col, w := range want {
		if got := wb.Caption(col); got != w {
			t.Errorf("Caption(%q) = %q, want %q", col, got, w)
		}
	}
}

// The fixture passes the header check against its own contract, merged
// and group-row captions included.
func TestVerifyHeadersAcceptsTheFixture(t *testing.T) {
	wb := openFixture(t, fixtureLayout())

	if err := wb.VerifyHeaders(fixtureColumns()); err != nil {
		t.Fatalf("the fixture should match its own contract:\n%v", err)
	}
}

// The check is the only defence against a shifted sheet, so its failure
// path is pinned: every mismatch is reported, not the first.
func TestVerifyHeadersReportsEveryMismatch(t *testing.T) {
	// Row 7 numbers the columns; every caption there is a digit.
	wb := openFixture(t, Layout{HeaderRow: 7, FirstRow: 9})

	err := wb.VerifyHeaders(fixtureColumns())
	if err == nil {
		t.Fatal("expected the column-number row to fail the check")
	}

	var le *LayoutError
	if !errors.As(err, &le) {
		t.Fatalf("error is %T, want a *LayoutError", err)
	}
	if got, want := len(le.Problems), len(fixtureColumns()); got != want {
		t.Errorf("reported %d mismatches, want all %d", got, want)
	}
	if le.Sheet != xlsxtest.Sheet || le.HeaderRow != 7 {
		t.Errorf("error names sheet %q row %d, want %q row 7",
			le.Sheet, le.HeaderRow, xlsxtest.Sheet)
	}
}

// A sheet with a single header row says so, and then a caption on the row
// above is not its to claim.
func TestGroupRowCanBeDisabled(t *testing.T) {
	wb := openFixture(t, Layout{HeaderRow: 6, GroupRow: -1, FirstRow: 9})

	if got := wb.GroupRow(); got != 0 {
		t.Errorf("GroupRow = %d, want 0 when disabled", got)
	}
	if got := wb.Caption("F"); got != "" {
		t.Errorf("Caption(\"F\") = %q, want empty with no group row",
			got)
	}
	// The captions on the header row itself are unaffected, including
	// the one resolved through a merge.
	if got := wb.Caption("C"); got != "Cabinet" {
		t.Errorf("Caption(\"C\") = %q, want Cabinet", got)
	}

	var le *LayoutError
	if !errors.As(wb.VerifyHeaders(fixtureColumns()), &le) {
		t.Fatal("expected the group-row columns to fail the check")
	}
	if len(le.Problems) != 2 {
		t.Errorf("reported %d mismatches, want 2 (F and G)",
			len(le.Problems))
	}
}

// A group row that is not the row above the header is named, and is then
// the row a column with no caption of its own takes one from.
func TestGroupRowCanBeNamed(t *testing.T) {
	// Row 7 numbers the columns B to E, and row 5 is two rows above it.
	wb := openFixture(t, Layout{HeaderRow: 7, GroupRow: 5, FirstRow: 9})

	if got := wb.GroupRow(); got != 5 {
		t.Errorf("GroupRow = %d, want the 5 the layout names", got)
	}
	if got := wb.Caption("F"); got != "Extra" {
		t.Errorf("Caption(\"F\") = %q, want Extra from row 5", got)
	}
	if got := wb.Caption("B"); got != "1" {
		t.Errorf("Caption(\"B\") = %q, want the 1 on the header row",
			got)
	}
}

// A heading that spans every row of the header is one block merged DOWN
// the sheet, with its caption in the block's top cell. The header row
// holds nothing for that column, nor does the group row above it, and
// the caption is read through the block all the same.
func TestCaptionResolvesThroughAMergeDownTheHeader(t *testing.T) {
	book := xlsxtest.WorkbookBytes(t, func(t *testing.T, f *excelize.File) {
		for cell, value := range map[string]string{
			"A1": "Kode", "B3": "Lemari", "A4": "K-1",
		} {
			xlsxtest.SetCell(t, f, cell, value)
		}
		xlsxtest.MergeCell(t, f, "A1", "A3")
	})

	wb := openBytes(t, book, Layout{HeaderRow: 3, FirstRow: 4})
	if got := wb.Caption("A"); got != "Kode" {
		t.Errorf("Caption(\"A\") = %q, want Kode from the top of its "+
			"block", got)
	}
	if got := wb.Caption("B"); got != "Lemari" {
		t.Errorf("Caption(\"B\") = %q, want Lemari", got)
	}
}

// A merged range is the same block from whichever corner it is written,
// and a reference that names no range is passed over.
func TestMergedRangesReadsARangeFromEitherCorner(t *testing.T) {
	rows := [][]string{{"", "Lokasi"}}
	got := mergedRanges([]string{"D1:B1", "B2", "1B:2C"}, rows)
	if len(got) != 1 {
		t.Fatalf("%d blocks were read, want the one range", len(got))
	}
	for col, inside := range map[int]bool{
		1: false, 2: true, 3: true, 4: true, 5: false,
	} {
		if got[0].contains(1, col) != inside {
			t.Errorf("column %d of row 1 inside the block: %t, want %t",
				col, !inside, inside)
		}
	}
	if got[0].value != "Lokasi" {
		t.Errorf("the block holds %q, want the Lokasi of its top-left "+
			"cell", got[0].value)
	}
}

// A row is skipped when every column the caller reads is empty. Row 11
// holds a value in Z, which no column in the contract names, so it counts
// as blank.
func TestDataRowsSkipsRowsWithNothingMapped(t *testing.T) {
	wb := openFixture(t, fixtureLayout())

	got := wb.DataRows(fixtureColumns())
	if want := []int{9, 10, 12}; !slices.Equal(got, want) {
		t.Errorf("DataRows = %v, want %v", got, want)
	}
	if !wb.RowIsBlank(11, fixtureColumns()) {
		t.Error("row 11 has nothing in a mapped column")
	}
	if wb.RowIsBlank(9, fixtureColumns()) {
		t.Error("row 9 is a data row")
	}
}

// A cell of spaces is an empty cell to whoever looks at the sheet, and a
// non-breaking space is one more of them. A row holding nothing else in
// the columns the caller reads is blank, though the grid holds it.
func TestARowOfSpacesIsBlank(t *testing.T) {
	book := xlsxtest.WorkbookBytes(t, func(t *testing.T, f *excelize.File) {
		fillFixture(t, f)
		for cell, value := range map[string]string{
			"B14": "   ", "C14": "\u00a0",
		} {
			xlsxtest.SetCell(t, f, cell, value)
		}
	})

	wb := openBytes(t, book, fixtureLayout())
	if wb.RowCount() != 14 || wb.Cell(14, "B") != "   " {
		t.Fatalf("the grid ends at row %d with %q in B14; the premise "+
			"of this test is row 14 and three spaces",
			wb.RowCount(), wb.Cell(14, "B"))
	}
	if !wb.RowIsBlank(14, fixtureColumns()) {
		t.Error("a row of spaces is not blank")
	}
	got := wb.DataRows(fixtureColumns())
	if want := []int{9, 10, 12}; !slices.Equal(got, want) {
		t.Errorf("DataRows = %v, want %v", got, want)
	}
}

// A row can be in a sheet's XML and hold nothing: a height set on it, or
// a style. One that comes after the data is the last row the XML
// declares, and the reading reaches it, so the sheet is not cut short.
// The grid still ends with the last row that holds anything.
func TestARowHoldingNothingIsNoTruncation(t *testing.T) {
	book := xlsxtest.WorkbookBytes(t, func(t *testing.T, f *excelize.File) {
		fillFixture(t, f)
		if err := f.SetRowHeight(xlsxtest.Sheet, 20, 30); err != nil {
			t.Fatal(err)
		}
	})

	// The premise: row 20 is the last row of the XML.
	arc, err := inspect(bytes.NewReader(book), int64(len(book)))
	if err != nil {
		t.Fatal(err)
	}
	if last, _, err := arc.scan(xlsxtest.Sheet); err != nil || last != 20 {
		t.Fatalf("the sheet's XML ends with row %d (%v), want 20",
			last, err)
	}

	wb := openBytes(t, book, fixtureLayout())
	if got := wb.RowCount(); got != 12 {
		t.Errorf("RowCount = %d, want the 12 rows that hold something",
			got)
	}
}

// Layout.LastRow ends the data range, and one past the end of the sheet is
// clamped to it.
func TestLastDataRowHonoursLayout(t *testing.T) {
	wb := openFixture(t, Layout{HeaderRow: 6, FirstRow: 9, LastRow: 10})

	if got := wb.LastDataRow(); got != 10 {
		t.Errorf("LastDataRow = %d, want 10", got)
	}
	got := wb.DataRows(fixtureColumns())
	if want := []int{9, 10}; !slices.Equal(got, want) {
		t.Errorf("DataRows = %v, want %v", got, want)
	}

	// A last row past the end of the sheet is clamped to it, so a
	// generous value is safe.
	wb = openFixture(t, Layout{HeaderRow: 6, FirstRow: 9, LastRow: 5000})
	if got := wb.LastDataRow(); got != wb.RowCount() {
		t.Errorf("LastDataRow = %d, want the row count %d",
			got, wb.RowCount())
	}
}

// Excelize trims trailing empty cells, so a short row is the ordinary
// case: asking it for a column it does not reach must answer rather than
// panic.
func TestCellOutsideTheSheetIsEmpty(t *testing.T) {
	wb := openFixture(t, fixtureLayout())

	cases := []struct {
		row int
		col string
	}{
		{0, "B"},            // before the first row
		{9999, "B"},         // past the last
		{9, "XFD"},          // past the end of a short row
		{9, "not a column"}, // not a column name at all
	}
	for _, c := range cases {
		if got := wb.Cell(c.row, c.col); got != "" {
			t.Errorf("Cell(%d, %q) = %q, want empty",
				c.row, c.col, got)
		}
	}
	if got := wb.Cell(9, "B"); got != "B-2" {
		t.Errorf("Cell(9, \"B\") = %q, want B-2", got)
	}
}

// ColumnIndex reads a letter in either case up to XFD, and refuses what
// names no column: no letters, other characters, a column past XFD, and
// a run long enough to overflow excelize's sum, which it reports as a
// negative number with no error.
func TestColumnIndex(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		col  string
		want int
	}{
		{"A", 1},
		{"c", 3},
		{"AZ", 52},
		{"XFD", 16384},
	} {
		if got, ok := ColumnIndex(c.col); !ok || got != c.want {
			t.Errorf("ColumnIndex(%q) = %d, %t, want %d", c.col, got, ok,
				c.want)
		}
	}
	for _, col := range []string{"", "A1", " A", "XFE", "AAAA",
		strings.Repeat("A", 15)} {
		if got, ok := ColumnIndex(col); ok {
			t.Errorf("ColumnIndex(%q) = %d, accepted", col, got)
		}
	}
}

// Both storage forms are read, because a workbook can use either and they
// are invisibly different to whoever built it.
func TestHyperlinkReadsBothForms(t *testing.T) {
	wb := openFixture(t, fixtureLayout())

	if got := wb.Hyperlink(9, "E"); got != "documents/1.pdf" {
		t.Errorf("row 9 link = %q, want documents/1.pdf", got)
	}
	if got := wb.Hyperlink(10, "E"); got != "documents/2.pdf" {
		t.Errorf("row 10 link = %q, want documents/2.pdf", got)
	}
	if got := wb.Hyperlink(12, "E"); got != "" {
		t.Errorf("row 12 carries no link, got %q", got)
	}
}

// Link joins the two halves this package holds: the target read from the
// sheet, resolved against the directory holding the workbook rather than
// the process working directory.
func TestLinkResolvesAgainstTheWorkbookDirectory(t *testing.T) {
	wb := openFixture(t, fixtureLayout())

	lt, err := wb.Link(9, "E")
	if err != nil {
		t.Fatalf("Link: %v", err)
	}
	want := filepath.Join(wb.Dir(), "documents", "1.pdf")
	if lt.Path != want {
		t.Errorf("Path = %q, want %q", lt.Path, want)
	}
	if lt.Base != "1.pdf" {
		t.Errorf("Base = %q, want 1.pdf", lt.Base)
	}
	if lt.Foreign {
		t.Error("a target relative to the workbook is not foreign")
	}

	if _, err := wb.Link(12, "E"); !errors.Is(err, ErrNoLink) {
		t.Errorf("no hyperlink on the cell = %v, want ErrNoLink", err)
	}
}

// Only a literal first argument of a HYPERLINK() formula is read; a
// computed one, and any other formula, yields nothing.
func TestHyperlinkFormulaTarget(t *testing.T) {
	cases := map[string]string{
		`=HYPERLINK("documents/1.pdf","1")`: "documents/1.pdf",
		`HYPERLINK("documents/1.pdf")`:      "documents/1.pdf",
		`=hyperlink("a b.pdf","x")`:         "a b.pdf",
		`=HYPERLINK(CONCATENATE(A1),"1")`:   "", // computed
		`=SUM(A1:A2)`:                       "",
		``:                                  "",
	}
	for in, want := range cases {
		if got := hyperlinkFormulaTarget(in); got != want {
			t.Errorf("hyperlinkFormulaTarget(%q) = %q, want %q",
				in, got, want)
		}
	}
}

// Captions are compared after folding, because a sheet spaces them
// however the maintainer felt at the time: a slash with a space after it
// in one column and without in the next, trailing spaces in a third.
// Folding is what lets one accepted spelling cover the lot, so it is
// pinned here. The literals are deliberately multi-byte in one case, a
// non-breaking space and a superscript two — folding must not depend on
// a caption being ASCII.
func TestCaptionFolding(t *testing.T) {
	if got := normaliseCaption("Desa/Kel.   "); got != "desa/kel." {
		t.Errorf("normaliseCaption = %q, want desa/kel.", got)
	}
	if got := normaliseCaption(" LUAS\u00a0(M²)  "); got != "luas (m²)" {
		t.Errorf("normaliseCaption = %q, want luas (m²)", got)
	}
	got := normaliseCaption("  Lokasi   Brankas ")
	if got != "lokasi brankas" {
		t.Errorf("normaliseCaption = %q, want lokasi brankas", got)
	}
	if foldCaption("Dept./ Unit") != foldCaption("Dept./Unit") {
		t.Error("interior spacing around punctuation must not" +
			" distinguish two captions")
	}
	if foldCaption("Link") == foldCaption("File") {
		t.Error("folding must not make genuinely different captions" +
			" equal")
	}
}

// Matches accepts any listed spelling and nothing else, which is what
// keeps a revision that respells a heading from failing the check. The
// "(m2)"/"(m²)" pair is the one that matters: no folding makes the two
// equal, so each is accepted only because it is listed.
func TestMatchesAcceptsEverySpelling(t *testing.T) {
	c := Column{
		Col:      "R",
		Captions: []string{"Luas (m2)", "Luas (m²)", "Luas"},
		Purpose:  "luas",
	}
	for _, caption := range []string{
		"Luas (m2)", "Luas (m²)", "luas", "Luas ", "LUAS (M2)",
	} {
		if !c.Matches(caption) {
			t.Errorf("Matches(%q) = false, want true", caption)
		}
	}
	for _, caption := range []string{"", "Letak", "Luas Tanah"} {
		if c.Matches(caption) {
			t.Errorf("Matches(%q) = true, want false", caption)
		}
	}
}

// OpenBytes reads what Open reads. The fixture is opened both ways and
// every cell of the sheet, every caption and the row selection are held
// against each other; only the directory differs, the one opened from
// memory having none.
func TestOpenBytesReadsWhatOpenReads(t *testing.T) {
	fromFile := openFixture(t, fixtureLayout())
	fromMemory := openBytes(t, fixtureBytes(t), fixtureLayout())

	if fromMemory.SheetName() != fromFile.SheetName() ||
		fromMemory.RowCount() != fromFile.RowCount() {
		t.Fatalf("OpenBytes read sheet %q of %d rows, Open %q of %d",
			fromMemory.SheetName(), fromMemory.RowCount(),
			fromFile.SheetName(), fromFile.RowCount())
	}
	for row := 1; row <= fromFile.RowCount(); row++ {
		for _, col := range []string{"A", "B", "C", "D", "E", "Z"} {
			got, want := fromMemory.Cell(row, col), fromFile.Cell(row, col)
			if got != want {
				t.Errorf("%s%d = %q from memory, %q from the file",
					col, row, got, want)
			}
		}
	}
	for _, c := range fixtureColumns() {
		if got, want := fromMemory.Caption(c.Col),
			fromFile.Caption(c.Col); got != want {
			t.Errorf("Caption(%q) = %q from memory, %q from the file",
				c.Col, got, want)
		}
	}
	if err := fromMemory.VerifyHeaders(fixtureColumns()); err != nil {
		t.Errorf("VerifyHeaders: %v", err)
	}
	if got, want := fromMemory.DataRows(fixtureColumns()),
		fromFile.DataRows(fixtureColumns()); !slices.Equal(got, want) {
		t.Errorf("DataRows = %v from memory, %v from the file", got, want)
	}
	if fromMemory.Dir() != "" {
		t.Errorf("Dir = %q, want none for a workbook in memory",
			fromMemory.Dir())
	}
}

// A workbook in memory has no directory to resolve a relative link
// against, so the link stays as the workbook stores it.
func TestLinkFromMemoryStaysRelative(t *testing.T) {
	wb := openBytes(t, fixtureBytes(t), fixtureLayout())

	lt, err := wb.Link(9, "E")
	if err != nil {
		t.Fatalf("Link: %v", err)
	}
	if want := filepath.Join("documents", "1.pdf"); lt.Path != want ||
		lt.Base != "1.pdf" || lt.Foreign {
		t.Errorf("Link = %+v, want %s relative and not foreign",
			lt, want)
	}
}

// Close lets go of what excelize holds and nothing of the grid, so a
// caller that only reads cells can close as soon as the workbook is
// open. Everything that reads the grid answers after Close as before.
//
// Link says the File is closed, where ErrNoLink would be an answer about
// a cell nobody looked at: row 9 carries a link. Hyperlink has no error
// to say it with, and answers "" without reaching for the workbook.
func TestTheGridOutlivesClose(t *testing.T) {
	wb, err := OpenBytes(fixtureBytes(t), fixtureLayout())
	if err != nil {
		t.Fatalf("OpenBytes: %v", err)
	}
	if err := wb.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if got := wb.Cell(9, "B"); got != "B-2" {
		t.Errorf("Cell(9, \"B\") = %q after Close, want B-2", got)
	}
	if got := wb.Caption("C"); got != "Cabinet" {
		t.Errorf("Caption(\"C\") = %q after Close, want Cabinet", got)
	}
	if err := wb.VerifyHeaders(fixtureColumns()); err != nil {
		t.Errorf("VerifyHeaders after Close: %v", err)
	}
	got := wb.DataRows(fixtureColumns())
	if want := []int{9, 10, 12}; !slices.Equal(got, want) {
		t.Errorf("DataRows = %v after Close, want %v", got, want)
	}

	if got := wb.Hyperlink(9, "E"); got != "" {
		t.Errorf("Hyperlink(9, \"E\") = %q after Close, want none", got)
	}
	if _, err := wb.Link(9, "E"); !errors.Is(err, ErrClosed) {
		t.Errorf("Link(9, \"E\") after Close = %v, want ErrClosed", err)
	}
	if err := wb.Close(); err != nil {
		t.Errorf("a second Close: %v", err)
	}
}

// Close lets go of the workbook itself, and not only of its temporary
// file. Excelize keeps every part it unpacked for as long as its File is
// reachable, closed or not, which can be several times the grid, so a
// File that held on to it would cost a caller that closes early
// everything the closing was for.
func TestCloseLetsGoOfTheWorkbook(t *testing.T) {
	wb, err := OpenBytes(fixtureBytes(t), fixtureLayout())
	if err != nil {
		t.Fatalf("OpenBytes: %v", err)
	}
	freed := make(chan struct{})
	runtime.AddCleanup(wb.file, func(c chan struct{}) { close(c) }, freed)
	if err := wb.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// A collection frees what nothing reaches, and the cleanup runs
	// shortly after it. The File itself stays in use below, so the
	// cleanup runs only if the File has let the workbook go.
	for range 100 {
		runtime.GC()
		select {
		case <-freed:
			if got := wb.Cell(9, "B"); got != "B-2" {
				t.Errorf("Cell(9, \"B\") = %q, want B-2", got)
			}
			return
		case <-time.After(10 * time.Millisecond):
		}
	}
	t.Error("the workbook excelize unpacked is still held after Close")
	runtime.KeepAlive(wb)
}

// HasMacros tells a workbook carrying a VBA project, which is what an
// .xlsm renamed to .xlsx still is, from one that does not.
//
// A project is told by the entry one is kept in by custom, in either
// letter case, and by the workbook's relationships naming a part under
// any other name. A file of that name elsewhere in the package, which
// nothing points at, is not one.
func TestHasMacros(t *testing.T) {
	plain := fixtureBytes(t)
	with := func(name string) []byte {
		return xlsxtest.Rebuild(t, plain, nil, name)
	}
	const end = "</Relationships>"
	renamed := replaced(t, plain, relsXML, end, `<Relationship `+
		`Id="rId99" Target="macros/project.bin" Type="http://`+
		`schemas.microsoft.com/office/2006/relationships/`+
		`vbaProject"/>`+end, "xl/macros/project.bin")
	for name, c := range map[string]struct {
		book []byte
		want bool
	}{
		"a workbook":                   {plain, false},
		"a workbook with a project":    {with("xl/vbaProject.bin"), true},
		"the project in other case":    {with(`xl\VBAProject.bin`), true},
		"a project somewhere else":     {with("docs/vbaProject.bin"), false},
		"a project the workbook names": {renamed, true},
	} {
		wb, err := OpenBytes(c.book, fixtureLayout())
		if err != nil {
			t.Fatalf("%s: OpenBytes: %v", name, err)
		}
		if got := wb.HasMacros(); got != c.want {
			t.Errorf("%s: HasMacros = %t, want %t", name, got, c.want)
		}
		_ = wb.Close()
	}
}

// ColumnsFromA letters its captions from A, past Z into AA, and gives a
// column no purpose, so a failed check names it by its letter alone. A
// column that does state a purpose keeps it in the message.
func TestColumnsFromA(t *testing.T) {
	var captions []string
	for i := range 28 {
		captions = append(captions, fmt.Sprintf("Kolom %d", i+1))
	}
	cols := ColumnsFromA(captions...)
	if len(cols) != 28 {
		t.Fatalf("%d columns, want 28", len(cols))
	}
	for i, want := range map[int]string{0: "A", 25: "Z", 26: "AA",
		27: "AB"} {
		c := cols[i]
		if c.Col != want || !slices.Equal(c.Captions,
			[]string{captions[i]}) || c.Purpose != "" {
			t.Errorf("column %d is %+v, want %s captioned %q",
				i, c, want, captions[i])
		}
	}

	// Row 6 of the fixture is captioned Cabinet, Cabinet, No., Link from
	// B; column A is empty.
	wb := openFixture(t, Layout{HeaderRow: 6, GroupRow: -1, FirstRow: 9})
	var le *LayoutError
	if !errors.As(wb.VerifyHeaders(ColumnsFromA("Kode", "Cabinet")),
		&le) {
		t.Fatal("expected column A to fail the check")
	}
	if want := []string{`column A: expected "Kode", found an ` +
		"empty cell"}; !slices.Equal(le.Problems, want) {
		t.Errorf("problems %q, want %q", le.Problems, want)
	}

	err := wb.VerifyHeaders([]Column{{Col: "D", Captions: []string{
		"Nomor"}, Purpose: "number"}})
	if !errors.As(err, &le) || !slices.Equal(le.Problems, []string{
		`column D (number): expected "Nomor", found "No."`}) {
		t.Errorf("a column with a purpose reads %v", err)
	}
}

// A cell holding what the cell above it holds shares its text, so a
// report that repeats most of a row from the row above costs the text
// once. Nothing else would notice the sharing go, which is why it is
// held here: excelize hands each shared string over as text of its own,
// so without it the two cells below are equal and apart.
func TestRepeatedTextIsShared(t *testing.T) {
	book := xlsxtest.WorkbookBytes(t, func(t *testing.T, f *excelize.File) {
		for _, cell := range []string{"A1", "A2", "A3"} {
			xlsxtest.SetCell(t, f, cell,
				strings.Repeat("Dinas Pendidikan ", 4))
		}
		xlsxtest.SetCell(t, f, "A4", "lain")
	})

	wb := openBytes(t, book, Layout{HeaderRow: 1, FirstRow: 2})
	first, second, third := wb.Cell(1, "A"), wb.Cell(2, "A"),
		wb.Cell(3, "A")
	if first != second || second != third {
		t.Fatalf("the cells read %q, %q and %q", first, second, third)
	}
	if unsafe.StringData(first) != unsafe.StringData(second) ||
		unsafe.StringData(second) != unsafe.StringData(third) {
		t.Error("equal cells in one column, one under the other, " +
			"hold their text apart")
	}
}

// TestColumnString names a column by its letter and purpose, and by its
// letter alone when it states none.
func TestColumnString(t *testing.T) {
	if got := (Column{Col: "C", Purpose: "amount"}).String(); got !=
		"column C (amount)" {
		t.Errorf("named %q", got)
	}
	if got := (Column{Col: "AB"}).String(); got != "column AB" {
		t.Errorf("named %q", got)
	}
}
