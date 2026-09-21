//go:build integration

package importer

// Integration tests for Execute.
//
// Everything else in this package is testable from a struct or a map, and
// is. Execute is not: it is the one thing here that opens a file, and what
// it actually does is ORDER the other parts — header check, then Prepare,
// then Import per row, with the limit and the cancellation check in
// between, and Prepare's parse failures merged into the imported rows at
// the end.
//
// None of that ordering is observable without a workbook, and every one of
// the orderings matters. The header check running after Prepare would mean
// parsing a sheet that was about to be rejected; running it after Import
// would mean writing rows from a sheet whose columns had moved.
//
// So the dataset below is a recorder: it satisfies the interface and
// writes down what it was asked to do and when. The assertions are about
// that transcript rather than about any data.
//
//	go test -tags integration -run Integration ./importer
//
// Workbooks are written to a temporary directory; nothing else is needed.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/xuri/excelize/v2"

	"github.com/mas-ony/go-toolkit/xlsx"
)

const itSheet = "Sheet1"

// recorder is a Dataset that records the calls Execute makes.
type recorder struct {
	// what Execute is told
	columns []xlsx.Column
	layout  xlsx.Layout

	// what Prepare hands back
	prepareOutcomes []Outcome
	prepareErr      error
	rows            int

	// what happened
	calls        []string
	importedRows []int
	headerAdvice error

	// let Import see the run
	onImport func(i int, run *Run) Outcome
}

// writeSheet builds a workbook whose header row carries captions and whose
// data rows carry values, and returns its path.
func writeSheet(t *testing.T, captions []string, rows int) string {
	t.Helper()

	f := excelize.NewFile()
	t.Cleanup(func() { _ = f.Close() })

	for i, caption := range captions {
		cell := fmt.Sprintf("%c1", 'A'+i)
		if err := f.SetCellValue(itSheet, cell, caption); err != nil {
			t.Fatalf("SetCellValue %s: %v", cell, err)
		}
	}
	for row := 2; row <= rows+1; row++ {
		for i := range captions {
			cell := fmt.Sprintf("%c%d", 'A'+i, row)
			if err := f.SetCellValue(itSheet, cell,
				fmt.Sprintf("v%d-%d", row, i)); err != nil {
				t.Fatalf("SetCellValue %s: %v", cell, err)
			}
		}
	}

	path := filepath.Join(t.TempDir(), "book.xlsx")
	if err := f.SaveAs(path); err != nil {
		t.Fatalf("SaveAs: %v", err)
	}
	return path
}

// newRun builds the Run Execute is given.
func newRun(opt Options) *Run {
	if opt.Layout.HeaderRow == 0 {
		opt.Layout = xlsx.Layout{HeaderRow: 1, FirstRow: 2}
	}
	return &Run{Log: zerolog.Nop(), Opt: opt, Count: &Counters{}}
}

// matchingColumns is a contract the fixture satisfies.
func matchingColumns() []xlsx.Column {
	return []xlsx.Column{
		{Col: "A", Captions: []string{"Code"}, Purpose: "code"},
		{Col: "B", Captions: []string{"Name"}, Purpose: "name"},
	}
}

func (r *recorder) Info() Info {
	return Info{Name: "recorder", Describe: "a test dataset"}
}

func (r *recorder) Flags(*flag.FlagSet) {}
func (r *recorder) Validate() error     { return nil }
func (r *recorder) Layout() xlsx.Layout { return r.layout }
func (r *recorder) Columns() []xlsx.Column {
	r.calls = append(r.calls, "Columns")
	return r.columns
}

func (r *recorder) HeaderAdvice(err error) error {
	r.calls = append(r.calls, "HeaderAdvice")
	if r.headerAdvice != nil {
		return r.headerAdvice
	}
	return fmt.Errorf("dataset advice: %w", err)
}

func (r *recorder) Connect(context.Context, Transport, *Run) error {
	r.calls = append(r.calls, "Connect")
	return nil
}

func (r *recorder) Prepare(
	_ context.Context, wb *xlsx.File, run *Run,
) ([]Outcome, error) {
	r.calls = append(r.calls, "Prepare")
	// Execute resolves the sheet name before Prepare, which is what lets
	// a dataset report it.
	if run.Opt.Layout.Sheet == "" {
		panic("Prepare ran before the sheet was resolved")
	}
	return r.prepareOutcomes, r.prepareErr
}

func (r *recorder) Len() int { return r.rows }

func (r *recorder) Import(_ context.Context, i int, run *Run) Outcome {
	r.calls = append(r.calls, "Import")
	r.importedRows = append(r.importedRows, i)
	if r.onImport != nil {
		return r.onImport(i, run)
	}
	run.Count.Created++
	return Outcome{ExcelRow: 10 + i, Status: StatusCreated}
}

func (r *recorder) Report() ReportShape {
	return ReportShape{Label: "label", ID: "id"}
}

