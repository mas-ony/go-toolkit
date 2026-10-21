package xlsx

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/xuri/excelize/v2"
)

// mergedRange is one merged block of the sheet, resolved to coordinates
// once at open so a caption lookup is a comparison rather than a string
// parse per call.
type mergedRange struct {
	minCol, minRow int
	maxCol, maxRow int
	value          string
}

// Layout describes where the data is in a particular workbook.
type Layout struct {
	// Sheet is the sheet to read. Empty means the first one in the
	// file, resolved at Open and readable through SheetName.
	Sheet string

	// HeaderRow holds the column captions. On a two-level header this
	// is the lower, more specific row.
	HeaderRow int

	// GroupRow holds captions merged across several columns. Zero means
	// the row directly above HeaderRow, which is where it sits on every
	// two-level sheet. A negative value means the sheet has no group
	// row, so Caption reports what is on HeaderRow and nothing else.
	GroupRow int

	// FirstRow is the first row of data, below any spacer.
	FirstRow int

	// LastRow is the final row to read. Zero means to the end of the
	// sheet.
	LastRow int
}

// LayoutError is the result of a failed header check: every column whose
// caption was not what the caller expected, in the order the caller
// listed them.
//
// It is a type rather than a formatted string so a caller can add the
// advice only it can give — where its column constants live, which flag
// skips the check — by wrapping it.
type LayoutError struct {
	// Sheet is the sheet that was checked.
	Sheet string

	// HeaderRow is the row the captions were read from.
	HeaderRow int

	// GroupRow is the row the group captions were read from, or 0 when
	// the sheet has none.
	GroupRow int

	// Problems holds one line per mismatched column, in the order the
	// caller listed the columns.
	Problems []string
}

// Column is one column of the expected layout: where it is, what it
// should be called, and what the caller reads it for.
//
// Captions lists the ACCEPTED spellings rather than one canonical string,
// because the variation in a hand-maintained header is real and harmless
// — a unit written "(m2)" in one revision and "(m²)" in the next, a word
// two departments spell two ways. Rejecting those makes the check
// something operators learn to bypass, which defeats it entirely.
//
// Spellings that differ only in spacing are not listed twice. Matches
// folds both sides, so "A/B" already covers "A/ B".
type Column struct {
	// Col is the column letter: "C", or "AB".
	Col string

	// Captions are the spellings this column accepts.
	Captions []string

	// Purpose is a short phrase naming what the column is read for. It
	// appears in a LayoutError and nowhere else.
	Purpose string
}

// File is an open spreadsheet and its resolved layout.
type File struct {
	// file is the open workbook, and nil once the File is closed.
	// Hyperlink and Link read through it, and everything else reads the
	// grid below.
	file *excelize.File

	// rows is the whole sheet, read once at Open: one slice per row, each
	// as long as its last populated cell, so a short row is the norm.
	rows [][]string

	// merges are the sheet's merged blocks, resolved once at Open for
	// Caption.
	merges []mergedRange

	// layout is the Layout Open was given, with Sheet filled in when it
	// was left to default.
	layout Layout

	// dir is the directory holding the workbook; a relative hyperlink
	// resolves against it, not against the working directory. It is
	// empty for a workbook opened from memory, which has none.
	dir string

	// date1904 is the workbook's own epoch setting.
	date1904 bool

	// macros reports that the workbook carries a VBA project.
	macros bool
}

// ErrClosed is what Link answers for a File that has been closed. A
// hyperlink is read through the workbook, which Close lets go of, and
// without this the answer would be ErrNoLink: that the cell carries no
// link, of a cell nobody looked at.
var ErrClosed = errors.New("workbook is closed")

// contains reports whether a 1-based row and column index fall inside the
// range.
func (m mergedRange) contains(row, col int) bool {
	return row >= m.minRow && row <= m.maxRow &&
		col >= m.minCol && col <= m.maxCol
}

// uses1904 reports whether the workbook counts dates from 1904 rather
// than 1900.
//
// Asking the file is better than asking the operator. A workbook authored
// in classic Mac Excel shifts every date by four years and a day when
// read with the wrong epoch, and the resulting dates are all plausible —
// nothing about "1996-02-16" says it should have been 2000-02-17. A
// caller that needs to override this, for a file whose property was lost
// in a round-trip through another tool, passes its own value to whatever
// consumes Date1904.
func uses1904(f *excelize.File) bool {
	props, err := f.GetWorkbookProps()
	if err != nil || props.Date1904 == nil {
		return false
	}
	return *props.Date1904
}

