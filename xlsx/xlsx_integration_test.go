//go:build integration

package xlsx

// Integration tests for xlsx.
//
// The unit suite already writes real workbooks and reads them back, so
// "integration" here is not about using a real file. It is about the three
// things that suite does not reach, and what they have in common is the
// reason they are worth the extra file: each produces a WRONG ANSWER
// rather than an error, so nothing downstream can notice.
//
//   - The 1904 epoch shifts every date by four years and a day, and every
//     shifted date is plausible.
//   - A workbook saved without cached formula results reads those cells as
//     empty, which is indistinguishable from a blank cell in the source.
//   - A data race on a shared *File corrupts whatever it corrupts.
//
//	go test -tags integration -run Integration ./xlsx
//
// Workbooks are written to a temporary directory; nothing else is needed.

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/xuri/excelize/v2"
)

const itSheet = "Sheet1"

// writeWorkbook builds a workbook with fill applied to it and saves it,
// returning the path.
func writeWorkbook(t *testing.T, name string,
	fill func(t *testing.T, f *excelize.File)) string {
	t.Helper()

	f := excelize.NewFile()
	t.Cleanup(func() { _ = f.Close() })
	fill(t, f)

	path := filepath.Join(t.TempDir(), name)
	if err := f.SaveAs(path); err != nil {
		t.Fatalf("SaveAs %s: %v", name, err)
	}
	return path
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

// ----------------------------------------------------------------------------
// The 1904 epoch
// ----------------------------------------------------------------------------

// The same serial number means two different dates depending on a property
// buried in the workbook, and both readings are plausible. Open asks the
// file rather than the operator, and this is what holds that wiring
// together: uses1904 reaching Date1904, and Date1904 reaching Date.
//
// Serial 35000 is 1995-10-28 under the 1900 epoch and 1999-10-29 under
// 1904 — four years and a day apart, which is the shift the comment on
// uses1904 describes and the one nothing downstream could ever spot.
//
// Both dates are derived from the epochs themselves rather than from this
// package: the 1900 system counts from 1899-12-30 and the 1904 system from
// 1904-01-01, so 35000 days after each is what the expectations below say.
// Taking them from the code under test would make this test agree with
// whatever that code did.
func TestIntegrationDate1904IsReadFromTheWorkbook(t *testing.T) {
	const serial = "35000"

	for _, c := range []struct {
		name  string
		epoch bool
		want  string
	}{
		{"the default 1900 epoch", false, "1995-10-28"},
		{"a workbook authored with the 1904 epoch", true, "1999-10-29"},
	} {
		t.Run(c.name, func(t *testing.T) {
			epoch := c.epoch
			path := writeWorkbook(t, "epoch.xlsx",
				func(t *testing.T, f *excelize.File) {
					if err := f.SetWorkbookProps(
						&excelize.WorkbookPropsOptions{
							Date1904: &epoch,
						}); err != nil {
						t.Fatalf("SetWorkbookProps: %v", err)
					}
					if err := f.SetCellValue(
						itSheet, "A1", serial); err != nil {
						t.Fatalf("SetCellValue: %v", err)
					}
				})

			w := openWorkbook(t, path, Layout{
				Sheet: itSheet, HeaderRow: 1, FirstRow: 1})

			if w.Date1904() != c.epoch {
				t.Fatalf("Date1904() = %v, want %v — the property was "+
					"not read from the file", w.Date1904(), c.epoch)
			}

			got, err := Date(w.Cell(1, "A"), w.Date1904())
			if err != nil {
				t.Fatalf("Date: %v", err)
			}
			if got == nil {
				t.Fatal("Date returned nil for a populated cell")
			}
			if have := got.Time.Format("2006-01-02"); have != c.want {
				t.Errorf("serial %s read as %s, want %s",
					serial, have, c.want)
			}
		})
	}
}

// The failure this guards against, stated as a comparison: reading a
// 1904 workbook with the 1900 epoch is not an error, it is a date four
// years out. If the two ever agree, the epoch has stopped being consulted
// and every Mac-authored workbook is being read wrong.
func TestIntegrationTheWrongEpochIsSilentlyWrong(t *testing.T) {
	const serial = "35000"

	right, err := Date(serial, true)
	if err != nil {
		t.Fatal(err)
	}
	wrong, err := Date(serial, false)
	if err != nil {
		t.Fatal(err)
	}
	if right.Time.Equal(wrong.Time) {
		t.Fatal("both epochs produced the same date; the flag is " +
			"being ignored")
	}

	gap := right.Time.Sub(wrong.Time)
	// Four years and a day, two of which are leap years in this span.
	if want := 1462 * 24 * time.Hour; gap != want {
		t.Errorf("the epochs differ by %v, want %v", gap, want)
	}
}

// ----------------------------------------------------------------------------
// Cached formula results
// ----------------------------------------------------------------------------

// Open's comment warns that a workbook saved by a script carries no cached
// formula values, so those cells read as EMPTY rather than as the formula
// or its result — and that a caller treating blank as missing will
// under-count a file nobody can see anything wrong with.
//
// That is a documented operational trap with no test, and it is
// reproducible in three lines: excelize writes the formula and calculates
// nothing, which is exactly what a script-built workbook looks like.
func TestIntegrationUncachedFormulaCellsReadAsEmpty(t *testing.T) {
	path := writeWorkbook(t, "formulas.xlsx",
		func(t *testing.T, f *excelize.File) {
			for cell, v := range map[string]any{
				"A1": 2, "B1": 3,
			} {
				if err := f.SetCellValue(itSheet, cell, v); err != nil {
					t.Fatalf("SetCellValue %s: %v", cell, err)
				}
			}
			if err := f.SetCellFormula(
				itSheet, "C1", "A1*B1"); err != nil {
				t.Fatalf("SetCellFormula: %v", err)
			}
		})

	w := openWorkbook(t, path, Layout{
		Sheet: itSheet, HeaderRow: 1, FirstRow: 1})

	// The literal cells are fine, which is what makes the empty one so
	// easy to misread as missing source data.
	if got := w.Cell(1, "A"); got != "2" {
		t.Errorf("A1 = %q, want 2", got)
	}
	if got := w.Cell(1, "B"); got != "3" {
		t.Errorf("B1 = %q, want 3", got)
	}

	if got := w.Cell(1, "C"); got != "" {
		t.Errorf("C1 = %q — a formula cell with no cached value is "+
			"expected to read as empty; if this now returns a result, "+
			"Open's warning is out of date and should be corrected",
			got)
	}

	// And the row counts as blank for a caller that maps only C, which is
	// the downstream consequence worth seeing written out.
	cols := []Column{{Col: "C", Captions: []string{"total"},
		Purpose: "total"}}
	if !w.RowIsBlank(1, cols) {
		t.Error("the row should read as blank when only the formula " +
			"column is mapped")
	}
	if rows := w.DataRows(cols); len(rows) != 0 {
		t.Errorf("DataRows = %v, want none — an uncached formula "+
			"column makes every row look empty", rows)
	}
}

// The other half: once a value IS cached, the same cell reads as the
// result rather than the formula text. Without this the test above could
// pass against a reader that never returned anything at all.
func TestIntegrationCachedFormulaResultsAreRead(t *testing.T) {
	path := writeWorkbook(t, "cached.xlsx",
		func(t *testing.T, f *excelize.File) {
			if err := f.SetCellValue(itSheet, "A1", 2); err != nil {
				t.Fatal(err)
			}
			if err := f.SetCellValue(itSheet, "B1", 3); err != nil {
				t.Fatal(err)
			}
			if err := f.SetCellFormula(
				itSheet, "C1", "A1*B1"); err != nil {
				t.Fatal(err)
			}
			// What Excel does on save, and what a script usually does
			// not: compute the value and store it beside the formula.
			v, err := f.CalcCellValue(itSheet, "C1")
			if err != nil {
				t.Skipf("this excelize cannot calculate the cell: %v",
					err)
			}
			if err := f.SetCellValue(itSheet, "D1", v); err != nil {
				t.Fatal(err)
			}
		})

	w := openWorkbook(t, path, Layout{
		Sheet: itSheet, HeaderRow: 1, FirstRow: 1})

	if got := w.Cell(1, "D"); got != "6" {
		t.Errorf("D1 = %q, want 6 — a stored result must be read", got)
	}
	// Crucially it is not the formula TEXT: RawCellValue yields the
	// cached value, and a reader that returned "A1*B1" would break every
	// generated column.
	if got := w.Cell(1, "D"); got == "A1*B1" {
		t.Error("the formula text was returned instead of its value")
	}
}

// ----------------------------------------------------------------------------
// Concurrency
// ----------------------------------------------------------------------------

// doc.go claims the grid is never written after Open, so the methods that
// only read it are safe to call concurrently. Nothing enforces that — a
// later change could add a cache to Cell or Caption and nothing would
// complain — so the claim needs a test that would notice.
//
// Meaningful mainly under -race. Without it the test still checks that
// every reader saw the right value under contention, which is the weaker
// half.
func TestIntegrationGridReadsAreSafeForConcurrentUse(t *testing.T) {
	path := writeWorkbook(t, "grid.xlsx",
		func(t *testing.T, f *excelize.File) {
			if err := f.SetCellValue(
				itSheet, "A1", "code"); err != nil {
				t.Fatal(err)
			}
			if err := f.SetCellValue(
				itSheet, "B1", "name"); err != nil {
				t.Fatal(err)
			}
			for row := 2; row <= 200; row++ {
				if err := f.SetCellValue(itSheet,
					fmt.Sprintf("A%d", row),
					fmt.Sprintf("C-%04d", row)); err != nil {
					t.Fatal(err)
				}
				if err := f.SetCellValue(itSheet,
					fmt.Sprintf("B%d", row),
					fmt.Sprintf("N-%04d", row)); err != nil {
					t.Fatal(err)
				}
			}
		})

	w := openWorkbook(t, path, Layout{
		Sheet: itSheet, HeaderRow: 1, FirstRow: 2})

	cols := []Column{
		{Col: "A", Captions: []string{"code"}, Purpose: "code"},
		{Col: "B", Captions: []string{"name"}, Purpose: "name"},
	}
	if err := w.VerifyHeaders(cols); err != nil {
		t.Fatalf("VerifyHeaders: %v", err)
	}

	const readers = 16
	errs := make(chan string, readers)
	var wg sync.WaitGroup

	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			for i := 0; i < 40; i++ {
				rows := w.DataRows(cols)
				if len(rows) != 199 {
					errs <- fmt.Sprintf(
						"reader %d: DataRows = %d, want 199",
						r, len(rows))
					return
				}
				row := rows[(r+i)%len(rows)]
				want := fmt.Sprintf("C-%04d", row)
				if got := w.Cell(row, "A"); got != want {
					errs <- fmt.Sprintf(
						"reader %d: A%d = %q, want %q",
						r, row, got, want)
					return
				}
				if got := w.Caption("A"); got != "code" {
					errs <- fmt.Sprintf(
						"reader %d: Caption(A) = %q, want code",
						r, got)
					return
				}
				if w.RowIsBlank(row, cols) {
					errs <- fmt.Sprintf(
						"reader %d: row %d read as blank", r, row)
					return
				}
				_ = w.RowCount()
				_ = w.LastDataRow()
			}
		}(r)
	}

	wg.Wait()
	close(errs)
	for msg := range errs {
		t.Error(msg)
	}
}