// The happy path, and the ordering that makes it correct.
func TestIntegrationExecuteRunsInOrder(t *testing.T) {
	path := writeSheet(t, []string{"Code", "Name"}, 3)
	ds := &recorder{columns: matchingColumns(), rows: 3}

	run := newRun(Options{})
	outcomes, err := Execute(context.Background(), ds, path, run)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Columns (the header check) strictly before Prepare, and Prepare
	// strictly before any Import.
	want := []string{"Columns", "Prepare", "Import", "Import", "Import"}
	if strings.Join(ds.calls, ",") != strings.Join(want, ",") {
		t.Errorf("calls = %v, want %v", ds.calls, want)
	}
	if len(outcomes) != 3 {
		t.Errorf("%d outcomes, want 3", len(outcomes))
	}
	if run.Count.Parsed != 3 {
		t.Errorf("Parsed = %d, want 3", run.Count.Parsed)
	}
	if run.Count.Created != 3 {
		t.Errorf("Created = %d, want 3 — the dataset moves its own "+
			"counters", run.Count.Created)
	}
}

// Execute fills in the resolved sheet name before anything downstream
// reads it, which is what lets a summary say which sheet was imported when
// the caller let it default.
func TestIntegrationExecuteResolvesTheSheetName(t *testing.T) {
	path := writeSheet(t, []string{"Code", "Name"}, 1)
	ds := &recorder{columns: matchingColumns(), rows: 1}

	run := newRun(Options{})
	if _, err := Execute(context.Background(), ds, path, run); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if run.Opt.Layout.Sheet != itSheet {
		t.Errorf("Layout.Sheet = %q, want %q",
			run.Opt.Layout.Sheet, itSheet)
	}
}

// The header check is the one gate, and it has to stop the run BEFORE
// anything is parsed: everything downstream of a moved column is
// confidently wrong rather than visibly broken.
func TestIntegrationHeaderMismatchStopsBeforeParsing(t *testing.T) {
	path := writeSheet(t, []string{"Code", "Something Else"}, 3)
	ds := &recorder{
		columns: matchingColumns(),
		rows:    3,
		headerAdvice: errors.New(
			"the column table lives in columns.go"),
	}

	run := newRun(Options{})
	outcomes, err := Execute(context.Background(), ds, path, run)

	if err == nil {
		t.Fatal("Execute imported a sheet whose header does not match")
	}
	if !strings.Contains(err.Error(), "columns.go") {
		t.Errorf("err = %v, want the dataset's own advice", err)
	}
	if outcomes != nil {
		t.Errorf("outcomes = %v, want none", outcomes)
	}
	// Nothing was parsed and nothing was imported.
	want := []string{"Columns", "HeaderAdvice"}
	if strings.Join(ds.calls, ",") != strings.Join(want, ",") {
		t.Errorf("calls = %v, want %v", ds.calls, want)
	}
}

// -skip-header-check warns and continues, which is the escape hatch for a
// workbook that really has been restructured deliberately.
func TestIntegrationSkipHeaderCheckImportsAnyway(t *testing.T) {
	path := writeSheet(t, []string{"Code", "Something Else"}, 2)
	ds := &recorder{columns: matchingColumns(), rows: 2}

	run := newRun(Options{SkipHeader: true})
	outcomes, err := Execute(context.Background(), ds, path, run)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(outcomes) != 2 {
		t.Errorf("%d outcomes, want 2", len(outcomes))
	}
	// HeaderAdvice is not consulted: the run was not stopped, so there
	// is nothing to advise about.
	for _, c := range ds.calls {
		if c == "HeaderAdvice" {
			t.Error("HeaderAdvice ran on a skipped header check")
		}
	}
}

// Parsed is the number Prepare found, BEFORE the limit, so a limited run
// still reports how big the workbook was. The limit changes only how many
// rows are imported.
func TestIntegrationLimitStopsEarlyButParsedReportsTheWhole(t *testing.T) {
	path := writeSheet(t, []string{"Code", "Name"}, 10)
	ds := &recorder{columns: matchingColumns(), rows: 10}

	run := newRun(Options{Limit: 3})
	outcomes, err := Execute(context.Background(), ds, path, run)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if run.Count.Parsed != 10 {
		t.Errorf("Parsed = %d, want 10 — the limit should not change "+
			"what was parsed", run.Count.Parsed)
	}
	if len(ds.importedRows) != 3 {
		t.Errorf("imported %v, want three rows", ds.importedRows)
	}
	if len(outcomes) != 3 {
		t.Errorf("%d outcomes, want 3", len(outcomes))
	}
}

// A limit above the row count is not an error and not a truncation.
func TestIntegrationLimitAboveTheRowCountImportsEverything(t *testing.T) {
	path := writeSheet(t, []string{"Code", "Name"}, 2)
	ds := &recorder{columns: matchingColumns(), rows: 2}

	run := newRun(Options{Limit: 99})
	if _, err := Execute(context.Background(), ds, path, run); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(ds.importedRows) != 2 {
		t.Errorf("imported %v, want both rows", ds.importedRows)
	}
}

