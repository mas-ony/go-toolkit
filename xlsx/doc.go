// Package xlsx reads a spreadsheet: where the data is, what the cells
// mean, and where the hyperlinks point. The sheet may be one kept by hand
// or one a program exported, and the workbook may be a file on disk or
// an upload held in memory.
//
// It is a reader. Nothing here writes a workbook, and nothing decides what
// to do with a row — that belongs to whatever is importing it.
//
//	w, err := xlsx.Open(path, xlsx.Layout{HeaderRow: 4, FirstRow: 6})
//	if err != nil {
//		return err
//	}
//	defer w.Close()
//
//	if err := w.VerifyHeaders(cols); err != nil {
//		return err
//	}
//	for _, row := range w.DataRows(cols) {
//		name, err := xlsx.StrMax(w.Cell(row, "C"), 100, "name")
//		...
//	}
//
// Four files, four jobs. workbook.go resolves the layout and hands back
// a grid. archive.go reads what excelize does not say about the ZIP
// package underneath: what it would expand to, whether it carries
// macros, where a sheet's XML really ends, and which cells it merges.
// value.go turns a cell's text into a typed Go value. link.go turns a
// hyperlink into a local path to try.
//
// # Columns are addressed by letter, and that is the whole risk
//
// A caller names "C" and gets what is in C. That is stable across every
// cosmetic change an operator makes — renaming a sheet, reordering rows,
// restyling a header — and it is fast, because the sheet is already a
// grid.
//
// What it does not survive is a column MOVING. The new occupant is usually
// the same type and a plausible length, so it parses, imports cleanly, and
// is simply wrong. Nothing downstream can notice: not the destination, not
// a schema, not a row count.
//
// VerifyHeaders is the only defence, and it is not optional in the way an
// extra check usually is. Call it before reading a single row. It compares
// each column's caption against the spellings the caller accepts and
// reports EVERY mismatch at once, because a restructured workbook usually
// moved several columns and fixing them one run at a time is the
// difference between one conversation with the maintainers and six.
//
// Captions are matched loosely on purpose — case, whitespace and interior
// spacing are folded away, and a Column lists several accepted spellings
// rather than one canonical string. The variation in a hand-maintained
// header is real and harmless: a unit written "(m2)" in one revision and
// "(m²)" in the next, a word two departments spell two ways. Rejecting
// those makes the check something operators learn to bypass, which defeats
// it entirely.
//
// A sheet a program exports has none of that variation, and its columns
// stand side by side from A. ColumnsFromA writes the contract of such a
// sheet from its captions alone, in order.
//
// # A workbook is checked before it is read
//
// Open and OpenBytes read the ZIP directory first, and refuse two kinds
// of content before excelize is handed any of it. Content that is no
// ZIP archive, or whose workbook lists no sheet, is ErrNotWorkbook: a
// legacy .xls, an encrypted workbook, an HTML or CSV export renamed, a
// .docx. A workbook that would expand past MaxUnzippedBytes is
// ErrTooLarge, which is what keeps a few megabytes of DEFLATE from
// asking excelize for gigabytes.
//
// After the rows are read, the reading is held against the sheet's own
// XML. Excelize takes the first fault in a sheet's XML for its end, so a
// sheet with one character XML does not allow would otherwise come back
// as its first rows, with no error. Such a sheet is refused with a
// *TruncatedError naming how far it reads. A fault inside the last row
// cannot be told from the end of the sheet this way, and passes.
//
// HasMacros says whether the workbook carries a VBA project. Nothing here
// runs one; the answer is for a caller with a policy on what it accepts.
//
// ErrNotWorkbook, ErrTooLarge and a *TruncatedError say nothing but what
// the workbook itself holds, and are safe to show whoever sent it. From
// OpenBytes they arrive as they are. Open puts the path it was given in
// front of every message, so what is safe to show there is the error
// errors.Is or errors.As finds inside, and not the text around it. Any
// other error wraps excelize's own, which can name a temporary file on
// the host.
//
// # Cells are read as stored, and formulas as their last result
//
// The sheet is read with RawCellValue, so a cell yields what is stored
// rather than what the sheet displays. A date comes back as its serial
// number instead of whatever the cell's number format renders it as, which
// is what makes Date able to resolve it at all.
//
// The second fact holds whatever the options, and it is the easier one to
// be caught by. A formula cell yields its CACHED result, never the formula
// text, and a workbook saved by a script rather than by Excel carries no
// cached values, so every generated column in it reads as empty. That is
// not a missing value in the source data, and a caller that treats blank as
// missing will quietly under-count a file nobody can see anything wrong
// with. If a whole column comes back empty, this is the first thing to
// check.
//
// A character the XML cannot carry as itself, a carriage return among
// them, is stored as an escape, _x000D_. Excelize decodes it in some
// cells and not in others, and the cell does not say which it was, so
// Unescape is the caller's to apply to the text columns of a sheet that
// shows them.
//
// # Ambiguity is resolved by rule, and each rule is a guess
//
// Three places accept input that is genuinely ambiguous, and in each the
// wrong answer is silent rather than an error:
//
//   - DATE ORDER. Text dates are read day-first, and no month-first
//     layout is tried at all, so 03/04/2005 is 3 April. A workbook
//     authored in a month-first locale is read wrong with no complaint
//     wherever the day is 12 or less, and refused only on a later day:
//     12/25/2005 has no month 25 to be read as.
//   - THE EPOCH. A workbook authored in classic Mac Excel counts from
//     1904, shifting every date by four years and a day. Open asks the
//     file rather than the operator, because the resulting dates are all
//     plausible — nothing about "1996-02-16" says it should have been
//     2000-02-17.
//   - THOUSANDS SEPARATORS. When a value carries both "." and ",", the
//     later of the two is the decimal point. When it carries only one of
//     them, that one is read as grouping only when it appears more than
//     once with three digits after each, and a single occurrence is always
//     a decimal point, so "1,234" is read as 1.234 and never as one
//     thousand two hundred and thirty-four. See Decimal, which also
//     says how a separator at either end of the number is read. A column
//     the caller knows to be written as 1.234.567,89 is read without the
//     guess by DecimalDotGrouped, which refuses any other form.
//
// The spelled-out month layouts read English and Indonesian month names,
// and the two share one table because no spelling in it names different
// months in the two languages. Any other language's month name does not
// parse however close its spelling, and fails loudly naming the cell,
// which is why that is documented rather than patched: adding a language
// means its names in the table and the same check against the two
// already there.
//
// # Hyperlinks resolve against the workbook, not the process
//
// A relative target is stored relative to the workbook FILE. Link resolves
// it against Dir, which is the one thing a caller would otherwise have to
// know and the thing that makes a link look broken on a machine where the
// file is sitting right there. A workbook opened with OpenBytes is in no
// directory, and Link leaves its relative targets relative.
//
// Two storage forms are read, because a workbook can use either and they
// are invisibly different to whoever built it: a real hyperlink in the
// sheet's relationship file, and a HYPERLINK() formula whose first
// argument is a literal path. A computed argument is not evaluated.
//
// LinkTarget.Foreign says the target was ABSOLUTE — a drive letter, a UNC
// share, or a POSIX root. It is a statement about the SHAPE of the target
// and not a prediction that it will fail, and it is NOT a containment
// check: a relative target may still climb out of the workbook directory
// with Foreign false. What stops a link naming anything it likes is the
// extension gate applied when the file is read, not this field.
//
// # Memory and concurrency
//
// Open reads the whole sheet into memory at once rather than handing it
// over a row at a time, which buys a random-access grid that the header
// check and the row loop can both index into. What is kept is the text of
// the cells, with a cell that repeats the cell above it sharing that
// cell's text, so a sheet costs about what it holds. A few thousand rows
// kept by hand cost nothing; an export of a hundred thousand rows by
// forty-five columns, 20 MB as uploaded, is held in about 130 MB.
//
// Excelize's own model of a sheet, a struct for every cell, is many times
// larger — about three gigabytes for that same export — and Open never
// asks for it. Hyperlink and Link do, on first use, since a hyperlink
// lives in that model and nowhere in the cells' text.
//
// Because the grid is never written after Open returns, the methods that
// only read it — Cell, Caption, RowCount, DataRows, RowIsBlank,
// VerifyHeaders and the layout accessors — are safe to call concurrently,
// and go on answering after Close, which releases only what excelize
// holds: its temporary file, and the parts of the workbook it unpacked
// into memory, which until then are held beside the grid. Hyperlink and
// Link are different: they call back into the underlying excelize file,
// so their safety is that library's to guarantee rather than this one's,
// and they are not for a closed File, where Hyperlink answers "" and Link
// ErrClosed. Close must not run concurrently with anything.
//
// # What the tests hold in place
//
// The unit suite writes real workbooks with excelize — merges, both
// hyperlink forms, formulas — and reads them back, so it already covers
// the layout resolution, caption folding, the header check and row
// selection against a real file, and every conversion in value.go and
// link.go on the text a cell holds. It also takes those workbooks apart:
// an archive declaring sizes it does not hold for the cap, a sheet given
// a character XML does not allow for the truncation, and the scan fed the
// same XML whole, in pieces and a byte at a time, since a tag falling
// across two writes is the ordinary case.
//
// workbook_integration_test.go covers what that suite does not reach. Each
// case produces a WRONG ANSWER rather than an error, which is why each is
// worth a real file:
//
//   - The 1904 epoch, end to end, against a workbook carrying the
//     property. A wrong epoch produces dates that are all plausible.
//
//   - Raw cell values: a cell carrying a date number format must still read
//     as its serial. With the format applied, Date cannot resolve it.
//
//   - The missing cached values above, reproduced by writing a formula and
//     not calculating it, so the documented behaviour is pinned rather
//     than described — and beside it, a formula whose result IS cached
//     reading as that result, without which the first case would pass
//     against a reader that returned nothing at all.
//
//   - Hyperlinks that resolve against the workbook's directory even after
//     the process has moved to another one, and a relative link that
//     climbs out of that directory, which is the non-boundary described
//     above.
//
//   - Concurrent readers under the race detector, against the claim in the
//     section above.
//
// It writes workbooks to a temporary directory and needs nothing else:
//
//	go test -tags integration -run Integration ./xlsx
package xlsx
