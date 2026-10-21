package xlsxtest

import (
	"path/filepath"
	"testing"

	"github.com/xuri/excelize/v2"
)

// Sheet is the sheet excelize puts in a new file, and so the one every
// workbook written here holds.
const Sheet = "Sheet1"

// SetCell writes value into cell of Sheet, and fails the test if excelize
// refuses it, so a fill that sets its cells one at a time reads as the
// layout it builds rather than as the error handling around each cell.
func SetCell(t *testing.T, f *excelize.File, cell string, value any) {
	t.Helper()
	if err := f.SetCellValue(Sheet, cell, value); err != nil {
		t.Fatalf("SetCellValue %s: %v", cell, err)
	}
}

// build is excelize's new file with fill applied to it, closed when the
// test ends: the workbook each function below then saves or hands over.
func build(
	t *testing.T,
	fill func(t *testing.T, f *excelize.File),
) *excelize.File {
	t.Helper()
	f := excelize.NewFile()
	t.Cleanup(func() { _ = f.Close() })
	fill(t, f)
	return f
}

// SaveWorkbook builds a workbook from excelize's new file, with fill
// applied to it, and saves it at path, which it returns.
func SaveWorkbook(
	t *testing.T,
	path string,
	fill func(t *testing.T, f *excelize.File),
) string {
	t.Helper()
	if err := build(t, fill).SaveAs(path); err != nil {
		t.Fatalf("SaveAs %s: %v", path, err)
	}
	return path
}

// WriteWorkbook is SaveWorkbook under name in a directory of the test's
// own, for a workbook nothing has to sit beside.
func WriteWorkbook(
	t *testing.T,
	name string,
	fill func(t *testing.T, f *excelize.File),
) string {
	t.Helper()
	return SaveWorkbook(t, filepath.Join(t.TempDir(), name), fill)
}

// WorkbookBytes builds the workbook SaveWorkbook would save and returns it
// as an upload holds one: in memory, at no path.
func WorkbookBytes(
	t *testing.T,
	fill func(t *testing.T, f *excelize.File),
) []byte {
	t.Helper()
	buf, err := build(t, fill).WriteToBuffer()
	if err != nil {
		t.Fatalf("WriteToBuffer: %v", err)
	}
	return buf.Bytes()
}
