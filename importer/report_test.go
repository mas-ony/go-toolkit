package importer

// Tests for report.go: the two renderings of a finished run.
//
// Both are asserted on their exact output rather than on "it did not
// error", because both are read by a person and the whole value of each is
// in what it shows and what it leaves out. The summary suppresses counters
// a workbook cannot have; the CSV has to keep every row the same shape so
// a spreadsheet can open it.

import (
	"bytes"
	"context"
	"encoding/csv"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/mas-ony/go-toolkit/xlsx"
)

// reportDataset supplies only the two methods the report renderers use.
type reportDataset struct {
	info  Info
	shape ReportShape
}

// summaryRun builds the Run a summary is rendered from.
func summaryRun(opt Options, c *Counters) *Run {
	return &Run{Log: zerolog.Nop(), Opt: opt, Count: c}
}

// readCSV parses a written report back.
func readCSV(t *testing.T, path string) [][]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("opening the report: %v", err)
	}
	defer func() { _ = f.Close() }()

	records, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatalf("parsing the report: %v", err)
	}
	return records
}

func (d reportDataset) Info() Info          { return d.info }
func (d reportDataset) Report() ReportShape { return d.shape }

func (reportDataset) Flags(*flag.FlagSet)      { panic("not used here") }
func (reportDataset) Validate() error          { panic("not used here") }
func (reportDataset) Layout() xlsx.Layout      { panic("not used here") }
func (reportDataset) Columns() []xlsx.Column   { panic("not used here") }
func (reportDataset) HeaderAdvice(error) error { panic("not used here") }
func (reportDataset) Len() int                 { panic("not used here") }

func (reportDataset) Connect(context.Context, Transport, *Run) error {
	panic("not used here")
}

func (reportDataset) Prepare(
	context.Context, *xlsx.File, *Run,
) ([]Outcome, error) {
	panic("not used here")
}

func (reportDataset) Import(context.Context, int, *Run) Outcome {
	panic("not used here")
}

// ----------------------------------------------------------------------------
// PrintSummary
// ----------------------------------------------------------------------------

// Created, updated and skipped are suppressed at zero because most
// datasets produce only one of the three, and a column of zeroes for
// outcomes this workbook cannot have is noise in the block an operator
// actually reads. Parsed and failed always print — a zero there is
// information.
func TestPrintSummarySuppressesZeroOutcomeCounters(t *testing.T) {
	c := &Counters{Parsed: 10, Created: 7}
	var buf bytes.Buffer

	PrintSummary(&buf, "/tmp/book.xlsx",
		reportDataset{info: Info{Name: "things"}},
		summaryRun(Options{}, c), nil)

	out := buf.String()
	for _, want := range []string{"rows parsed", "created", "failed"} {
		if !strings.Contains(out, want) {
			t.Errorf("summary is missing %q:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{"updated", "skipped"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("summary shows %q at zero:\n%s", unwanted, out)
		}
	}
	// The file is named by basename, not by the path it was given.
	if !strings.Contains(out, "book.xlsx") ||
		strings.Contains(out, "/tmp/book.xlsx") {
		t.Errorf("summary should name the file by basename:\n%s", out)
	}
}

// A dry run wrote nothing, so the three outcome counters are omitted
// entirely rather than shown as zeroes that look like failures.
func TestPrintSummaryDryRunOmitsTheWriteCounters(t *testing.T) {
	c := &Counters{Parsed: 4}
	var buf bytes.Buffer

	PrintSummary(&buf, "book.xlsx",
		reportDataset{info: Info{Name: "things"}},
		summaryRun(Options{DryRun: true}, c), nil)

	out := buf.String()
	if !strings.Contains(out, "dry run — nothing was written") {
		t.Errorf("summary does not say it was a dry run:\n%s", out)
	}
	for _, unwanted := range []string{"created", "updated", "skipped"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("a dry run shows %q:\n%s", unwanted, out)
		}
	}
	if !strings.Contains(out, "rows parsed") {
		t.Errorf("a dry run should still report what it parsed:\n%s", out)
	}
}

