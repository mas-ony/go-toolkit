package xlsxtest

// Tests for xlsxtest.go: a workbook written to a path, or handed over in
// memory, opens again in excelize, holding Sheet and what the fill put
// there with SetCell.

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/xuri/excelize/v2"
)

// keep is the fill both tests write: one cell, in Sheet.
func keep(t *testing.T, f *excelize.File) {
	t.Helper()
	SetCell(t, f, "B2", "kept")
}

// wantKept fails the test unless f holds Sheet alone, with what keep
// wrote there.
func wantKept(t *testing.T, f *excelize.File) {
	t.Helper()
	defer func() { _ = f.Close() }()
	if got := f.GetSheetList(); len(got) != 1 || got[0] != Sheet {
		t.Errorf("sheets = %v, want only %s", got, Sheet)
	}
	if got, err := f.GetCellValue(Sheet, "B2"); err != nil ||
		got != "kept" {
		t.Errorf("B2 = %q, %v, want kept", got, err)
	}
}

// The fill reaches the saved file, the sheet SetCell wrote is Sheet, and
// the path handed back is the one the file is at.
func TestWriteWorkbook(t *testing.T) {
	t.Parallel()
	path := WriteWorkbook(t, "book.xlsx", keep)
	if filepath.Base(path) != "book.xlsx" {
		t.Errorf("WriteWorkbook returned %q", path)
	}

	f, err := excelize.OpenFile(path)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	wantKept(t, f)
}

// The bytes handed back are the same workbook, read from memory.
func TestWorkbookBytes(t *testing.T) {
	t.Parallel()
	f, err := excelize.OpenReader(bytes.NewReader(WorkbookBytes(t, keep)))
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	wantKept(t, f)
}
