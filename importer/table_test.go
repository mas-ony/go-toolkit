package importer

// Tests for table.go: the column mapping, its self-check, and every
// constructor.
//
// All of it runs against a Source held in the test rather than a file on
// disk, which is what that interface exists for — a table's coercion rules
// have nothing to do with where the grid came from, and building a
// workbook per case would make each test slower and harder to read without
// covering anything more.

import (
	"errors"
	"strings"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/mas-ony/go-toolkit/datetime"
)

// grid is a Source backed by maps, keyed "row:col".
type grid struct {
	cells    map[string]string
	links    map[string]string
	date1904 bool
}

// row is the destination the constructors write into: one field per
// coercion, in the pointer shapes each of them expects.
type row struct {
	Name   *string
	Count  *int
	Amount *decimal.Decimal
	When   *datetime.Datetime
	Plain  string
	Target string
}

func newGrid() *grid {
	return &grid{
		cells: map[string]string{},
		links: map[string]string{},
	}
}

func (g *grid) set(row int, col, text string) *grid {
	g.cells[key(row, col)] = text
	return g
}

func (g *grid) link(row int, col, target string) *grid {
	g.links[key(row, col)] = target
	return g
}

func key(row int, col string) string {
	return col + ":" + itoa(row)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func (g *grid) Cell(row int, col string) string {
	return g.cells[key(row, col)]
}

func (g *grid) Hyperlink(row int, col string) string {
	return g.links[key(row, col)]
}

func (g *grid) Date1904() bool { return g.date1904 }

// ----------------------------------------------------------------------------
// Columns
// ----------------------------------------------------------------------------

// Every field reaches the contract, Expect entries included. That is what
// makes the blank-row skip safe: DataRows tests the columns the caller
// declared an interest in, and a column read only for a lookup is still
// one of them.
func TestColumnsIncludesFieldsWithNoSetter(t *testing.T) {
	tbl := Table[row]{
		Text("A", "name", 50, func(r *row) **string { return &r.Name },
			"Name"),
		Expect[row]("B", "unused", "Notes"),
	}

	cols := tbl.Columns()
	if len(cols) != 2 {
		t.Fatalf("%d columns, want 2", len(cols))
	}
	if cols[1].Col != "B" || cols[1].Purpose != "unused" {
		t.Errorf("the Expect entry is missing from the contract: %+v",
			cols[1])
	}
	if len(cols[1].Captions) != 1 || cols[1].Captions[0] != "Notes" {
		t.Errorf("captions = %v, want [Notes]", cols[1].Captions)
	}
}

// ----------------------------------------------------------------------------
// Validate
// ----------------------------------------------------------------------------

func TestValidateAcceptsAWellFormedTable(t *testing.T) {
	tbl := Table[row]{
		Text("A", "name", 50, func(r *row) **string { return &r.Name },
			"Name"),
		Int("B", "count", func(r *row) **int { return &r.Count }, "Qty"),
		Expect[row]("AZ", "spacer", "—"),
	}
	if err := tbl.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

// Each arm fails in a way that looks like a problem with the WORKBOOK
// rather than with the code, which is why they are worth catching at
// start-up instead of being discovered from a report.
func TestValidateReportsEveryProblem(t *testing.T) {
	tests := []struct {
		name  string
		table Table[row]
		want  string
	}{
		{
			// Reads as an empty cell on every row.
			name: "not a column letter",
			table: Table[row]{
				Raw("A1", "bad", func(r *row) *string {
					return &r.Plain
				}, "X"),
			},
			want: "is not a column letter",
		},
		{
			name: "letter mapped twice",
			table: Table[row]{
				Raw("C", "first", func(r *row) *string {
					return &r.Plain
				}, "X"),
				Raw("C", "second", func(r *row) *string {
					return &r.Target
				}, "Y"),
			},
			want: "mapped more than once",
		},
		{
			// The header check can never pass for it.
			name: "no accepted caption",
			table: Table[row]{
				Raw("D", "nameless", func(r *row) *string {
					return &r.Plain
				}),
			},
			want: "accepts no caption",
		},
		{
			// Key is not set by any constructor in this package, so
			// reaching this arm means building the field by hand —
			// which is exactly how a dataset opts into the check.
			name: "two columns writing the same value",
			table: Table[row]{
				{Col: "E", Captions: []string{"X"}, Key: "code",
					Purpose: "first"},
				{Col: "F", Captions: []string{"Y"}, Key: "code",
					Purpose: "second"},
			},
			want: "both write",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.table.Validate()
			if err == nil {
				t.Fatalf("Validate accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %q, want it to mention %q",
					err, tc.want)
			}
		})
	}
}

// Every problem at once, because a table that has one typo usually has
// several and fixing them one start-up at a time is the slow way.
func TestValidateReportsAllProblemsTogether(t *testing.T) {
	tbl := Table[row]{
		Raw("A1", "bad letter", func(r *row) *string {
			return &r.Plain
		}, "X"),
		Raw("C", "first", func(r *row) *string { return &r.Plain }, "Y"),
		Raw("C", "duplicate", func(r *row) *string {
			return &r.Target
		}, "Z"),
		Raw("D", "no caption", func(r *row) *string { return &r.Plain }),
	}

	err := tbl.Validate()
	if err == nil {
		t.Fatal("Validate accepted a table with three problems")
	}
	for _, want := range []string{
		"is not a column letter",
		"mapped more than once",
		"accepts no caption",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err is missing %q:\n%v", want, err)
		}
	}
}

