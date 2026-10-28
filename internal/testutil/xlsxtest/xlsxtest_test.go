package xlsxtest

// Tests for xlsxtest.go: a workbook written to a path, or handed over in
// memory, opens again in excelize, holding Sheet and what the fill put
// there with SetCell and MergeCell, and still does once Rebuild has
// written it again, with what an edit changed and each entry it added.

import (
	"archive/zip"
	"bytes"
	"path/filepath"
	"slices"
	"strings"
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

// The block MergeCell joins is one merged range of Sheet, holding the
// value of its top-left cell.
func TestMergeCell(t *testing.T) {
	t.Parallel()
	book := WorkbookBytes(t, func(t *testing.T, f *excelize.File) {
		SetCell(t, f, "B2", "kept")
		MergeCell(t, f, "B2", "C3")
	})
	f, err := excelize.OpenReader(bytes.NewReader(book))
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	defer func() { _ = f.Close() }()
	merged, err := f.GetMergeCells(Sheet)
	if err != nil || len(merged) != 1 {
		t.Fatalf("merged ranges = %v, %v, want one", merged, err)
	}
	m := merged[0]
	if m.GetStartAxis() != "B2" || m.GetEndAxis() != "C3" ||
		m.GetCellValue() != "kept" {
		t.Errorf("merged %s:%s holding %q, want B2:C3 holding kept",
			m.GetStartAxis(), m.GetEndAxis(), m.GetCellValue())
	}
}

// Without an edit the rebuilt workbook reads as the one it was built from,
// with each extra entry added to it; with one, an entry holds what the edit
// returned for it.
func TestRebuild(t *testing.T) {
	t.Parallel()
	book := WorkbookBytes(t, keep)

	kept := Rebuild(t, book, nil, "xl/extra.bin")
	zr, err := zip.NewReader(bytes.NewReader(kept), int64(len(kept)))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	if !slices.ContainsFunc(zr.File, func(e *zip.File) bool {
		return e.Name == "xl/extra.bin"
	}) {
		t.Error("the extra entry is not in the rebuilt archive")
	}
	f, err := excelize.OpenReader(bytes.NewReader(kept))
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	wantKept(t, f)

	edited := Rebuild(t, book, func(_, content string) string {
		return strings.ReplaceAll(content, ">kept<", ">made<")
	})
	f, err = excelize.OpenReader(bytes.NewReader(edited))
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	defer func() { _ = f.Close() }()
	if got, err := f.GetCellValue(Sheet, "B2"); err != nil ||
		got != "made" {
		t.Errorf("B2 = %q, %v, want the edit's made", got, err)
	}
}
