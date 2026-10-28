// Package xlsxtest writes the workbooks the spreadsheet suites read: a
// workbook built from excelize's new file, filled by the test, and saved
// where the test needs it or handed over in memory, plus the name of the
// one sheet such a workbook holds and the cell setter and merger a fill
// writes that sheet with. For a workbook excelize would not write, such as
// one carrying a VBA project, Rebuild writes one of these again entry by
// entry, with what an edit changes and the entries it is told to add.
//
// The xlsx suites read the workbooks this writes, and the importer suite
// hands them to Execute, which opens them through the xlsx package. One
// writer for both means a workbook the importer is tested with is built
// exactly the way the reader's own fixtures are.
package xlsxtest