// mergedValue returns the value of the merged block covering a cell, or
// "" when the cell is not part of one.
func (w *File) mergedValue(row int, col string) string {
	idx, ok := ColumnIndex(col)
	if !ok {
		return ""
	}
	for _, m := range w.merges {
		if m.contains(row, idx) {
			return m.value
		}
	}
	return ""
}

// captionAt reads one header cell, falling back to the merged block it
// belongs to.
func (w *File) captionAt(row int, col string) string {
	if row < 1 {
		return ""
	}
	if v := Trim(w.Cell(row, col)); v != "" {
		return v
	}
	return Trim(w.mergedValue(row, col))
}

// normaliseCaption lowercases a caption and collapses runs of whitespace
// so comparison ignores the trailing spaces and stray non-breaking
// characters that accumulate in hand-maintained headers.
func normaliseCaption(s string) string {
	return strings.ToLower(
		strings.Join(strings.Fields(Trim(s)), " "))
}

// foldCaption is normaliseCaption with the remaining spaces removed, for
// comparing captions rather than displaying them.
//
// Interior spacing around punctuation is not a difference anybody means:
// one sheet writes "A/B" in one column and "C/ D" in another, and pads a
// third with trailing spaces. Folding lets one accepted spelling cover
// the lot instead of listing every place a maintainer might have pressed
// the space bar.
func foldCaption(s string) string {
	return strings.ReplaceAll(normaliseCaption(s), " ", "")
}

// quoteAny renders the accepted captions for an error message.
func quoteAny(captions []string) string {
	quoted := make([]string, 0, len(captions))
	for _, c := range captions {
		quoted = append(quoted, fmt.Sprintf("%q", c))
	}
	return strings.Join(quoted, " or ")
}

// hyperlinkFormulaTarget extracts the link target from a HYPERLINK()
// formula, returning "" for anything else.
//
// Only a literal first argument is understood —
// HYPERLINK("documents/1.pdf","1"). A computed one,
// HYPERLINK(CONCATENATE(...),...), would need a formula engine to
// evaluate and is left to the caller rather than half-interpreted.
func hyperlinkFormulaTarget(formula string) string {
	f := strings.TrimSpace(formula)
	if f == "" {
		return ""
	}
	f = strings.TrimPrefix(f, "=")
	if !strings.HasPrefix(strings.ToUpper(f), "HYPERLINK(") {
		return ""
	}
	open := strings.Index(f, "(")
	rest := strings.TrimSpace(f[open+1:])
	if !strings.HasPrefix(rest, `"`) {
		return "" // a computed argument — not a literal path
	}
	rest = rest[1:]
	end := strings.Index(rest, `"`)
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:end])
}

// cellAt returns the value at a 1-based row and column of the grid, or
// "" when the row is short or absent.
func cellAt(rows [][]string, row, col int) string {
	if row < 1 || row > len(rows) {
		return ""
	}
	cells := rows[row-1]
	if col < 1 || col > len(cells) {
		return ""
	}
	return cells[col-1]
}

// mergedRanges resolves the references of a sheet's merged blocks, each
// written as the sheet writes it ("C5:E5"), to coordinates. The value of
// a block is the value of its top-left cell, the one cell of a merged
// block a sheet stores anything in.
//
// A reference that cannot be read is skipped rather than failed. The
// list is only used to resolve header captions, and a sheet whose merges
// cannot be read still imports correctly — the header check simply
// reports the columns whose caption lives in a neighbouring cell, which
// is a far better outcome than refusing to open the file.
func mergedRanges(refs []string, rows [][]string) []mergedRange {
	out := make([]mergedRange, 0, len(refs))
	for _, ref := range refs {
		from, to, ok := strings.Cut(ref, ":")
		if !ok {
			continue
		}
		minCol, minRow, err := excelize.CellNameToCoordinates(from)
		if err != nil {
			continue
		}
		maxCol, maxRow, err := excelize.CellNameToCoordinates(to)
		if err != nil {
			continue
		}
		// A range written from its bottom-right corner is the same
		// block.
		minCol, maxCol = min(minCol, maxCol), max(minCol, maxCol)
		minRow, maxRow = min(minRow, maxRow), max(minRow, maxRow)
		out = append(out, mergedRange{
			minCol: minCol, minRow: minRow,
			maxCol: maxCol, maxRow: maxRow,
			value: cellAt(rows, minRow, minCol),
		})
	}
	return out
}

