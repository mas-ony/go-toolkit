package importer

// Tests for upload.go, against workbooks written with excelize in
// memory.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mas-ony/go-toolkit/internal/testutil"
	"github.com/mas-ony/go-toolkit/internal/testutil/xlsxtest"
	"github.com/mas-ony/go-toolkit/xlsx"
	"github.com/xuri/excelize/v2"
)

// cells is one row of a workbook under construction: the value of each
// cell, by the caption of its column.
type cells map[string]any

// uploadDataset is an UploadDataset of the export layout, which stores
// a record by keeping its rows' codes, for the doc's loop.
type uploadDataset struct{ stored [][]string }

// export is a layout as a report's: a title block over the captions on
// row 3, data from row 4, and a closing Total row.
var export = UploadLayout{
	Layout: xlsx.Layout{HeaderRow: 3, GroupRow: -1},
	Title:  "Laporan Uji",
	Header: []string{"Kode", "Nama", "Jumlah", "Nilai", "Tanggal",
		"Rekening"},
	Required: []string{"Kode", "Nilai"},
	Footer:   "Total",
}

// numbers returns the numbers of the sheet's data rows.
func numbers(s *Sheet) []int {
	var out []int
	for _, r := range s.Rows() {
		out = append(out, r.Number)
	}
	return out
}

// upload writes a workbook laid out as l describes: the title above a
// header row that is not the first, the captions on the header row, and
// one row for each of rows from the layout's first row of data. A nil
// entry leaves its row blank.
func upload(t *testing.T, l UploadLayout, rows ...cells) []byte {
	t.Helper()
	return xlsxtest.WorkbookBytes(t, func(t *testing.T, f *excelize.File) {
		if l.HeaderRow > 1 {
			setCellAt(t, f, 0, 1, strings.ToUpper(l.Title))
		}
		for i, caption := range l.Header {
			setCellAt(t, f, i, l.HeaderRow, caption)
		}
		for n, row := range rows {
			for caption, v := range row {
				i := slices.Index(l.Header, caption)
				if i < 0 {
					t.Fatalf("the layout has no column %q",
						caption)
				}
				setCellAt(t, f, i, l.FirstDataRow()+n, v)
			}
		}
	})
}

// setCellAt writes v into the cell of xlsxtest.Sheet at column col,
// counted from 0 as a header's captions are, and at row, counted from 1
// as a sheet's rows are. excelize names the cell, so a column past Z is
// named as a sheet names it.
func setCellAt(t *testing.T, f *excelize.File, col, row int, v any) {
	t.Helper()
	cell, err := excelize.CoordinatesToCellName(col+1, row)
	if err != nil {
		t.Fatal(err)
	}
	xlsxtest.SetCell(t, f, cell, v)
}

// openUpload opens workbook against l, and fails the test when it will
// not.
func openUpload(t *testing.T, workbook []byte, l UploadLayout) *Sheet {
	t.Helper()
	s, err := OpenUpload(workbook, l)
	if err != nil {
		t.Fatalf("refused: %v", err)
	}
	return s
}

func (d *uploadDataset) Name() string         { return "uji" }
func (d *uploadDataset) Layout() UploadLayout { return export }

func (d *uploadDataset) Import(_ context.Context, rows []*Row) error {
	var codes []string
	for _, r := range rows {
		codes = append(codes, r.Peek("Kode"))
		if r.Text("Kode"); r.Failed() {
			return nil
		}
	}
	d.stored = append(d.stored, codes)
	return nil
}

