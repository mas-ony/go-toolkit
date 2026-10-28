package xlsxtest

import (
	"archive/zip"
	"bytes"
	"io"
	"path/filepath"
	"testing"

	"github.com/xuri/excelize/v2"
)

// Sheet is the sheet excelize puts in a new file, and so the one every
// workbook written here holds.
const Sheet = "Sheet1"

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

// SetCell writes value into cell of Sheet, and fails the test if excelize
// refuses it, so a fill that sets its cells one at a time reads as the
// layout it builds rather than as the error handling around each cell.
func SetCell(t *testing.T, f *excelize.File, cell string, value any) {
	t.Helper()
	if err := f.SetCellValue(Sheet, cell, value); err != nil {
		t.Fatalf("SetCellValue %s: %v", cell, err)
	}
}

// MergeCell merges the block from one corner cell to the other on Sheet,
// and fails the test if excelize refuses it, as SetCell does for a value.
func MergeCell(t *testing.T, f *excelize.File, from, to string) {
	t.Helper()
	if err := f.MergeCell(Sheet, from, to); err != nil {
		t.Fatalf("MergeCell %s:%s: %v", from, to, err)
	}
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

// WriteEntry adds an entry named name, holding content, to the archive zw
// writes, and fails the test if the archive refuses it.
func WriteEntry(t *testing.T, zw *zip.Writer, name, content string) {
	t.Helper()
	w, err := zw.Create(name)
	if err == nil {
		_, err = io.WriteString(w, content)
	}
	if err != nil {
		t.Fatal(err)
	}
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

// Rebuild returns book, a workbook as WorkbookBytes hands one over, written
// again entry by entry, for a workbook excelize would not write. Every entry
// is passed through edit, which is handed the entry's name and content and
// returns the content to keep; a nil edit keeps each one as it was. Each of
// extra is added as an entry of its own, holding bytes nothing reads.
func Rebuild(
	t *testing.T, book []byte,
	edit func(name, content string) string, extra ...string,
) []byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(book), int64(len(book)))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, entry := range zr.File {
		rc, err := entry.Open()
		if err != nil {
			t.Fatal(err)
		}
		content, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		kept := string(content)
		if edit != nil {
			kept = edit(entry.Name, kept)
		}
		WriteEntry(t, zw, entry.Name, kept)
	}
	for _, name := range extra {
		WriteEntry(t, zw, name, "not read")
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