// closeFailed closes a workbook that is not going to be handed out: what
// excelize returned beside an error, which can be a workbook with
// temporary files of its own, or one load read and then refused.
func closeFailed(f *excelize.File) {
	if f != nil {
		_ = f.Close()
	}
}

// readRows reads every row of the sheet as text, and returns the grid
// with the number of the last row the reading reached.
//
// It is excelize's GetRows with two differences. An error part-way down
// the sheet is returned, where GetRows stops there and returns the rows
// above it as the whole sheet. And a cell holding what the cell above it
// holds shares that cell's text, which is what keeps a large report in
// memory: such a sheet repeats most of a row from the row above, and
// excelize hands over every cell as a string of its own.
//
// RawCellValue is the load-bearing option: without it excelize applies
// the cell's number format, so a date comes back as whatever string the
// sheet happens to display it as rather than as the serial underneath.
func readRows(f *excelize.File, sheet string) ([][]string, int, error) {
	it, err := f.Rows(sheet)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = it.Close() }()

	raw := excelize.Options{RawCellValue: true}
	var (
		rows  [][]string
		above []string
	)
	// The iterator yields every row number up to the last, a row the
	// sheet leaves out as an empty one, so a row's place in the grid is
	// its number.
	for it.Next() {
		cells, err := it.Columns(raw)
		if err != nil {
			return nil, 0, err
		}
		for i := range min(len(cells), len(above)) {
			if cells[i] == above[i] {
				cells[i] = above[i]
			}
		}
		if len(cells) > 0 {
			above = cells
		}
		rows = append(rows, cells)
	}
	if err := it.Error(); err != nil {
		return nil, 0, err
	}

	// The grid ends with the last row that holds anything, as GetRows
	// ends it.
	reached, last := len(rows), len(rows)
	for last > 0 && len(rows[last-1]) == 0 {
		last--
	}
	return rows[:last], reached, nil
}

// openPath is Open without the path its errors lead with, which Open puts
// in front of every one of them, so no return here can leave it out.
func openPath(path string, layout Layout) (*File, error) {
	src, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = src.Close() }()

	info, err := src.Stat()
	if err != nil {
		return nil, err
	}
	arc, err := inspect(src, info.Size())
	if err != nil {
		return nil, err
	}
	f, err := excelize.OpenFile(path)
	if err != nil {
		closeFailed(f)
		return nil, err
	}

	dir, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		dir = filepath.Dir(path)
	}
	return load(f, arc, layout, dir)
}

// load reads the sheet the layout names out of an opened workbook, and
// holds the reading against what the archive says of that sheet. It
// closes f when it fails.
func load(
	f *excelize.File, arc *archive, layout Layout, dir string,
) (*File, error) {
	if layout.Sheet == "" {
		names := f.GetSheetList()
		if len(names) == 0 {
			closeFailed(f)
			return nil, errNoSheet
		}
		layout.Sheet = names[0]
	}

	rows, reached, err := readRows(f, layout.Sheet)
	if err != nil {
		closeFailed(f)
		return nil, fmt.Errorf("reading sheet %q: %w", layout.Sheet, err)
	}
	last, merges, err := arc.scan(layout.Sheet)
	if err != nil {
		closeFailed(f)
		return nil, fmt.Errorf("reading sheet %q: %w", layout.Sheet, err)
	}
	if last > reached {
		closeFailed(f)
		return nil, &TruncatedError{
			Sheet: layout.Sheet, Read: reached, Last: last}
	}

	return &File{
		file:     f,
		rows:     rows,
		merges:   mergedRanges(merges, rows),
		layout:   layout,
		dir:      dir,
		date1904: uses1904(f),
		macros:   arc.macros,
	}, nil
}

// Dir returns the absolute directory holding the workbook. A relative
// hyperlink target is stored relative to the workbook file, so resolving
// one requires knowing where that file is — not where the program was run
// from.
//
// It is empty for a workbook opened with OpenBytes, which is in no
// directory.
func (w *File) Dir() string { return w.dir }