// TestUploadLayoutCheck holds every arm of Check to its message, and a
// layout that is sound to none.
func TestUploadLayoutCheck(t *testing.T) {
	if err := export.Check(); err != nil {
		t.Errorf("the export layout: %v", err)
	}
	header := func(row int) xlsx.Layout {
		return xlsx.Layout{HeaderRow: row, GroupRow: -1}
	}
	for _, c := range []struct {
		name   string
		layout UploadLayout
		want   string
	}{
		{"no header", UploadLayout{}, "it has no header row"},
		{"no captions", UploadLayout{Layout: header(1)},
			"it has no header row"},
		{"data on the header row", UploadLayout{
			Layout: xlsx.Layout{HeaderRow: 3, FirstRow: 3},
			Header: []string{"A"}},
			"its data starts above its header row"},
		{"group captions below the header", UploadLayout{
			Layout: xlsx.Layout{HeaderRow: 3, GroupRow: 4},
			Header: []string{"A"}},
			"its group captions are not above its header row"},
		{"a caption twice", UploadLayout{Layout: header(1),
			Header: []string{"A", "A"}},
			`its header lists "A" twice`},
		{"an empty caption", UploadLayout{Layout: header(1),
			Header: []string{"A", ""}}, `lists "" twice, or is`},
		{"a required column not listed", UploadLayout{
			Layout: header(1), Header: []string{"A"},
			Required: []string{"B"}},
			`it requires "B", which its header does not list`},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := c.layout.Check()
			if err == nil ||
				!strings.Contains(err.Error(), c.want) {
				t.Errorf("answered %v, want %q", err, c.want)
			}
		})
	}
}

// TestFirstDataRow is the row below the header unless the layout names
// another.
func TestFirstDataRow(t *testing.T) {
	below := UploadLayout{Layout: xlsx.Layout{HeaderRow: 5}}
	if got := below.FirstDataRow(); got != 6 {
		t.Errorf("answered %d, want 6", got)
	}
	named := UploadLayout{Layout: xlsx.Layout{HeaderRow: 7,
		FirstRow: 9}}
	if got := named.FirstDataRow(); got != 9 {
		t.Errorf("answered %d, want 9", got)
	}
}

// TestOpenUploadKeepsTheDataRows opens a report with a blank row among
// its data and a closing row in another letter case. The data rows are
// kept in sheet order, and the blank and the closing rows are not data.
func TestOpenUploadKeepsTheDataRows(t *testing.T) {
	s := openUpload(t, upload(t, export,
		cells{"Kode": "A", "Nilai": 1},
		nil,
		cells{"Kode": "B", "Nilai": 2},
		cells{"Kode": "TOTAL", "Nilai": 3},
	), export)
	if got := numbers(s); !slices.Equal(got, []int{4, 6}) {
		t.Errorf("data rows %v, want 4 and 6", got)
	}
	if s.Name() != xlsxtest.Sheet {
		t.Errorf("read sheet %q, want %s", s.Name(), xlsxtest.Sheet)
	}
	if s.Rows()[0] != s.Rows()[0] {
		t.Error("Rows did not return the same rows twice")
	}
}

// TestOpenUploadRefusesAnotherHeader opens workbooks whose header row is
// not the layout's. Each is an *xlsx.LayoutError naming every column
// that differs, by its letter, and no Sheet.
func TestOpenUploadRefusesAnotherHeader(t *testing.T) {
	swapped := export
	swapped.Header = slices.Clone(export.Header)
	swapped.Header[0], swapped.Header[1] = "Nama", "Kode"

	lower := export
	lower.HeaderRow = 4

	for name, workbook := range map[string][]byte{
		"two columns have changed places": upload(t, swapped),
		"the captions are a row lower":    upload(t, lower),
	} {
		t.Run(name, func(t *testing.T) {
			s, err := OpenUpload(workbook, export)
			var mismatch *xlsx.LayoutError
			if s != nil || !errors.As(err, &mismatch) {
				t.Fatalf("answered %v and %v, want an "+
					"*xlsx.LayoutError", s, err)
			}
			if mismatch.HeaderRow != 3 ||
				len(mismatch.Problems) < 2 ||
				!strings.HasPrefix(mismatch.Problems[0],
					`column A: expected "Kode", found `) {
				t.Errorf("row %d, problems %q; want row 3 "+
					"and two or more from column A",
					mismatch.HeaderRow, mismatch.Problems)
			}
		})
	}
}

// TestUploadCaptionsAreMatchedLoosely writes the header in another
// letter case and with stray spaces. The workbook is still the
// layout's.
func TestUploadCaptionsAreMatchedLoosely(t *testing.T) {
	loose := export
	loose.Header = []string{"KODE", " nama ", "Jumlah", "NILAI",
		"tanggal", "Rekening"}
	s := openUpload(t, upload(t, loose,
		cells{"KODE": "A", "NILAI": 1}), export)
	if got := s.Rows()[0].Text("Kode"); got == nil || *got != "A" {
		t.Errorf("read %v, want A", got)
	}
}