// A cancelled context stops between rows rather than mid-row, and keeps
// what it already did. An import that threw away the completed rows would
// leave the operator with no record of what had been written.
func TestIntegrationCancellationStopsBetweenRowsAndKeepsTheWork(t *testing.T) {
	path := writeSheet(t, []string{"Code", "Name"}, 10)

	ctx, cancel := context.WithCancel(context.Background())
	ds := &recorder{columns: matchingColumns(), rows: 10}
	ds.onImport = func(i int, run *Run) Outcome {
		if i == 2 {
			cancel()
		}
		run.Count.Created++
		return Outcome{ExcelRow: 10 + i, Status: StatusCreated}
	}

	outcomes, err := Execute(ctx, ds, path, newRun(Options{}))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Rows 0, 1 and 2 ran; the check at the top of row 3 stopped it.
	if len(ds.importedRows) != 3 {
		t.Errorf("imported %v, want three rows before the stop",
			ds.importedRows)
	}
	if len(outcomes) != 3 {
		t.Errorf("%d outcomes, want the completed rows kept",
			len(outcomes))
	}
}

// Prepare's parse failures are merged with the imported rows and the whole
// lot is ordered by workbook row, so a reviewer can walk the report
// against the sheet instead of finding the failures in a block at the top.
func TestIntegrationPrepareFailuresAreMergedAndSorted(t *testing.T) {
	path := writeSheet(t, []string{"Code", "Name"}, 3)

	ds := &recorder{
		columns: matchingColumns(),
		rows:    3,
		prepareOutcomes: []Outcome{
			{ExcelRow: 11, Status: StatusFailed, Label: "parse-11"},
			{ExcelRow: 13, Status: StatusFailed, Label: "parse-13"},
		},
	}
	// Imported rows land on 10, 12 and 14, interleaving with the above.
	ds.onImport = func(i int, run *Run) Outcome {
		run.Count.Created++
		return Outcome{ExcelRow: 10 + i*2, Status: StatusCreated,
			Label: fmt.Sprintf("import-%d", 10+i*2)}
	}

	outcomes, err := Execute(context.Background(), ds, path, newRun(
		Options{}))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var rows []int
	for _, o := range outcomes {
		rows = append(rows, o.ExcelRow)
	}
	want := []int{10, 11, 12, 13, 14}
	if fmt.Sprint(rows) != fmt.Sprint(want) {
		t.Errorf("rows = %v, want %v", rows, want)
	}
}

// An error from Prepare stops the run, and the outcomes it returned
// alongside are dropped: a dataset that could not prepare has not told a
// coherent story about the rows it managed.
func TestIntegrationPrepareErrorStopsTheRun(t *testing.T) {
	path := writeSheet(t, []string{"Code", "Name"}, 3)

	ds := &recorder{
		columns:         matchingColumns(),
		rows:            3,
		prepareOutcomes: []Outcome{{ExcelRow: 11}},
		prepareErr:      errors.New("the lookup endpoint is down"),
	}

	outcomes, err := Execute(context.Background(), ds, path, newRun(
		Options{}))
	if err == nil {
		t.Fatal("Execute continued after Prepare failed")
	}
	if !strings.Contains(err.Error(), "lookup endpoint") {
		t.Errorf("err = %v, want Prepare's own error", err)
	}
	if outcomes != nil {
		t.Errorf("outcomes = %v, want none", outcomes)
	}
	if len(ds.importedRows) != 0 {
		t.Errorf("imported %v after a failed Prepare", ds.importedRows)
	}
}

// A workbook that will not open is one of the three things that stop a
// run, and the error has to name the file.
func TestIntegrationMissingWorkbookIsReported(t *testing.T) {
	ds := &recorder{columns: matchingColumns()}
	path := filepath.Join(t.TempDir(), "absent.xlsx")

	_, err := Execute(context.Background(), ds, path, newRun(Options{}))
	if err == nil {
		t.Fatal("Execute opened a file that does not exist")
	}
	if !strings.Contains(err.Error(), "absent.xlsx") {
		t.Errorf("err = %v, want it to name the file", err)
	}
	if len(ds.calls) != 0 {
		t.Errorf("calls = %v, want none before the file opened",
			ds.calls)
	}
}

// A dataset with nothing to import is not an error: an empty sheet, or one
// whose rows were all rejected by Prepare, still produces a report.
func TestIntegrationNoImportableRowsIsNotAnError(t *testing.T) {
	path := writeSheet(t, []string{"Code", "Name"}, 0)
	ds := &recorder{columns: matchingColumns(), rows: 0}

	outcomes, err := Execute(context.Background(), ds, path, newRun(
		Options{}))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(outcomes) != 0 {
		t.Errorf("%d outcomes, want none", len(outcomes))
	}
	want := []string{"Columns", "Prepare"}
	if strings.Join(ds.calls, ",") != strings.Join(want, ",") {
		t.Errorf("calls = %v, want %v", ds.calls, want)
	}
}