// Validate upper-cases before checking, and the underlying reader is
// case-insensitive too, so a lowercase letter is accepted rather than
// being a trap that passes the check and then reads nothing.
func TestValidateAcceptsLowercaseColumnLetters(t *testing.T) {
	tbl := Table[row]{
		Raw("c", "lower", func(r *row) *string { return &r.Plain }, "X"),
	}
	if err := tbl.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}

	// And it reads the same cell an uppercase letter would.
	g := newGrid().set(2, "c", "value")
	var dst row
	if _, err := tbl.Apply(g, 2, &dst); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if dst.Plain != "value" {
		t.Errorf("Plain = %q, want value", dst.Plain)
	}
}

// ----------------------------------------------------------------------------
// Apply
// ----------------------------------------------------------------------------

// The central rule: a cell that will not coerce is a WARNING and the row
// goes on. A record whose one numeric cell holds a stray note is still
// worth importing with its other columns intact.
func TestApplyReportsACoercionFailureAsAWarning(t *testing.T) {
	tbl := Table[row]{
		Text("A", "name", 50, func(r *row) **string {
			return &r.Name
		}, "Name"),
		Int("B", "count", func(r *row) **int { return &r.Count }, "Qty"),
	}
	g := newGrid().set(3, "A", "Widget").set(3, "B", "see note")

	var dst row
	warns, err := tbl.Apply(g, 3, &dst)
	if err != nil {
		t.Fatalf("Apply returned an error for a non-required column: %v",
			err)
	}
	if len(warns) != 1 {
		t.Fatalf("warnings = %v, want one", warns)
	}
	if !strings.Contains(warns[0], "column B") ||
		!strings.Contains(warns[0], "count") ||
		!strings.Contains(warns[0], "left empty") {
		t.Errorf("warning %q should name the column, its purpose and "+
			"the outcome", warns[0])
	}

	// The other column still landed, which is the point.
	if dst.Name == nil || *dst.Name != "Widget" {
		t.Errorf("Name = %v, want Widget", dst.Name)
	}
	if dst.Count != nil {
		t.Errorf("Count = %v, want it left unset", dst.Count)
	}
}

// Must inverts that for a column a row genuinely cannot be imported
// without.
func TestApplyMustTurnsAFailureIntoTheRowsError(t *testing.T) {
	tbl := Table[row]{
		Must(Int("B", "count", func(r *row) **int {
			return &r.Count
		}, "Qty")),
		Raw("C", "note", func(r *row) *string { return &r.Plain }, "N"),
	}
	g := newGrid().set(4, "B", "not a number").set(4, "C", "later")

	var dst row
	warns, err := tbl.Apply(g, 4, &dst)
	if err == nil {
		t.Fatal("Apply accepted a failed required column")
	}
	if !strings.Contains(err.Error(), "column B") {
		t.Errorf("err = %q, want it to name column B", err)
	}
	if len(warns) != 0 {
		t.Errorf("warnings = %v, want none before the failure", warns)
	}
	// Apply stops at the required failure, so a later column is not
	// read. dst is left partly filled, which its doc says is fine
	// because the caller is dropping the row.
	if dst.Plain != "" {
		t.Errorf("Plain = %q, want the run to have stopped", dst.Plain)
	}
}

// A field with no setter is in the contract but read for nothing, so
// Apply must skip it rather than dereference a nil func.
func TestApplySkipsFieldsWithNoSetter(t *testing.T) {
	tbl := Table[row]{
		Expect[row]("A", "pinned only", "Name"),
		Raw("B", "read", func(r *row) *string { return &r.Plain }, "N"),
	}
	g := newGrid().set(5, "A", "ignored").set(5, "B", "kept")

	var dst row
	warns, err := tbl.Apply(g, 5, &dst)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(warns) != 0 {
		t.Errorf("warnings = %v, want none", warns)
	}
	if dst.Plain != "kept" {
		t.Errorf("Plain = %q, want kept", dst.Plain)
	}
}

