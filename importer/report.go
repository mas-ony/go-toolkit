package importer

import (
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// orDefault substitutes a fallback for an empty column name, so a dataset
// that leaves one unset still produces a CSV with a named header rather
// than a blank cell an importer downstream cannot address.
func orDefault(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// PrintSummary writes the human-facing result of a run.
//
// It is separate from the structured log on purpose: the log is for a
// later investigation, this is read once by the person who just ran the
// command and then thrown away. Everything that needs to survive goes in
// the CSV.
func PrintSummary(
	w io.Writer,
	file string,
	ds Dataset,
	run *Run,
	outcomes []Outcome,
) {
	info := ds.Info()
	t := run.Count

	mode := "import"
	if info.Caveat != "" {
		mode += " — " + info.Caveat
	}
	if run.Opt.DryRun {
		mode = "dry run — nothing was written"
	}

	line := strings.Repeat("─", 72)
	fmt.Fprintf(w, "\n%s\n", line)
	fmt.Fprintf(w, "  %s\n", filepath.Base(file))
	fmt.Fprintf(w, "  %s · sheet %q · %s\n",
		info.Name, run.Opt.Layout.Sheet, mode)
	fmt.Fprintf(w, "%s\n", line)

	// The fixed counters. Created, updated and skipped are suppressed at
	// zero because most datasets produce only one of the three, and a
	// column of zeroes for outcomes this workbook cannot have is noise
	// in the block an operator actually reads.
	rows := []Counter{{Name: "rows parsed", Value: t.Parsed, Always: true}}
	if !run.Opt.DryRun {
		rows = append(rows,
			Counter{Name: "created", Value: t.Created},
			Counter{Name: "updated", Value: t.Updated},
			Counter{Name: "skipped", Value: t.Skipped},
		)
	}
	rows = append(rows,
		Counter{Name: "failed", Value: t.Failed, Always: true})
	rows = append(rows, t.Extras()...)

	width := 0
	for _, c := range rows {
		if len(c.Name) > width {
			width = len(c.Name)
		}
	}
	for _, c := range rows {
		if c.Value == 0 && !c.Always {
			continue
		}
		fmt.Fprintf(w, "  %-*s  %d\n", width, c.Name, c.Value)
	}

	var problems []Outcome
	for _, o := range outcomes {
		if len(o.Notes) > 0 || o.Failed() {
			problems = append(problems, o)
		}
	}
	if len(problems) == 0 {
		fmt.Fprintf(w, "%s\n  No problems.\n", line)
		return
	}

	fmt.Fprintf(w, "%s\n  Rows needing attention:\n\n", line)
	for _, o := range problems {
		fmt.Fprintf(w, "  row %-5d %-20s %-10s %s\n",
			o.ExcelRow, o.Label, o.Status, o.ID)
		for _, n := range o.Notes {
			fmt.Fprintf(w, "            · %s\n", n)
		}
	}
	fmt.Fprintln(w)
}

// WriteReport writes one CSV line per workbook row.
//
// The row number comes first so a reviewer can go straight to the cell in
// the spreadsheet, and the notes are joined into one field because their
// number varies per row and a ragged CSV is harder to open than a long
// field.
//
// Every value is written as it came. A spreadsheet opening the file reads a
// cell that begins with "=", "+", "-" or "@" as a formula, and the labels
// and notes come from the workbook's own cells, so a report from a
// workbook of uncertain origin deserves the same care as the workbook.
//
// # Keep this file
//
// For a dataset that cannot match a row on a re-run — one whose Caveat
// says so — the id column here is the ONLY record of what the run
// created, and the only way to correct one of those rows afterwards is to
// address it by that id. A run without -report leaves nothing behind but
// the log.
func WriteReport(path string, ds Dataset, outcomes []Outcome) error {
	shape := ds.Report()

	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("creating %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	w := csv.NewWriter(f)

	header := make([]string, 0, len(shape.Fields)+5)
	header = append(header, "excel_row",
		orDefault(shape.Label, "label"),
		"status",
		orDefault(shape.ID, "id"))
	header = append(header, shape.Fields...)
	header = append(header, "notes")
	if err := w.Write(header); err != nil {
		return err
	}

	record := make([]string, len(header))
	for _, o := range outcomes {
		record[0] = strconv.Itoa(o.ExcelRow)
		record[1] = o.Label
		record[2] = string(o.Status)
		record[3] = o.ID
		for i, name := range shape.Fields {
			record[4+i] = o.Fields[name]
		}
		record[len(record)-1] = strings.Join(o.Notes, "; ")

		if err := w.Write(record); err != nil {
			return err
		}
	}

	w.Flush()
	if err := w.Error(); err != nil {
		return err
	}
	return f.Close()
}
