package importer

// An import a service runs on an uploaded export: the kind of workbook
// it is, the sheet read against it, and the rows a dataset reads by the
// caption of their columns.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/mas-ony/go-toolkit/datetime"
	"github.com/mas-ony/go-toolkit/xlsx"
	"github.com/shopspring/decimal"
)

// uploadColumn is where a caption of the layout stands in the sheet.
type uploadColumn struct {
	xlsx.Column

	// required says a row cannot be stored without the cell.
	required bool
}

// Row is one data row of a Sheet, which a dataset reads cell by cell
// under the captions of its layout.
//
// Each reader returns nil for an empty cell, which is stored as a NULL,
// and notes what it finds wrong with the cell instead of returning an
// error, by the rule Table.Apply follows:
//
//   - A required cell that will not read, or is empty, fails the row,
//     and Failed then reports true.
//   - Any other cell that will not read is left nil, with a warning.
//
// A reader panics for a caption the layout does not list, which is a
// mistake in the dataset and not in the workbook.
//
// A Row is not safe for concurrent use: its notes are appended to as it
// is read.
type Row struct {
	// Number is the row's number in the sheet.
	Number int

	sheet *Sheet

	// errs say why the row is not stored, and warns which of its cells
	// are stored empty because they would not read.
	errs, warns []string
}

// Sheet is the sheet of an upload, read to its end and checked against
// an UploadLayout.
type Sheet struct {
	// book is the workbook as xlsx read it, which keeps the text of
	// every cell of the sheet.
	book *xlsx.File

	// rows are the sheet's data rows, top to bottom, and first is the
	// number of the row the data starts on.
	rows  []*Row
	first int

	// title is the layout's, for the panic of a dataset that reads a
	// column its layout does not list.
	title string

	// cols holds the layout's columns by their captions, as the layout
	// spells them, each named by its caption.
	cols map[string]uploadColumn
}

// UploadLayout describes the sheet an UploadDataset reads: the first
// sheet of the workbook unless Sheet names another, with one row of
// captions and a row of data under it for every record.
//
// The columns stand side by side from column A, as a program writes
// them, so the captions alone say where each one is. A sheet kept by
// hand, whose columns move and whose captions are spelled several ways,
// wants a Table instead.
type UploadLayout struct {
	// Layout is where the captions and the data are, as xlsx reads a
	// sheet. A FirstRow of zero means the row below HeaderRow here.
	// GroupRow is xlsx's: zero for a header of two rows, the group
	// captions on the row above HeaderRow, and -1 for a header of one.
	// A report whose header row has a title block above it sets -1, or
	// a caption missing from the header row is looked for in the title.
	xlsx.Layout

	// Title is what the people who export the workbook call it:
	// "Laporan Realisasi Per Dokumen". A service names it when it tells
	// a client the workbook is not that export.
	Title string

	// Header lists the captions of the header row, left to right from
	// column A. A workbook whose header row carries others is refused,
	// and a Row reads a cell by the caption of its column.
	Header []string

	// Required lists the captions of the columns a row cannot be
	// stored without: the columns the table declares NOT NULL.
	Required []string

	// Footer is what a closing row carries in column A, such as the
	// "Total" under a report, in any letter case. That row is not data.
	// It is empty for a sheet that has none.
	Footer string
}

// UploadDataset is one kind of export an upload is imported as: what its
// sheet looks like, and how its rows become rows of a table.
//
// It is the upload's counterpart of Dataset, which a command runs over a
// file: a service holds one per kind of export it takes, and the upload
// names the one it is for. The service opens the workbook with
// OpenUpload against the dataset's Layout, then hands the sheet's rows to
// Import, one record at a time.
type UploadDataset interface {
	// Name is what an upload names the dataset by. A service picks how:
	// a form field, a path segment. It is in lower case, and no two
	// datasets of one service share it.
	Name() string

	// Layout describes the sheet the dataset reads.
	Layout() UploadLayout

	// Import parses the rows of one record and stores them: a single
	// row, or the rows a service gathers into one, such as the lines
	// of one document, in sheet order.
	//
	// A record whose rows fail their checks is not stored: Row.Failed
	// reports it once the cells are read, and Import then returns nil.
	// The error it returns is the store's: one wrapping
	// database.ErrRejected for a record the table refuses, which a
	// service records against its rows and goes on, and any other the
	// database's own, which would fail every record after it the same
	// way.
	Import(ctx context.Context, rows []*Row) error
}

// ErrMacros is what OpenUpload answers for a workbook that carries a VBA
// project, whatever the file is named. It says nothing but what the
// workbook holds, and is safe to show whoever sent it.
var ErrMacros = errors.New("the workbook carries macros")