// TestOpenUploadPassesOnWhatXlsxRefuses opens content that is no
// workbook. The error is the one xlsx answers with, as it came.
func TestOpenUploadPassesOnWhatXlsxRefuses(t *testing.T) {
	_, err := OpenUpload([]byte("Kode,Nama\nA,B\n"), export)
	if !errors.Is(err, xlsx.ErrNotWorkbook) {
		t.Errorf("answered %v, want xlsx.ErrNotWorkbook", err)
	}
}

// TestOpenUploadRefusesMacros opens a workbook that carries a VBA
// project.
func TestOpenUploadRefusesMacros(t *testing.T) {
	plain := upload(t, export, cells{"Kode": "A", "Nilai": 1})
	macros := xlsxtest.Rebuild(t, plain, nil, "xl/vbaProject.bin")

	_, err := OpenUpload(macros, export)
	if !errors.Is(err, ErrMacros) {
		t.Errorf("answered %v, want ErrMacros", err)
	}
}

// TestUploadGroupedHeader opens a sheet whose header is two rows: a
// caption merged down both stands on the upper one, a group caption is
// merged across its columns, and a row that numbers the columns stands
// between the header and the data.
func TestUploadGroupedHeader(t *testing.T) {
	l := UploadLayout{
		Layout: xlsx.Layout{HeaderRow: 3, FirstRow: 5},
		Title:  "Uji",
		Header: []string{"No.", "Nomor", "Nama"},
	}
	book := xlsxtest.WorkbookBytes(t,
		func(t *testing.T, f *excelize.File) {
			do := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			const sheet = xlsxtest.Sheet
			xlsxtest.SetCell(t, f, "A2", "No.")
			xlsxtest.MergeCell(t, f, "A2", "A3")
			xlsxtest.SetCell(t, f, "B2", "TBP")
			xlsxtest.MergeCell(t, f, "B2", "C2")
			xlsxtest.SetCell(t, f, "B3", "Nomor")
			xlsxtest.SetCell(t, f, "C3", "Nama")
			do(f.SetSheetRow(sheet, "A4",
				&[]any{"(1)", "(2)", "(3)"}))
			do(f.SetSheetRow(sheet, "A5",
				&[]any{1, "TBP/1", "PPH 21"}))
		})

	s := openUpload(t, book, l)
	if got := numbers(s); !slices.Equal(got, []int{5}) {
		t.Fatalf("data rows %v, want 5", got)
	}
	if got := s.Rows()[0].Peek("Nomor"); got != "TBP/1" {
		t.Errorf("read %q, want TBP/1", got)
	}
}

