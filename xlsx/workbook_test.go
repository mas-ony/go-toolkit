package xlsx

import (
	"errors"
	"path/filepath"
	"slices"
	"testing"

	"github.com/xuri/excelize/v2"
)

// fixtureSheet is the sheet excelize puts in a new file.
const fixtureSheet = "Sheet1"

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

func openFixture(t *testing.T, layout Layout) *File {
	t.Helper()

	wb, err := Open(writeFixture(t), layout)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = wb.Close() })
	return wb
}

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
// is (B), one merged across from a neighbour (C), one with no group above
// it (D and E), one that exists only on the group row (F), and one that is
// both merged and on the group row (G).
func writeFixture(t *testing.T) string {
	t.Helper()

	f := excelize.NewFile()
	t.Cleanup(func() { _ = f.Close() })

	set := func(cell, value string) {
		t.Helper()
		if err := f.SetCellValue(fixtureSheet, cell, value); err != nil {
			t.Fatalf("SetCellValue %s: %v", cell, err)
		}
	}
	merge := func(from, to, value string) {
		t.Helper()
		set(from, value)
		if err := f.MergeCell(fixtureSheet, from, to); err != nil {
			t.Fatalf("MergeCell %s:%s: %v", from, to, err)
		}
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
	err := f.SetCellHyperLink(
		fixtureSheet, "E9", "documents/1.pdf", "External")
	if err != nil {
		t.Fatalf("SetCellHyperLink: %v", err)
	}

	set("B10", "C-4")
	set("C10", "25")
	set("D10", "8")
	err = f.SetCellFormula(
		fixtureSheet, "E10", `HYPERLINK("documents/2.pdf","1")`)
	if err != nil {
		t.Fatalf("SetCellFormula: %v", err)
	}

	set("Z11", "a note in a column nobody reads")

	set("B12", "D-1")
	set("C12", "26")
	set("D12", "9")
	set("E12", "2")

	path := filepath.Join(t.TempDir(), "fixture.xlsx")
	if err := f.SaveAs(path); err != nil {
		t.Fatalf("SaveAs: %v", err)
	}
	return path
}

func TestOpenDefaultsToTheFirstSheet(t *testing.T) {
	wb := openFixture(t, fixtureLayout())

	if got := wb.SheetName(); got != fixtureSheet {
		t.Errorf("SheetName = %q, want %q", got, fixtureSheet)
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
	if le.Sheet != fixtureSheet || le.HeaderRow != 7 {
		t.Errorf("error names sheet %q row %d, want %q row 7",
			le.Sheet, le.HeaderRow, fixtureSheet)
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
// pinned here. The literals are deliberately multi-byte in one case —
// folding must not depend on a caption being ASCII.
func TestCaptionFolding(t *testing.T) {
	if got := normaliseCaption("Desa/Kel.   "); got != "desa/kel." {
		t.Errorf("normaliseCaption = %q, want desa/kel.", got)
	}
	got := normaliseCaption("  Lokasi   Brankas ")
	if got != "lokasi brankas" {
		t.Errorf("normaliseCaption = %q, want lokasi brankas", got)
	}
	if foldCaption("Kab./ Kota") != foldCaption("Kab./Kota") {
		t.Error("interior spacing around punctuation must not" +
			" distinguish two captions")
	}
	if foldCaption("Link") == foldCaption("Berkas") {
		t.Error("folding must not make genuinely different captions" +
			" equal")
	}
}

// Matches accepts any listed spelling and nothing else, which is what
// keeps a revision that respells a heading from failing the check. The
// "(m2)"/"(m²)" pair is the one that matters: the two differ by a
// multi-byte rune, so a comparison written over bytes rather than runes
// would still pass every other case here.
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