// readCell reads the cell of row r under caption with parse, and notes
// on r what is wrong with it; see Row.
func readCell[T any](
	r *Row, caption string, parse func(cell string) (*T, error),
) *T {
	col := r.column(caption)
	v, err := parse(r.sheet.book.Cell(r.Number, col.Col))
	if err != nil {
		fatal, warning := cellFault(col.Column, col.required, err)
		if fatal != nil {
			r.errs = append(r.errs, fatal.Error())
		} else {
			r.warns = append(r.warns, warning)
		}
		return nil
	}
	if v == nil && col.required {
		r.FailEmpty(caption)
	}
	return v
}

// OpenUpload reads the sheet of workbook as the layout describes it: it
// checks the header row, and keeps every row under it that holds data. A
// blank row and the layout's footer row are passed over.
//
// The whole sheet is read before OpenUpload returns, so a caller that
// stores nothing until it has a Sheet leaves no row behind when it
// fails. It fails with:
//
//   - ErrMacros for a workbook that carries a VBA project.
//   - What xlsx.OpenBytes fails with for content it will not read:
//     xlsx.ErrNotWorkbook, xlsx.ErrTooLarge or an *xlsx.TruncatedError,
//     each safe to show whoever sent the workbook, or an error wrapping
//     excelize's own, which can name a temporary file on the host.
//   - An *xlsx.LayoutError when the header row is not the layout's,
//     naming every column that carries another caption.
//
// The workbook is closed as soon as it is read, which lets go of
// excelize's copy of it, and of the temporary file excelize unpacks a
// sheet of more than 16 MiB of XML into, before the first row is stored.
// The text of the cells stays readable, and that is all a row is read
// from. A failure to remove that temporary file costs the import
// nothing, and does not fail OpenUpload.
//
// Captions are compared by xlsx.Column.Matches, which ignores letter case
// and spacing. The check is what makes reading by caption safe: a column
// that has moved would otherwise import cleanly, as a value that is
// simply wrong.
func OpenUpload(workbook []byte, l UploadLayout) (*Sheet, error) {
	layout := l.Layout
	layout.FirstRow = l.FirstDataRow()
	book, err := xlsx.OpenBytes(workbook, layout)
	if err != nil {
		return nil, err
	}
	_ = book.Close()
	if book.HasMacros() {
		return nil, ErrMacros
	}

	header := xlsx.ColumnsFromA(l.Header...)
	if err := book.VerifyHeaders(header); err != nil {
		return nil, err
	}

	s := &Sheet{
		book:  book,
		first: layout.FirstRow,
		title: l.Title,
		cols:  make(map[string]uploadColumn, len(header)),
	}
	for i, c := range header {
		c.Purpose = l.Header[i]
		s.cols[l.Header[i]] = uploadColumn{Column: c}
	}
	for _, caption := range l.Required {
		c := s.cols[caption]
		c.required = true
		s.cols[caption] = c
	}
	for _, n := range book.DataRows(header) {
		footer := xlsx.Trim(book.Cell(n, header[0].Col))
		if l.Footer != "" && strings.EqualFold(footer, l.Footer) {
			continue
		}
		s.rows = append(s.rows, &Row{Number: n, sheet: s})
	}
	return s, nil
}

// column returns the column of the layout captioned caption, and panics
// for one the layout does not list; see Row.
func (r *Row) column(caption string) uploadColumn {
	col, listed := r.sheet.cols[caption]
	if !listed {
		panic(fmt.Sprintf("the %s dataset reads column %q, which "+
			"its layout does not list", r.sheet.title, caption))
	}
	return col
}

// Text reads a cell as text: trimmed, with the _x000D_ escapes of a
// workbook's XML decoded.
func (r *Row) Text(caption string) *string {
	return readCell(r, caption, func(cell string) (*string, error) {
		return xlsx.Str(xlsx.Unescape(cell)), nil
	})
}

// Int reads a cell as a whole number; see xlsx.Int.
func (r *Row) Int(caption string) *int {
	return readCell(r, caption, xlsx.Int)
}

// Code reads a cell that holds a dotted code as the number its digits
// spell; see xlsx.Code.
func (r *Row) Code(caption string) *int {
	return readCell(r, caption, xlsx.Code)
}

// Decimal reads a cell the workbook stores as a number; see
// xlsx.Decimal for the separators it takes in one that was typed.
func (r *Row) Decimal(caption string) *decimal.Decimal {
	return readCell(r, caption, xlsx.Decimal)
}

// DecimalDotGrouped reads a cell that holds a number as text in
// Indonesian notation, 1.234.567,89, and refuses any other form.
func (r *Row) DecimalDotGrouped(caption string) *decimal.Decimal {
	return readCell(r, caption, xlsx.DecimalDotGrouped)
}