// TestUploadReaders reads a row with every reader, from cells that read,
// and one with cells that do not: a required cell fails the row, and any
// other is left nil with a warning. Each note names its column by letter
// and caption.
func TestUploadReaders(t *testing.T) {
	when := time.Date(2026, time.March, 4, 0, 0, 0, 0, time.UTC)
	s := openUpload(t, upload(t, export,
		cells{"Kode": " A-1 ", "Nama": "baris 1_x000D_\nbaris 2",
			"Jumlah": 12, "Nilai": 1500.25, "Tanggal": when,
			"Rekening": "5.1.02.01.01.0024"},
		cells{"Nama": "x", "Jumlah": "dua", "Nilai": "seratus",
			"Tanggal": "segera", "Rekening": "5.1.x"},
	), export)
	good, bad := s.Rows()[0], s.Rows()[1]

	text := good.Text("Kode")
	nama := good.Text("Nama")
	jumlah := good.Int("Jumlah")
	nilai := good.Decimal("Nilai")
	tanggal := good.Date("Tanggal")
	rekening := good.Code("Rekening")
	got := fmt.Sprintf("%s|%q|%d|%s|%s|%d", *text, *nama, *jumlah,
		nilai, tanggal.Format(time.DateOnly), *rekening)
	const want = `A-1|"baris 1\r\nbaris 2"|12|1500.25|2026-03-04|` +
		"510201010024"
	if got != want {
		t.Errorf("read %s\nwant %s", got, want)
	}
	if good.Failed() || len(good.Warnings()) != 0 {
		t.Errorf("the good row noted %q and %q", good.Errors(),
			good.Warnings())
	}

	if bad.Text("Kode") != nil || bad.Int("Jumlah") != nil ||
		bad.Decimal("Nilai") != nil || bad.Date("Tanggal") != nil ||
		bad.Code("Rekening") != nil {
		t.Error("a cell that would not read was not left nil")
	}
	if !bad.Failed() {
		t.Error("the bad row has not failed")
	}
	wantErrs := []string{
		"column A (Kode) is empty",
		`column D (Nilai): "seratus" is not a number`,
	}
	wantWarns := []string{
		`column C (Jumlah): "dua" is not a number — left empty`,
		`column E (Tanggal): "segera" is not a recognised date ` +
			"— left empty",
		`column F (Rekening): "5.1.x" is not a dotted code of at ` +
			"most 18 digits — left empty",
	}
	if !slices.Equal(bad.Errors(), wantErrs) {
		t.Errorf("errors:\n%q\nwant:\n%q", bad.Errors(), wantErrs)
	}
	if !slices.Equal(bad.Warnings(), wantWarns) {
		t.Errorf("warnings:\n%q\nwant:\n%q", bad.Warnings(),
			wantWarns)
	}
}

// TestUploadReadersFollowTheTableRule reads one cell that will not read
// through a Row and through Table.Apply. Both say the same of it, as a
// warning and, for a required column, as the row's error.
func TestUploadReadersFollowTheTableRule(t *testing.T) {
	s := openUpload(t, upload(t, export,
		cells{"Kode": "A", "Jumlah": "dua", "Nilai": "seratus"}),
		export)
	r := s.Rows()[0]
	r.Int("Jumlah")
	r.Decimal("Nilai")

	src := newGrid().set(4, "C", "dua").set(4, "D", "seratus")
	warns, _ := Table[row]{Int("C", "Jumlah", dstCount)}.Apply(
		src, 4, &row{})
	_, fatal := Table[row]{Must(Decimal("D", "Nilai", dstAmount))}.
		Apply(src, 4, &row{})

	if !slices.Equal(r.Warnings(), warns) {
		t.Errorf("the row warns %q, the table %q", r.Warnings(),
			warns)
	}
	if fatal == nil || !slices.Equal(r.Errors(),
		[]string{fatal.Error()}) {
		t.Errorf("the row fails with %q, the table with %v",
			r.Errors(), fatal)
	}
}

// TestCodeRefusesTooManyDigits reads a code one digit over the cap.
func TestCodeRefusesTooManyDigits(t *testing.T) {
	long := strings.Repeat("1", xlsx.MaxCodeDigits)
	s := openUpload(t, upload(t, export,
		cells{"Kode": "A", "Nilai": 1, "Rekening": long},
		cells{"Kode": "A", "Nilai": 1, "Rekening": long + "1"},
	), export)
	if s.Rows()[0].Code("Rekening") == nil {
		t.Error("a code of MaxCodeDigits digits did not read")
	}
	if s.Rows()[1].Code("Rekening") != nil {
		t.Error("a code over MaxCodeDigits digits read")
	}
}

// TestUploadDecimalDotGrouped reads an amount in Indonesian notation,
// and refuses one in any other.
func TestUploadDecimalDotGrouped(t *testing.T) {
	s := openUpload(t, upload(t, export,
		cells{"Kode": "A", "Nilai": "1.300.000,50"},
		cells{"Kode": "A", "Nilai": "1,300,000.50"},
	), export)
	got := s.Rows()[0].DecimalDotGrouped("Nilai")
	if got == nil || got.String() != "1300000.5" {
		t.Errorf("read %v, want 1300000.5", got)
	}
	if s.Rows()[1].DecimalDotGrouped("Nilai") != nil ||
		!s.Rows()[1].Failed() {
		t.Error("an amount in another notation read")
	}
}