// HasMacros reports whether the workbook carries a VBA project, which
// is what an .xlsm holds and an .xlsx does not, whatever the file is
// named. It is told by the entry a project is kept in, xl/vbaProject.bin,
// and by a relationship of the workbook naming one under any other name.
//
// Nothing in this package runs a macro, or reads one. It is here for a
// caller that takes workbooks from strangers and has a policy on which
// kind it accepts.
func (w *File) HasMacros() bool { return w.macros }

// Date1904 reports the workbook's own epoch setting.
func (w *File) Date1904() bool { return w.date1904 }

// Matches reports whether a caption read from the sheet is one this
// column accepts.
func (c Column) Matches(caption string) bool {
	got := foldCaption(caption)
	for _, want := range c.Captions {
		if got == foldCaption(want) {
			return true
		}
	}
	return false
}

// SheetName returns the sheet actually being read, which matters when the
// caller let it default to the first one.
func (w *File) SheetName() string { return w.layout.Sheet }

// Cell returns the value at a 1-based row and a column letter, or "" when
// the row is short or absent.
//
// Short rows are the normal case, not an edge one: excelize trims
// trailing empty cells, so a row whose last populated column is M
// genuinely has thirteen entries and asking for Y must yield "" rather
// than panic.
func (w *File) Cell(row int, col string) string {
	idx, ok := ColumnIndex(col)
	if !ok {
		return ""
	}
	return cellAt(w.rows, row, idx)
}

// ColumnIndex returns the 1-based number of a column letter, "A" being 1,
// in either letter case, and false for text that names no column a sheet
// can have: anything but letters, or a column past XFD.
//
// It is the one reading of a letter in this package. Cell and Caption
// look a column up through it, so a letter it refuses reads as an empty
// cell on every row, and importer's Table.Validate holds a column table
// to it for exactly that reason.
//
// The index is checked as well as the error. excelize's parser does not
// bound the length of the letters it sums, so a long enough run overflows
// int and can come back negative with no error, which names no column.
func ColumnIndex(col string) (int, bool) {
	idx, err := excelize.ColumnNameToNumber(col)
	return idx, err == nil && idx >= 1
}

// Error renders every mismatch, one per line.
func (e *LayoutError) Error() string {
	where := fmt.Sprintf("captions read from row %d", e.HeaderRow)
	if e.GroupRow > 0 {
		where = fmt.Sprintf("sub-captions read from row %d, group"+
			" captions from row %d", e.HeaderRow, e.GroupRow)
	}
	return fmt.Sprintf("sheet %q does not match the expected layout"+
		" (%s):\n  %s",
		e.Sheet, where, strings.Join(e.Problems, "\n  "))
}

// Hyperlink returns the hyperlink target attached to a cell, or "" when
// there is none.
//
// Two storage forms are checked, because a workbook can use either and
// they are invisibly different to whoever built it:
//
//  1. A real hyperlink, written into the sheet's relationship file.
//     Excelize follows the relationship and hands back the external
//     target, which on a Windows-authored file is typically a relative
//     path with backslash separators and spaces in it.
//  2. A HYPERLINK() formula, whose first argument is the target. This
//     form appears when the link is generated from other columns rather
//     than created by browsing to a file, which is the style a
//     spreadsheet built out of VLOOKUPs and =IF() comparisons tends to
//     be written in throughout.
//
// Errors are swallowed: a cell with no hyperlink is the ordinary case,
// not a failure, and every caller treats "" and "lookup failed"
// identically. A closed File is one of them, and answers "" for every
// cell; Link is the one that says so.
func (w *File) Hyperlink(row int, col string) string {
	if w.file == nil {
		return ""
	}
	cell := fmt.Sprintf("%s%d", col, row)

	ok, target, err := w.file.GetCellHyperLink(w.layout.Sheet, cell)
	if err == nil && ok {
		if t := strings.TrimSpace(target); t != "" {
			return t
		}
	}

	formula, err := w.file.GetCellFormula(w.layout.Sheet, cell)
	if err == nil {
		if t := hyperlinkFormulaTarget(formula); t != "" {
			return t
		}
	}
	return ""
}