// Date reads a cell as a date: an Excel serial under the workbook's own
// epoch, or a date written out; see xlsx.Date.
func (r *Row) Date(caption string) *datetime.Datetime {
	date1904 := r.sheet.book.Date1904()
	return readCell(r, caption,
		func(cell string) (*datetime.Datetime, error) {
			return xlsx.Date(cell, date1904)
		})
}

// Failed reports whether the row has failed: a cell read so far, Fail,
// FailCell or FailEmpty says it cannot be stored. A dataset asks once it
// has read them all, and does not store a row that has.
func (r *Row) Failed() bool { return len(r.errs) > 0 }

// Fail fails the row with note, which says why it is not stored. It is
// for what is wrong with the row as a whole: a lookup that finds
// nothing, a table that refuses it.
func (r *Row) Fail(note string) { r.errs = append(r.errs, note) }

// FailCell fails the row for what is in the cell under caption, which
// the readers took but no rule of the dataset accepts: a word that is
// not one of the values the column holds. The note names the column as
// a reader's would: "column D (Jenis Transaksi): reason".
func (r *Row) FailCell(caption, reason string) {
	r.Fail(fmt.Sprintf("%s: %s", r.column(caption), reason))
}

// FailEmpty fails the row for the cell under caption being empty, as a
// reader fails it for a required column: "column B (Nomor) is empty".
// It is for a cell a dataset cannot store the row without although the
// layout does not require it, such as one merged down from a row above,
// which the dataset reads from there.
func (r *Row) FailEmpty(caption string) {
	r.Fail(r.column(caption).String() + " is empty")
}

// Errors returns why the row is not stored, in the order the reasons
// arose. Each names its column where it has one.
func (r *Row) Errors() []string {
	return append([]string(nil), r.errs...)
}

// Warnings returns which of the row's cells are stored empty because
// they would not read, in the order they were read.
func (r *Row) Warnings() []string {
	return append([]string(nil), r.warns...)
}

// Probe returns a copy of the row that notes nothing on it: what the
// copy's readers find wrong stays on the copy. It is for a dataset that
// looks at a row before the row is read, to tell whether it is one the
// dataset stores, or which rows belong together.
func (r *Row) Probe() *Row {
	return &Row{Number: r.Number, sheet: r.sheet}
}

// Peek returns the text of a cell as Text reads it, or "" for an empty
// one, and notes nothing on the row.
func (r *Row) Peek(caption string) string {
	if text := r.Probe().Text(caption); text != nil {
		return *text
	}
	return ""
}

// PeekAbove is Peek of the nearest row above r whose cell under caption
// is not empty, or "" when there is none in the data. It is how a cell
// merged down several rows is read from a row below its first: the
// workbook keeps the text on the first row alone.
func (r *Row) PeekAbove(caption string) string {
	for n := r.Number - 1; n >= r.sheet.first; n-- {
		above := Row{Number: n, sheet: r.sheet}
		if text := above.Peek(caption); text != "" {
			return text
		}
	}
	return ""
}

// Name returns the name of the sheet that was read.
func (s *Sheet) Name() string { return s.book.SheetName() }

// Rows returns the sheet's data rows, top to bottom. Every call returns
// the same rows, so what a dataset notes on one is there for the next
// caller to read.
func (s *Sheet) Rows() []*Row { return s.rows }

// FirstDataRow returns the number of the first row of data: FirstRow,
// or the row below the header when FirstRow is zero.
func (l UploadLayout) FirstDataRow() int {
	if l.FirstRow != 0 {
		return l.FirstRow
	}
	return l.HeaderRow + 1
}

// Check returns what makes the layout unusable, or nil. Each is a
// mistake in a dataset's own declaration rather than in a workbook, so a
// service checks every dataset's layout once, at startup, and refuses to
// start on one that fails.
func (l UploadLayout) Check() error {
	if l.HeaderRow < 1 || len(l.Header) == 0 {
		return errors.New("it has no header row")
	}
	if l.FirstRow != 0 && l.FirstRow <= l.HeaderRow {
		return errors.New("its data starts above its header row")
	}
	if l.GroupRow > 0 && l.GroupRow >= l.HeaderRow {
		return errors.New("its group captions are not above its " +
			"header row")
	}
	listed := make(map[string]bool, len(l.Header))
	for _, caption := range l.Header {
		if caption == "" || listed[caption] {
			return fmt.Errorf("its header lists %q twice, or is "+
				"missing a caption", caption)
		}
		listed[caption] = true
	}
	for _, caption := range l.Required {
		if !listed[caption] {
			return fmt.Errorf("it requires %q, which its header "+
				"does not list", caption)
		}
	}
	return nil
}