// TestUploadFail fails a row the readers passed, as a whole and for one
// of its cells, and the notes come back as copies the caller cannot
// change the row through.
func TestUploadFail(t *testing.T) {
	s := openUpload(t, upload(t, export,
		cells{"Kode": "A", "Nilai": 1}), export)
	r := s.Rows()[0]
	r.Text("Kode")
	if r.Failed() {
		t.Fatal("the row failed before Fail")
	}
	r.Fail("the table refused the row: duplicate key")
	r.FailCell("Kode", `"A" is no code the dataset knows`)
	r.FailEmpty("Nama")
	want := []string{"the table refused the row: duplicate key",
		`column A (Kode): "A" is no code the dataset knows`,
		"column B (Nama) is empty"}
	if !r.Failed() || !slices.Equal(r.Errors(), want) {
		t.Fatalf("failed %t with %q, want %q", r.Failed(),
			r.Errors(), want)
	}
	errs := r.Errors()
	errs[0] = "changed"
	if r.Errors()[0] == "changed" {
		t.Error("Errors handed out the row's own notes")
	}
}

// TestUploadReadingAnUnlistedCaptionPanics reads a column the layout
// does not list. The panic names the dataset and the caption.
func TestUploadReadingAnUnlistedCaptionPanics(t *testing.T) {
	s := openUpload(t, upload(t, export,
		cells{"Kode": "A", "Nilai": 1}), export)
	for name, read := range map[string]func(*Row){
		"Text":     func(r *Row) { r.Text("Nama Lengkap") },
		"FailCell": func(r *Row) { r.FailCell("Nama Lengkap", "x") },
	} {
		t.Run(name, func(t *testing.T) {
			p := testutil.CapturePanic(func() { read(s.Rows()[0]) })
			if p == nil {
				t.Fatal("the cell was read")
			}
			const want = `the Laporan Uji dataset reads column ` +
				`"Nama Lengkap", which its layout does not list`
			if got := fmt.Sprint(p); got != want {
				t.Errorf("panicked with %q, want %q", got, want)
			}
		})
	}
}

// TestUploadLookingNotesNothing peeks at a row whose required cell is
// empty, and probes one whose cell will not read. Neither leaves a note
// on the row, which then reads as it would have.
func TestUploadLookingNotesNothing(t *testing.T) {
	s := openUpload(t, upload(t, export,
		cells{"Kode": "A", "Nama": "Ani", "Nilai": 1},
		cells{"Nilai": "seratus"},
		cells{"Kode": "C", "Nilai": 3},
	), export)
	first, second, third := s.Rows()[0], s.Rows()[1], s.Rows()[2]

	if got := second.Peek("Kode"); got != "" {
		t.Errorf("peeked %q, want an empty cell", got)
	}
	if second.Probe().Decimal("Nilai") != nil {
		t.Error("the probe read a bad cell")
	}
	if second.Failed() || len(second.Warnings()) != 0 {
		t.Errorf("looking noted %q and %q", second.Errors(),
			second.Warnings())
	}

	if got := second.PeekAbove("Nama"); got != "Ani" {
		t.Errorf("peeked above %q, want Ani", got)
	}
	if got := third.PeekAbove("Nama"); got != "Ani" {
		t.Errorf("peeked above %q from two rows down, want Ani",
			got)
	}
	if got := first.PeekAbove("Nama"); got != "" {
		t.Errorf("peeked above the data and found %q", got)
	}
}

// TestUploadDatasetLoop drives a dataset as the package doc does, a row
// at a time. The row that fails its checks is not stored.
func TestUploadDatasetLoop(t *testing.T) {
	var ds UploadDataset = &uploadDataset{}
	s := openUpload(t, upload(t, ds.Layout(),
		cells{"Kode": "A", "Nilai": 1},
		cells{"Nilai": 2},
		cells{"Kode": "C", "Nilai": 3},
	), ds.Layout())
	for _, row := range s.Rows() {
		if err := ds.Import(context.Background(),
			[]*Row{row}); err != nil {
			t.Fatal(err)
		}
	}
	got := ds.(*uploadDataset).stored
	if fmt.Sprint(got) != "[[A] [C]]" {
		t.Errorf("stored %v, want A and C", got)
	}
}
