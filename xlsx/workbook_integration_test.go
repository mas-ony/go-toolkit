//go:build integration

package xlsx

// Integration tests for the xlsx package.
//
// The unit suite already writes real workbooks and reads them back, so
// "integration" here is not about using a real file. It is about the five
// things that suite does not reach, and what they have in common is the
// reason they are worth the extra file: each produces a WRONG ANSWER
// rather than an error, so nothing downstream can notice.
//
//   - The 1904 epoch shifts every date by four years and a day, and every
//     shifted date is plausible.
//   - A cell carrying a date number format has to read as its serial;
//     with the format applied, Date cannot resolve it.
//   - A workbook saved without cached formula results reads those cells as
//     empty, which is indistinguishable from a blank cell in the source;
//     beside it, a cached result has to read as that result.
//   - A relative link resolves against the workbook's directory whatever
//     the working directory is, and can climb out of it with Foreign
//     false.
//   - A data race on a shared *File corrupts whatever it corrupts.
//
//	go test -tags integration -run Integration ./xlsx
//
// Workbooks are written to a temporary directory; nothing else is needed.

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/mas-ony/go-toolkit/internal/testutil"
	"github.com/mas-ony/go-toolkit/internal/testutil/xlsxtest"
	"github.com/xuri/excelize/v2"
)

// serial is the date serial the epoch and number-format tests write.
const serial = 35000

// linkedWorkbook writes a workbook into dir whose column A holds one link
// per target, alternating between the two storage forms Hyperlink reads —
// a real hyperlink in the sheet's relationships, and a HYPERLINK() formula
// with a literal path — so every test here covers both.
//
// The workbook is saved into dir itself, because every relative target
// resolves against the workbook's own directory, which is why it goes
// through xlsxtest.SaveWorkbook rather than xlsxtest.WriteWorkbook.
func linkedWorkbook(t *testing.T, dir string, targets ...string) string {
	t.Helper()
	fill := func(t *testing.T, f *excelize.File) {
		for i, target := range targets {
			cell := fmt.Sprintf("A%d", i+1)
			if i%2 == 0 {
				xlsxtest.SetCell(t, f, cell, "open")
				setLink(t, f, cell, target)
			} else {
				setFormula(t, f, cell,
					fmt.Sprintf(`HYPERLINK(%q,"open")`, target))
			}
		}
	}
	return xlsxtest.SaveWorkbook(t, filepath.Join(dir, "book.xlsx"), fill)
}

// bareLayout reads Sheet from its first row, with no header above the
// data, which is how most of the workbooks here are laid out.
func bareLayout() Layout {
	return Layout{Sheet: xlsxtest.Sheet, HeaderRow: 1, FirstRow: 1}
}

// fillProduct writes 2 into A1, 3 into B1, and A1*B1 into C1 as a formula
// excelize stores without calculating, which is what a workbook built by
// a script looks like.
func fillProduct(t *testing.T, f *excelize.File) {
	t.Helper()
	xlsxtest.SetCell(t, f, "A1", 2)
	xlsxtest.SetCell(t, f, "B1", 3)
	setFormula(t, f, "C1", "A1*B1")
}

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
			path := xlsxtest.WriteWorkbook(t, "epoch.xlsx",
				func(t *testing.T, f *excelize.File) {
					if err := f.SetWorkbookProps(
						&excelize.WorkbookPropsOptions{
							Date1904: &epoch,
						}); err != nil {
						t.Fatalf("SetWorkbookProps: %v", err)
					}
					xlsxtest.SetCell(t, f, "A1", serial)
				})

			w := openWorkbook(t, path, bareLayout())

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
				t.Errorf("serial %d read as %s, want %s",
					serial, have, c.want)
			}
		})
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
	path := xlsxtest.WriteWorkbook(t, "formatted.xlsx",
		func(t *testing.T, f *excelize.File) {
			// 14 is Excel's built-in short date format.
			style, err := f.NewStyle(&excelize.Style{NumFmt: 14})
			if err != nil {
				t.Fatalf("NewStyle: %v", err)
			}
			xlsxtest.SetCell(t, f, "A1", serial)
			if err := f.SetCellStyle(
				xlsxtest.Sheet, "A1", "A1", style); err != nil {
				t.Fatalf("SetCellStyle: %v", err)
			}
		})

	w := openWorkbook(t, path, bareLayout())

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