// Open calls RawCellValue "the load-bearing option", and until this test
// nothing noticed if it were turned off.
//
// The difference only shows on a cell carrying a number FORMAT. Without
// the option excelize applies that format and hands back what the sheet
// DISPLAYS — "10/28/95", or whatever the author's locale renders — instead
// of the serial underneath. Date cannot resolve a formatted string of that
// shape, and the whole date path would start failing on workbooks that
// look perfectly ordinary in Excel.
//
// A plain unformatted number cannot show this: formatted and raw are the
// same string there, which is why every other test in the package passes
// either way.
func TestIntegrationRawCellValueBypassesTheNumberFormat(t *testing.T) {
	const serial = 35000

	path := writeWorkbook(t, "formatted.xlsx",
		func(t *testing.T, f *excelize.File) {
			// 14 is Excel's built-in short date format.
			style, err := f.NewStyle(&excelize.Style{NumFmt: 14})
			if err != nil {
				t.Fatalf("NewStyle: %v", err)
			}
			if err := f.SetCellValue(
				itSheet, "A1", serial); err != nil {
				t.Fatalf("SetCellValue: %v", err)
			}
			if err := f.SetCellStyle(
				itSheet, "A1", "A1", style); err != nil {
				t.Fatalf("SetCellStyle: %v", err)
			}
		})

	w := openWorkbook(t, path, Layout{
		Sheet: itSheet, HeaderRow: 1, FirstRow: 1})

	got := w.Cell(1, "A")
	if got != "35000" {
		t.Fatalf("A1 = %q, want the raw serial 35000 — the cell's "+
			"number format was applied, which is what RawCellValue "+
			"exists to prevent", got)
	}

	// And the consequence: the raw serial is what Date can resolve.
	d, err := Date(got, w.Date1904())
	if err != nil {
		t.Fatalf("Date(%q): %v", got, err)
	}
	if have := d.Time.Format("2006-01-02"); have != "1995-10-28" {
		t.Errorf("serial read as %s, want 1995-10-28", have)
	}
}