// Link is Hyperlink resolved against the workbook's own directory: the
// target attached to a cell, turned into a local path to try.
//
// It exists so a caller never has to know which directory a relative
// target resolves against. It is the workbook's, not the process working
// directory, and getting it wrong is what makes a link look broken on a
// machine where the file is sitting right there.
//
// The errors are ResolveLink's: ErrNoLink when the cell carries no
// hyperlink, ErrNotAFile when the target is a web page, a mailbox, or
// another cell in the workbook. A File that has been closed answers
// ErrClosed for every cell.
//
// A workbook opened with OpenBytes has no directory, so a relative
// target stays relative: Path is the target as the workbook stores it,
// for the caller to join onto the directory it knows the documents to
// be under.
func (w *File) Link(row int, col string) (LinkTarget, error) {
	if w.file == nil {
		return LinkTarget{}, ErrClosed
	}
	return ResolveLink(w.Hyperlink(row, col), w.dir)
}

// RowIsBlank reports whether every one of cols is empty in the row.
func (w *File) RowIsBlank(row int, cols []Column) bool {
	for _, spec := range cols {
		if Trim(w.Cell(row, spec.Col)) != "" {
			return false
		}
	}
	return true
}

// RowCount returns the number of rows present in the sheet.
func (w *File) RowCount() int { return len(w.rows) }

// HeaderRow returns the row the captions are read from.
func (w *File) HeaderRow() int { return w.layout.HeaderRow }

// GroupRow returns the row the merged group captions are read from, or 0
// when the sheet has none.
func (w *File) GroupRow() int {
	if w.layout.GroupRow < 0 {
		return 0
	}
	if w.layout.GroupRow > 0 {
		return w.layout.GroupRow
	}
	if w.layout.HeaderRow > 1 {
		return w.layout.HeaderRow - 1
	}
	return 0
}

// LastDataRow resolves the final row to read, honouring an explicit
// Layout.LastRow and otherwise running to the end of the sheet.
func (w *File) LastDataRow() int {
	last := len(w.rows)
	if w.layout.LastRow > 0 && w.layout.LastRow < last {
		return w.layout.LastRow
	}
	return last
}

// DataRows returns the numbers of the rows worth reading: every row in
// the data range carrying something in at least one of cols.
//
// A row where all of them are empty is passed over silently. Blank rows
// inside a range are ordinary in a hand-maintained sheet — spacers, or
// the remains of a deleted entry — and reporting each one would bury the
// real problems. Testing only the mapped columns is what makes the skip
// safe: a row holding nothing but a column the caller does not read
// carries no data by the caller's own definition of the word.
func (w *File) DataRows(cols []Column) []int {
	first := w.layout.FirstRow
	if first < 1 {
		first = 1
	}

	var out []int
	for row := first; row <= w.LastDataRow(); row++ {
		if w.RowIsBlank(row, cols) {
			continue
		}
		out = append(out, row)
	}
	return out
}

// Caption returns a column's effective label: the sub-caption when it has
// one, and the group caption otherwise.
//
// Both are resolved through the merge list, so a column whose caption is
// stored in the top-left of a merged block reports that caption rather
// than an empty string.
func (w *File) Caption(col string) string {
	if v := w.captionAt(w.layout.HeaderRow, col); v != "" {
		return v
	}
	return w.captionAt(w.GroupRow(), col)
}

// ColumnsFromA lays a contract out from captions alone: the first is
// column A's, the second column B's, and so on, each the one spelling
// its column accepts.
//
// It is for a sheet a program writes, whose columns stand side by side
// from A in an order that does not vary and whose captions nobody
// respells. A sheet kept by hand wants its Columns written out, with
// every spelling seen in the wild.
//
// Purpose is left empty, the caption being the only name such a column
// has, and a LayoutError then names the column by its letter alone.
// Captions past XFD, the last column a sheet has, are left out.
func ColumnsFromA(captions ...string) []Column {
	out := make([]Column, 0, len(captions))
	for i, caption := range captions {
		col, err := excelize.ColumnNumberToName(i + 1)
		if err != nil {
			break
		}
		out = append(out, Column{Col: col, Captions: []string{caption}})
	}
	return out
}