// Open's comment warns that a workbook saved by a script carries no cached
// formula values, so those cells read as EMPTY rather than as the formula
// or its result — and that a caller treating blank as missing will
// under-count a file nobody can see anything wrong with.
//
// That is a documented operational trap with no test, and it is
// reproducible in three lines: excelize writes the formula and calculates
// nothing, which is exactly what a script-built workbook looks like.
func TestIntegrationUncachedFormulaCellsReadAsEmpty(t *testing.T) {
	path := xlsxtest.WriteWorkbook(t, "formulas.xlsx", fillProduct)

	w := openWorkbook(t, path, bareLayout())

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
	path := xlsxtest.WriteWorkbook(t, "cached.xlsx",
		func(t *testing.T, f *excelize.File) {
			fillProduct(t, f)
			// What Excel does on save, and what a script usually does
			// not: compute the value and store it beside the formula.
			v, err := f.CalcCellValue(xlsxtest.Sheet, "C1")
			if err != nil {
				t.Skipf("this excelize cannot calculate the cell: %v",
					err)
			}
			xlsxtest.SetCell(t, f, "D1", v)
		})

	w := openWorkbook(t, path, bareLayout())

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

// doc.go claims the grid is never written after Open, so the methods that
// only read it are safe to call concurrently. Nothing enforces that — a
// later change could add a cache to Cell or Caption and nothing would
// complain — so the claim needs a test that would notice.
//
// Meaningful mainly under -race. Without it the test still checks that
// every reader saw the right value under contention, which is the weaker
// half.
func TestIntegrationGridReadsAreSafeForConcurrentUse(t *testing.T) {
	path := xlsxtest.WriteWorkbook(t, "grid.xlsx",
		func(t *testing.T, f *excelize.File) {
			xlsxtest.SetCell(t, f, "A1", "code")
			xlsxtest.SetCell(t, f, "B1", "name")
			for row := 2; row <= 200; row++ {
				xlsxtest.SetCell(t, f, fmt.Sprintf("A%d", row),
					fmt.Sprintf("C-%04d", row))
				xlsxtest.SetCell(t, f, fmt.Sprintf("B%d", row),
					fmt.Sprintf("N-%04d", row))
			}
		})

	w := openWorkbook(t, path, Layout{
		Sheet: xlsxtest.Sheet, HeaderRow: 1, FirstRow: 2})

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

// A relative link is stored relative to the WORKBOOK, so it has to resolve
// there whatever the process's working directory is — including after the
// workbook was opened by a relative path and the process has moved on.
//
// That last case is what the filepath.Abs in Open is for, and nothing else
// holds it in place. Without it Dir keeps the relative directory, and every
// link silently re-anchors to wherever the process happens to be when Link
// is called: a link that reads as broken on a machine where the file is
// sitting right next to the workbook.
func TestIntegrationLinksResolveAgainstTheWorkbookNotTheProcess(t *testing.T) {
	root := t.TempDir()
	books := filepath.Join(root, "books")
	elsewhere := filepath.Join(root, "elsewhere")
	testutil.WriteFile(t, filepath.Join(books, "scans", "1.pdf"),
		[]byte("first"))
	testutil.WriteFile(t, filepath.Join(books, "scans", "2.pdf"),
		[]byte("second"))
	if err := os.MkdirAll(elsewhere, 0o755); err != nil {
		t.Fatal(err)
	}
	linkedWorkbook(t, books, "scans/1.pdf", "scans/2.pdf")

	// Open by a RELATIVE path, then move away before resolving anything.
	t.Chdir(books)
	w := openWorkbook(t, "book.xlsx", bareLayout())
	t.Chdir(elsewhere)

	// The premise: resolved against the process, the target is not there.
	if _, err := os.Stat(filepath.Join("scans", "1.pdf")); err == nil {
		t.Fatal("the target also exists relative to the working " +
			"directory, so this test would prove nothing")
	}

	for row, want := range map[int]string{1: "first", 2: "second"} {
		lt, err := w.Link(row, "A")
		if err != nil {
			t.Fatalf("row %d: Link: %v", row, err)
		}
		if lt.Foreign {
			t.Errorf("row %d: a relative target was marked foreign", row)
		}
		got, err := os.ReadFile(lt.Path)
		if err != nil {
			t.Errorf("row %d resolved to %s, which does not open from "+
				"another directory: %v", row, lt.Path, err)
			continue
		}
		if string(got) != want {
			t.Errorf("row %d opened %q, want %q", row, got, want)
		}
	}
}

// Foreign describes the SHAPE of a target — absolute or not — and is not a
// containment check. A relative target can climb out of the workbook's
// directory with Foreign false, and Link resolves it to the real file
// there. doc.go says so; this pins it, so that tightening it later is a
// decision rather than an accident. What actually limits what a link may
// name is the extension gate applied when the file is read.
func TestIntegrationARelativeLinkCanLeaveTheWorkbookDirectory(t *testing.T) {
	root := t.TempDir()
	books := filepath.Join(root, "books")
	outside := filepath.Join(root, "outside.pdf")
	testutil.WriteFile(t, outside, []byte("outside"))
	if err := os.MkdirAll(books, 0o755); err != nil {
		t.Fatal(err)
	}
	path := linkedWorkbook(t, books, "../outside.pdf")

	w := openWorkbook(t, path, bareLayout())
	lt, err := w.Link(1, "A")
	if err != nil {
		t.Fatalf("Link: %v", err)
	}
	if lt.Foreign {
		t.Error("a relative target was marked foreign")
	}
	if filepath.Clean(lt.Path) != outside {
		t.Errorf("resolved to %s, want %s", lt.Path, outside)
	}
	if _, err := os.Stat(lt.Path); err != nil {
		t.Errorf("the resolved path does not exist: %v", err)
	}
}