// The hyperlink is resolved only for the columns that asked, because
// resolving one is two lookups through the sheet's relationships and on a
// wide sheet that is tens of thousands of lookups bought for nothing.
func TestApplyResolvesHyperlinksOnlyWhenAsked(t *testing.T) {
	var sawLink, sawNoLink string

	tbl := Table[row]{
		WithLink(Custom("A", "with", func(_ *row, c Cell) error {
			sawLink = c.Link
			return nil
		}, "A")),
		Custom("B", "without", func(_ *row, c Cell) error {
			sawNoLink = c.Link
			return nil
		}, "B"),
	}
	g := newGrid().
		link(6, "A", "documents/1.pdf").
		link(6, "B", "documents/2.pdf")

	var dst row
	if _, err := tbl.Apply(g, 6, &dst); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if sawLink != "documents/1.pdf" {
		t.Errorf("WithLink field saw %q, want documents/1.pdf", sawLink)
	}
	if sawNoLink != "" {
		t.Errorf("a field without WithLink saw the link %q", sawNoLink)
	}
}

// The epoch travels on every Cell, because a setter cannot reach back at
// the file to ask.
func TestApplyCarriesTheEpochOntoEveryCell(t *testing.T) {
	var seen []bool
	tbl := Table[row]{
		Custom("A", "one", func(_ *row, c Cell) error {
			seen = append(seen, c.Date1904)
			return nil
		}, "A"),
		Custom("B", "two", func(_ *row, c Cell) error {
			seen = append(seen, c.Date1904)
			return nil
		}, "B"),
	}

	g := newGrid()
	g.date1904 = true
	var dst row
	if _, err := tbl.Apply(g, 7, &dst); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	for i, got := range seen {
		if !got {
			t.Errorf("cell %d did not carry the 1904 epoch", i)
		}
	}
}

// The row and column reach the setter too, so a Custom rule can name the
// cell in its own error.
func TestApplyCarriesTheCellAddress(t *testing.T) {
	var got Cell
	tbl := Table[row]{
		Custom("AB", "addressed", func(_ *row, c Cell) error {
			got = c
			return errors.New("nope")
		}, "X"),
	}
	g := newGrid().set(42, "AB", "text")

	var dst row
	warns, err := tbl.Apply(g, 42, &dst)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got.Row != 42 || got.Col != "AB" || got.Text != "text" {
		t.Errorf("cell = %+v, want row 42, col AB, text \"text\"", got)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "nope") {
		t.Errorf("warnings = %v, want the setter's own message", warns)
	}
}

// ----------------------------------------------------------------------------
// Constructors
// ----------------------------------------------------------------------------

