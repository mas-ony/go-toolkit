// Package importer runs one spreadsheet import: open a workbook, check it
// is the shape that was expected, parse it, and hand each row to whatever
// writes it.
//
// It is the half that is the same for every import. What a row MEANS, and
// where it goes, is a Dataset — an interface a caller implements once per
// workbook shape. Nothing here knows about any particular sheet.
//
//	func main() {
//		ds, _ := importer.Lookup(*dataset)
//		run := &importer.Run{Log: log, Opt: opt, Count: &importer.Counters{}}
//		if err := ds.Connect(ctx, transport, run); err != nil {
//			...
//		}
//		outcomes, err := importer.Execute(ctx, ds, path, run)
//		...
//		importer.PrintSummary(os.Stdout, path, ds, run, outcomes)
//		_ = importer.WriteReport(*report, ds, outcomes)
//	}
//
// Four files. importer.go holds the run itself — what one row's outcome
// is, the counters a run tallies, the Dataset seam, and Execute, which
// drives the two against a workbook. table.go maps a sheet's columns onto
// a dataset's row type. report.go renders a finished run. upload.go is
// the same reading for a service that imports an upload rather than a
// file: UploadDataset, the export it reads, and OpenUpload, which hands
// back its rows to be read by the caption of their columns.
//
// # An upload is an export, read by caption
//
// A service holds an UploadDataset per kind of export it takes, opens an
// upload's bytes with OpenUpload against the dataset's UploadLayout, and
// hands the sheet's rows to Import, a record at a time:
//
//	sheet, err := importer.OpenUpload(data, ds.Layout())
//	if err != nil {
//		return err
//	}
//	for _, row := range sheet.Rows() {
//		rows := []*importer.Row{row}
//		err := ds.Import(ctx, rows)
//		if errors.Is(err, database.ErrRejected) {
//			row.Fail(err.Error())
//		} else if err != nil {
//			return err
//		}
//	}
//
// An export is a sheet a program wrote: its columns stand side by side
// from A in an order that does not vary, so an UploadLayout lists its
// captions and nothing else, and a column's letter is where its caption
// stands. The header check is made before OpenUpload returns a Sheet.
//
// A dataset reads a Row's cells by caption with Text, Int, Code, Decimal,
// DecimalDotGrouped and Date, which note on the row what will not read by
// the rule Table.Apply follows: a required cell fails the row, and any
// other is left empty with a warning. A required cell that is empty
// fails the row too, which Must does not ask of a Table: a column an
// export declares NOT NULL is empty only when the row cannot be stored.
// Peek, PeekAbove and Probe look at a row without noting anything on it,
// for a dataset that has to tell which rows belong together before it
// reads them.
//
// # A partly-failed import is the normal outcome
//
// Nothing here stops on a bad row. A cell that will not coerce leaves its
// field unset and the row goes on; a row that cannot be parsed at all is
// recorded and the run continues. A hand-maintained workbook of a few
// thousand rows reliably contains a handful of cells somebody typed a note
// into, and an importer that refused the file would never import anything.
//
// So the unit of failure is the ROW, not the run, and the output is a
// report rather than an error. Once Execute has it, only three things stop
// a run: the workbook will not open, the header check fails, or the
// dataset's own Prepare gives up. Connect failing stops it earlier, before
// Execute is called at all.
//
// A cancelled context ends a run early without failing it. Execute stops
// between rows, never inside one, and returns every outcome it already has,
// so the report still names each row that was written.
//
// # The header check is the one gate
//
// It runs before any parsing, because everything downstream of a moved
// column is confidently wrong rather than visibly broken — the new
// occupant is usually the same type and a plausible length, so it imports
// cleanly as a value that is simply wrong.
//
// -skip-header-check exists for a workbook that really has been
// restructured deliberately. It warns and continues. Reaching for it to
// make a run go through is how a silent corruption gets imported.
//
// # Every counter is the dataset's to move
//
// Execute sets Counters.Parsed and nothing else. Created, Updated,
// Skipped and Failed are moved by the dataset's own Import, including the
// fixed ones, because only it knows whether a row that landed but whose
// follow-up call did not is Created or Failed. A driver that guessed would
// be wrong on half the datasets.
//
// A dataset that wants its own tallies names them through Inc or Add.
// Declare pins their order and prints them even at zero, which is worth
// doing for a counter whose zero is itself information: "files uploaded 0"
// says the scans were not found, where a missing line says nothing at all.
//
// # The table is why a column moves in one edit
//
// A parser written by hand states every column three times: once as a
// letter constant, once in the caption contract, and once in the parse
// function. The three drift, and a column moved in the contract but not in
// the parser passes the header check and then reads the wrong cell — which
// is precisely the failure the header check exists to catch.
//
// Table puts the letter, the accepted captions and the coercion in one
// entry, so a column moves in a single edit or not at all. Columns renders
// the contract; Apply fills one row.
//
// Validate is worth calling once at start-up. Every problem it reports is
// a typo in a table nobody reads end to end, and each fails in a way that
// looks like a problem with the WORKBOOK rather than with the code.
//
// One caveat on its duplicate-value check. It reads Field.Key, and NO
// constructor in this package sets one — Text, Int, Decimal, Date, Raw,
// Link, Custom and Expect all leave it empty. A dataset that wants that
// check sets Key itself on the struct literal, or through its own
// constructors. Left unset, the check is inert rather than passing.
//
// # Registration is process-wide
//
// Register is meant to be called from a dataset's own init, so adding one
// is a single new file and no edit to main. The registry behind it is
// package state with no lock, which is safe for that use and is not safe
// for anything else: two importers in one process share it, and Only
// answers about whatever the whole binary registered.
//
// A duplicate name panics rather than replacing, because the alternative
// is one of the two datasets silently never being reachable.
//
// # What the tests hold in place
//
// The unit suite covers the parts that need no workbook: the counters and
// their ordering, the outcome helpers, the report sort, the registry
// including the panic and the global-state restore, every Table
// constructor and every arm of Validate and Apply, the epoch override, and
// the exact text of both report renderings.
//
// Apply is tested against a Source held in the test rather than a file,
// which is what that interface exists for — a table's coercion rules have
// nothing to do with where the grid came from.
//
// upload_test.go writes workbooks in memory and opens them with
// OpenUpload: the header check, a header of two rows, the footer and
// blank rows, the errors passed through and ErrMacros, every reader's
// empty, good and bad cell, required against optional columns, the
// panic, the readers that note nothing, and every arm of
// UploadLayout.Check. It holds the readers' notes to the same text Apply
// gives, since both follow one rule.
//
// importer_integration_test.go drives Execute end to end against a real
// workbook written to a temporary directory, with a dataset that records
// what it was asked to do. That is the only way to reach the ordering
// Execute promises between the header check, Prepare and Import, the
// limit, the cancellation check, and the merge of Prepare's parse failures
// into the imported rows.
//
//	go test -tags integration -run Integration ./importer
//
// It needs no server and no configuration.
package importer