// The caveat is the warning that a re-run duplicates, and it has to reach
// the banner — it is the one thing on screen telling an operator not to
// just run it again.
func TestPrintSummaryShowsTheCaveat(t *testing.T) {
	var buf bytes.Buffer
	PrintSummary(&buf, "book.xlsx",
		reportDataset{info: Info{
			Name:   "things",
			Caveat: "a re-run duplicates every row",
		}},
		summaryRun(Options{}, &Counters{}), nil)

	if !strings.Contains(buf.String(), "a re-run duplicates every row") {
		t.Errorf("the caveat is missing:\n%s", buf.String())
	}
}

// A dry run replaces the mode line outright, so the caveat about
// duplicating does not appear on a run that wrote nothing and could not
// have duplicated anything.
func TestPrintSummaryDryRunReplacesTheCaveat(t *testing.T) {
	var buf bytes.Buffer
	PrintSummary(&buf, "book.xlsx",
		reportDataset{info: Info{
			Name:   "things",
			Caveat: "a re-run duplicates every row",
		}},
		summaryRun(Options{DryRun: true}, &Counters{}), nil)

	if strings.Contains(buf.String(), "duplicates") {
		t.Errorf("a dry run warns about duplicating:\n%s", buf.String())
	}
}

// Declared counters print at zero; incidental ones do not.
func TestPrintSummaryHonoursDeclaredCounters(t *testing.T) {
	c := &Counters{Parsed: 1}
	c.Declare("files uploaded")
	c.Inc("lookups cached")
	c.Add("lookups cached", -1) // back to zero, but not declared

	var buf bytes.Buffer
	PrintSummary(&buf, "book.xlsx",
		reportDataset{info: Info{Name: "things"}},
		summaryRun(Options{}, c), nil)

	out := buf.String()
	if !strings.Contains(out, "files uploaded") {
		t.Errorf("a declared counter is missing at zero:\n%s", out)
	}
	if strings.Contains(out, "lookups cached") {
		t.Errorf("an undeclared counter shows at zero:\n%s", out)
	}
}

// A row with notes is listed even when it did not fail — a warning names
// a cell somebody has to go and fix, and losing it would make the report
// the only record of it.
func TestPrintSummaryListsRowsWithNotesAndFailures(t *testing.T) {
	outcomes := []Outcome{
		{ExcelRow: 5, Label: "fine", Status: StatusCreated},
		{ExcelRow: 6, Label: "warned", Status: StatusCreated,
			Notes: []string{"column B: left empty"}},
		{ExcelRow: 7, Label: "broken", Status: StatusFailed,
			ID: "x-1"},
	}

	var buf bytes.Buffer
	PrintSummary(&buf, "book.xlsx",
		reportDataset{info: Info{Name: "things"}},
		summaryRun(Options{}, &Counters{Parsed: 3}), outcomes)

	out := buf.String()
	if !strings.Contains(out, "Rows needing attention") {
		t.Errorf("no attention block:\n%s", out)
	}
	for _, want := range []string{"warned", "broken",
		"column B: left empty", "x-1"} {
		if !strings.Contains(out, want) {
			t.Errorf("attention block is missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "fine") {
		t.Errorf("a clean row was listed as needing attention:\n%s", out)
	}
}

func TestPrintSummarySaysSoWhenThereAreNoProblems(t *testing.T) {
	var buf bytes.Buffer
	PrintSummary(&buf, "book.xlsx",
		reportDataset{info: Info{Name: "things"}},
		summaryRun(Options{}, &Counters{Parsed: 2}),
		[]Outcome{{ExcelRow: 5, Status: StatusCreated}})

	if !strings.Contains(buf.String(), "No problems.") {
		t.Errorf("a clean run should say so:\n%s", buf.String())
	}
}

// ----------------------------------------------------------------------------
// WriteReport
// ----------------------------------------------------------------------------

// The column order is a contract: the row number first so a reviewer can
// go straight to the cell, the dataset's own fields in the order it
// declared them, and the notes last because that field is long.
func TestWriteReportColumnOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.csv")
	ds := reportDataset{shape: ReportShape{
		Label:  "reference",
		ID:     "record_id",
		Fields: []string{"unit", "area"},
	}}

	outcomes := []Outcome{
		{
			ExcelRow: 6,
			Label:    "REF-1",
			Status:   StatusCreated,
			ID:       "42",
			Fields:   map[string]string{"unit": "7", "area": "12"},
			Notes:    []string{"first", "second"},
		},
	}
	if err := WriteReport(path, ds, outcomes); err != nil {
		t.Fatalf("WriteReport: %v", err)
	}

	got := readCSV(t, path)
	wantHeader := []string{"excel_row", "reference", "status",
		"record_id", "unit", "area", "notes"}
	if !reflect.DeepEqual(got[0], wantHeader) {
		t.Errorf("header = %v, want %v", got[0], wantHeader)
	}

	// The notes are joined rather than spread across columns, because
	// their number varies per row and a ragged CSV is harder to open.
	wantRow := []string{"6", "REF-1", "created", "42", "7", "12",
		"first; second"}
	if !reflect.DeepEqual(got[1], wantRow) {
		t.Errorf("row = %v, want %v", got[1], wantRow)
	}
}

// A dataset that leaves a column name unset still has to produce an
// addressable header rather than a blank cell.
func TestWriteReportFallsBackToDefaultColumnNames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.csv")

	if err := WriteReport(path, reportDataset{}, nil); err != nil {
		t.Fatalf("WriteReport: %v", err)
	}

	got := readCSV(t, path)
	wantHeader := []string{"excel_row", "label", "status", "id", "notes"}
	if !reflect.DeepEqual(got[0], wantHeader) {
		t.Errorf("header = %v, want %v", got[0], wantHeader)
	}
	if len(got) != 1 {
		t.Errorf("%d records, want the header alone", len(got))
	}
}