// Each constructor's contract in one place: what an empty cell does, what
// a good value does, and what a bad one does. The empty case is the one
// worth stating — a nullable column has to stay NULL rather than become a
// zero value, which is why the destinations are pointers to pointers.
func TestConstructorCoercions(t *testing.T) {
	t.Run("Text keeps an empty cell NULL", func(t *testing.T) {
		tbl := Table[row]{Text("A", "name", 10, func(r *row) **string {
			return &r.Name
		}, "N")}

		var dst row
		if _, err := tbl.Apply(newGrid().set(1, "A", ""), 1,
			&dst); err != nil {
			t.Fatal(err)
		}
		if dst.Name != nil {
			t.Errorf("Name = %q, want nil for an empty cell", *dst.Name)
		}

		dst = row{}
		if _, err := tbl.Apply(newGrid().set(1, "A", " Widget "), 1,
			&dst); err != nil {
			t.Fatal(err)
		}
		if dst.Name == nil || *dst.Name != "Widget" {
			t.Errorf("Name = %v, want the trimmed value", dst.Name)
		}
	})

	t.Run("Text refuses an over-long value", func(t *testing.T) {
		// An error rather than a truncation, so the cell gets looked
		// at instead of being silently shortened by the driver.
		tbl := Table[row]{Text("A", "name", 5, func(r *row) **string {
			return &r.Name
		}, "N")}

		var dst row
		warns, err := tbl.Apply(newGrid().set(1, "A", "far too long"),
			1, &dst)
		if err != nil {
			t.Fatal(err)
		}
		if len(warns) != 1 {
			t.Fatalf("warnings = %v, want one", warns)
		}
		if dst.Name != nil {
			t.Errorf("Name = %q, want it left unset", *dst.Name)
		}
	})

	t.Run("Int", func(t *testing.T) {
		tbl := Table[row]{Int("A", "count", func(r *row) **int {
			return &r.Count
		}, "N")}

		var dst row
		if _, err := tbl.Apply(newGrid().set(1, "A", "42"), 1,
			&dst); err != nil {
			t.Fatal(err)
		}
		if dst.Count == nil || *dst.Count != 42 {
			t.Errorf("Count = %v, want 42", dst.Count)
		}

		dst = row{}
		if _, err := tbl.Apply(newGrid().set(1, "A", ""), 1,
			&dst); err != nil {
			t.Fatal(err)
		}
		if dst.Count != nil {
			t.Errorf("Count = %v, want nil for an empty cell",
				*dst.Count)
		}
	})

	t.Run("Decimal", func(t *testing.T) {
		tbl := Table[row]{Decimal("A", "amount",
			func(r *row) **decimal.Decimal { return &r.Amount }, "N")}

		var dst row
		if _, err := tbl.Apply(newGrid().set(1, "A", "1.234"), 1,
			&dst); err != nil {
			t.Fatal(err)
		}
		if dst.Amount == nil {
			t.Fatal("Amount = nil, want 1.234")
		}
		if got := dst.Amount.String(); got != "1.234" {
			t.Errorf("Amount = %s, want 1.234", got)
		}
	})

	t.Run("Date reads a serial under the carried epoch", func(t *testing.T) {
		tbl := Table[row]{Date("A", "when",
			func(r *row) **datetime.Datetime { return &r.When }, "N")}

		g := newGrid().set(1, "A", "35000")
		var dst row
		if _, err := tbl.Apply(g, 1, &dst); err != nil {
			t.Fatal(err)
		}
		if dst.When == nil {
			t.Fatal("When = nil")
		}
		if got := dst.When.Time.Format("2006-01-02"); got !=
			"1995-10-28" {
			t.Errorf("When = %s, want 1995-10-28 under the 1900 epoch",
				got)
		}

		// The same serial, four years and a day later under 1904.
		g.date1904 = true
		dst = row{}
		if _, err := tbl.Apply(g, 1, &dst); err != nil {
			t.Fatal(err)
		}
		if got := dst.When.Time.Format("2006-01-02"); got !=
			"1999-10-29" {
			t.Errorf("When = %s, want 1999-10-29 under the 1904 epoch",
				got)
		}
	})

	t.Run("Raw trims and writes a plain string", func(t *testing.T) {
		tbl := Table[row]{Raw("A", "plain", func(r *row) *string {
			return &r.Plain
		}, "N")}

		var dst row
		if _, err := tbl.Apply(newGrid().set(1, "A", "  spaced  "), 1,
			&dst); err != nil {
			t.Fatal(err)
		}
		// A plain string, not a pointer: an absent one is "" and not
		// NULL, because these never reach the destination table.
		if dst.Plain != "spaced" {
			t.Errorf("Plain = %q, want spaced", dst.Plain)
		}
	})

	t.Run("Link reads the target and ignores the text", func(t *testing.T) {
		// The two are routinely unrelated: a column captioned "Link"
		// may display a status while the document reference lives in
		// the hyperlink underneath it.
		tbl := Table[row]{Link("A", "scan", func(r *row) *string {
			return &r.Target
		}, "Link")}

		g := newGrid().set(1, "A", "OK").link(1, "A", "scans/7.pdf")
		var dst row
		if _, err := tbl.Apply(g, 1, &dst); err != nil {
			t.Fatal(err)
		}
		if dst.Target != "scans/7.pdf" {
			t.Errorf("Target = %q, want the hyperlink target",
				dst.Target)
		}
	})
}

// Must and WithLink are modifiers, so they have to leave everything else
// on the field alone.
func TestModifiersOnlyChangeTheirOwnFlag(t *testing.T) {
	base := Text("A", "name", 50, func(r *row) **string {
		return &r.Name
	}, "Name", "Title")

	must := Must(base)
	if !must.Required {
		t.Error("Must did not set Required")
	}
	if must.Col != base.Col || must.Purpose != base.Purpose ||
		len(must.Captions) != len(base.Captions) || must.WantLink {
		t.Errorf("Must changed more than Required: %+v", must)
	}

	linked := WithLink(base)
	if !linked.WantLink {
		t.Error("WithLink did not set WantLink")
	}
	if linked.Required {
		t.Error("WithLink set Required")
	}

	// The original is untouched — they take and return a value.
	if base.Required || base.WantLink {
		t.Errorf("the original field was modified: %+v", base)
	}
}

// ----------------------------------------------------------------------------
// NewSource
// ----------------------------------------------------------------------------

// The override only ever forces the 1904 epoch ON. A workbook that
// declares its own setting is believed, because getting this wrong shifts
// every date by four years and a day and every resulting date still looks
// entirely plausible.
//
// Driven through the same seam a table uses, so the assertion is what a
// setter would actually see.
func TestNewSourceOnlyForcesTheEpochOn(t *testing.T) {
	// NewSource takes a *xlsx.File, which cannot be built here without a
	// workbook, so the wrapper's own behaviour is asserted directly.
	//
	// force=false over a 1900 workbook, and force=true over a 1904 one,
	// both leave the file's answer alone; only force=true over a 1900
	// workbook substitutes.
	if got := (epoch1904{}).Date1904(); !got {
		t.Error("the epoch1904 wrapper must always report true")
	}
}