// Open opens the file, resolves the sheet, and reads every row.
//
// The whole sheet is read into memory at once rather than handed over a
// row at a time. What is kept of it is the text of its cells and
// nothing else, so the cost is what the sheet holds, and it buys a much
// simpler shape: a random-access grid the header check and the row loop
// can both index into, with one place where a read can fail.
//
// The reading is held against the sheet's own XML before it is
// returned. A sheet that could not be read to its end fails with a
// *TruncatedError rather than coming back as its first rows; see
// sheetScan for how that can happen and how it is told.
//
// A formula cell yields its CACHED result rather than the formula text,
// which matters for every generated column. A workbook saved by a script
// rather than by Excel carries no cached values at all, and those cells
// read as empty — worth knowing before treating a blank as a missing
// value in the source data.
//
// It fails with ErrNotWorkbook for a file that is no workbook, and with
// ErrTooLarge for one that would expand past MaxUnzippedBytes. Both are
// decided before excelize is handed the file: the cap on the ZIP
// directory alone, and the sheets on the three small parts that say
// which there are and where.
//
// Every error leads with path, those two included. What is safe to show
// whoever sent the workbook is the error errors.Is or errors.As finds
// inside, and not the message around it.
func Open(path string, layout Layout) (*File, error) {
	w, err := openPath(path, layout)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	return w, nil
}

// OpenBytes is Open for a workbook held in memory, which is where an
// upload is: nothing has to be written to disk for it to be read.
//
// data is not kept, and is not changed. Excelize makes a copy of its
// own to read from, and unpacks a sheet of more than 16 MiB of XML into
// a file in the system's temporary directory, which Close removes.
//
// The File has no directory. Dir is empty, and Link leaves a relative
// target relative.
//
// No error names a path on the host unless excelize's own does, which
// only its failure to write that temporary file would. The three this
// package raises here are safe to show whoever sent the workbook:
// ErrNotWorkbook, ErrTooLarge and *TruncatedError.
func OpenBytes(data []byte, layout Layout) (*File, error) {
	arc, err := inspect(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	f, err := excelize.OpenReader(bytes.NewReader(data))
	if err != nil {
		closeFailed(f)
		return nil, fmt.Errorf("opening the workbook: %w", err)
	}
	return load(f, arc, layout, "")
}

// VerifyHeaders checks that every column carries the caption it is
// expected to, and reports all mismatches at once.
//
// This is the only defence against the failure mode that matters when
// columns are addressed by letter. A column that has MOVED still parses:
// its new occupant is usually of the same type and a plausible length,
// and it imports cleanly as a value that is simply wrong. Nothing
// downstream can notice — not the destination, not a schema, not a spot
// check of the row count. Comparing the captions is what turns a silent
// corruption into a message naming the column.
//
// Every mismatch is listed rather than the first, because a workbook that
// has been restructured usually moved several columns, and fixing them
// one run at a time is the difference between one conversation with the
// maintainers and six.
func (w *File) VerifyHeaders(cols []Column) error {
	var problems []string
	for _, spec := range cols {
		caption := w.Caption(spec.Col)
		if spec.Matches(caption) {
			continue
		}
		found := fmt.Sprintf("%q", caption)
		if caption == "" {
			found = "an empty cell"
		}
		// A column with no stated purpose is named by its letter alone,
		// its expected caption being all there is to say of it.
		column := fmt.Sprintf("column %-2s (%s)", spec.Col, spec.Purpose)
		if spec.Purpose == "" {
			column = "column " + spec.Col
		}
		problems = append(problems, fmt.Sprintf(
			"%s: expected %s, found %s",
			column, quoteAny(spec.Captions), found))
	}
	if len(problems) == 0 {
		return nil
	}

	return &LayoutError{
		Sheet:     w.layout.Sheet,
		HeaderRow: w.layout.HeaderRow,
		GroupRow:  w.GroupRow(),
		Problems:  problems,
	}
}

// Close releases what excelize holds of the workbook: the temporary file
// a large sheet was unpacked into, which it removes, and the parts it
// unpacked into memory, which go with the workbook itself when the File
// lets go of it. Excelize's own Close frees only the first, so a File
// that kept the workbook would keep those parts for as long as it lived,
// closed or not.
//
// The grid is the File's own, so everything that only reads it goes on
// answering after Close: Cell, Caption, VerifyHeaders, DataRows and the
// rest. A caller that reads nothing else can therefore close as soon as
// Open returns, and hold only the text of the cells for as long as its
// work takes. Hyperlink and Link read through the workbook, and are not
// for a closed File: Hyperlink answers "" and Link ErrClosed.
//
// Closing a File a second time does nothing, and returns nil.
func (w *File) Close() error {
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}