// Every line has the same shape whatever each row happened to set, which
// is what lets a spreadsheet open the file. A field with no value is an
// empty cell, not an omitted one — and a row must not inherit the
// previous row's values through the reused record buffer.
func TestWriteReportKeepsEveryRowTheSameShape(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.csv")
	ds := reportDataset{shape: ReportShape{
		Fields: []string{"unit", "area"},
	}}

	outcomes := []Outcome{
		{ExcelRow: 5, Status: StatusCreated,
			Fields: map[string]string{"unit": "7", "area": "12"},
			Notes:  []string{"a note"}},
		// Sets neither field and carries no notes.
		{ExcelRow: 6, Status: StatusSkipped},
	}
	if err := WriteReport(path, ds, outcomes); err != nil {
		t.Fatalf("WriteReport: %v", err)
	}

	got := readCSV(t, path)
	if len(got) != 3 {
		t.Fatalf("%d records, want 3", len(got))
	}
	want := []string{"6", "", "skipped", "", "", "", ""}
	if !reflect.DeepEqual(got[2], want) {
		t.Errorf("the second row = %v, want %v — a value leaked from "+
			"the row before it", got[2], want)
	}
}

// A value carrying the delimiter has to survive the round trip, which is
// what using encoding/csv rather than joining by hand buys.
func TestWriteReportQuotesAwkwardValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.csv")

	outcomes := []Outcome{{
		ExcelRow: 5,
		Label:    `a "quoted", comma'd label`,
		Status:   StatusFailed,
		Notes:    []string{"line one\nline two"},
	}}
	if err := WriteReport(path, reportDataset{}, outcomes); err != nil {
		t.Fatalf("WriteReport: %v", err)
	}

	got := readCSV(t, path)
	if got[1][1] != `a "quoted", comma'd label` {
		t.Errorf("label = %q, did not survive the round trip", got[1][1])
	}
	if got[1][4] != "line one\nline two" {
		t.Errorf("notes = %q, did not survive the round trip", got[1][4])
	}
}

func TestWriteReportReportsAnUnwritablePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "no-such-dir", "report.csv")

	err := WriteReport(path, reportDataset{}, nil)
	if err == nil {
		t.Fatal("WriteReport succeeded into a missing directory")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("err = %v, want it to name the path", err)
	}
}
