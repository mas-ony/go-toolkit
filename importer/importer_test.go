package importer

// Tests for importer.go: the counters, the outcome helpers, the report
// sort, and the registry.
//
// The registry is package state that Register only ever adds to, so every
// test touching it saves and restores both halves. Without that the
// duplicate-name panic would fire on a re-run under -count=2, and Only
// would answer about whatever some earlier test happened to register.

import (
	"context"
	"flag"
	"reflect"
	"strings"
	"testing"

	"github.com/mas-ony/go-toolkit/xlsx"
)

// stubDataset is the smallest thing satisfying Dataset. Only Info is read
// by the tests in this file; the rest exist to satisfy the interface, and
// panic rather than returning zero values so a test that reaches one by
// accident says so instead of quietly passing.
type stubDataset struct{ name string }

// withCleanRegistry empties the registry for one test and puts the
// original back afterwards.
func withCleanRegistry(t *testing.T) {
	t.Helper()
	oldRegistry, oldOrder := registry, order
	registry = map[string]func() Dataset{}
	order = nil
	t.Cleanup(func() { registry, order = oldRegistry, oldOrder })
}

// Info names the stub, which is all these tests read.
func (d stubDataset) Info() Info { return Info{Name: d.name} }

// Len is not reached by these tests; see stubDataset.
func (stubDataset) Len() int { panic("not used here") }

// Flags is not reached by these tests; see stubDataset.
func (stubDataset) Flags(*flag.FlagSet) { panic("not used here") }

// Validate is not reached by these tests; see stubDataset.
func (stubDataset) Validate() error { panic("not used here") }

// Layout is not reached by these tests; see stubDataset.
func (stubDataset) Layout() xlsx.Layout { panic("not used here") }

// Columns is not reached by these tests; see stubDataset.
func (stubDataset) Columns() []xlsx.Column { panic("not used here") }

// HeaderAdvice is not reached by these tests; see stubDataset.
func (stubDataset) HeaderAdvice(error) error { panic("not used here") }

// Connect is not reached by these tests; see stubDataset.
func (stubDataset) Connect(context.Context, Transport, *Run) error {
	panic("not used here")
}

// Prepare is not reached by these tests; see stubDataset.
func (stubDataset) Prepare(
	context.Context, *xlsx.File, *Run,
) ([]Outcome, error) {
	panic("not used here")
}

// Import is not reached by these tests; see stubDataset.
func (stubDataset) Import(context.Context, int, *Run) Outcome {
	panic("not used here")
}

// Report is not reached by these tests; see stubDataset.
func (stubDataset) Report() ReportShape { panic("not used here") }

// The zero value has to work: Run holds a *Counters a caller builds with
// &Counters{}, and the maps behind the named tallies are created lazily.
func TestCountersZeroValueIsUsable(t *testing.T) {
	var c Counters

	if got := c.Value("never touched"); got != 0 {
		t.Errorf("Value on an untouched counter = %d, want 0", got)
	}
	if got := c.Extras(); len(got) != 0 {
		t.Errorf("Extras = %v, want none", got)
	}

	c.Inc("rows")
	if got := c.Value("rows"); got != 1 {
		t.Errorf("after Inc, Value = %d, want 1", got)
	}
}

// Extras reports in FIRST-USE order, not map order, because the summary
// prints them in that order and a block of counters that reshuffles
// between runs is hard to compare against the last one.
func TestExtrasKeepFirstUseOrder(t *testing.T) {
	var c Counters
	c.Inc("zebra")
	c.Add("apple", 3)
	c.Inc("mango")
	c.Inc("zebra") // a repeat must not move it

	var names []string
	for _, e := range c.Extras() {
		names = append(names, e.Name)
	}
	want := []string{"zebra", "apple", "mango"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("Extras order = %v, want %v", names, want)
	}
	if got := c.Value("zebra"); got != 2 {
		t.Errorf("zebra = %d, want 2", got)
	}
	if got := c.Value("apple"); got != 3 {
		t.Errorf("apple = %d, want 3", got)
	}
}

// Declare is what makes a zero worth printing. The distinction is the
// whole point of the Always flag: "files uploaded 0" says the scans were
// not found, where a missing line says nothing at all.
func TestDeclareMarksCountersAsAlwaysPrinted(t *testing.T) {
	var c Counters
	c.Declare("uploaded", "matched")
	c.Inc("incidental")

	got := map[string]Counter{}
	for _, e := range c.Extras() {
		got[e.Name] = e
	}

	if !got["uploaded"].Always {
		t.Error("a declared counter should print at zero")
	}
	if got["uploaded"].Value != 0 {
		t.Errorf("uploaded = %d, want 0", got["uploaded"].Value)
	}
	if got["incidental"].Always {
		t.Error("a counter that arrived through Inc should not print " +
			"at zero")
	}
}

// Declare also pins ORDER, which is why it is called before the run
// rather than wherever the first Inc happens to land.
func TestDeclarePinsOrderAheadOfFirstUse(t *testing.T) {
	var c Counters
	c.Declare("second", "third")
	c.Inc("fourth")
	c.Inc("second") // already declared; must not move to the end

	var names []string
	for _, e := range c.Extras() {
		names = append(names, e.Name)
	}
	want := []string{"second", "third", "fourth"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("order = %v, want %v", names, want)
	}
}

// Note formats and appends, and Set creates Fields on first use, the last
// write for a field winning.
func TestOutcomeNoteAndSet(t *testing.T) {
	var o Outcome

	o.Note("column %s: %s", "C", "not a number")
	o.Note("plain")
	if want := []string{"column C: not a number", "plain"}; !reflect.
		DeepEqual(o.Notes, want) {
		t.Errorf("Notes = %v, want %v", o.Notes, want)
	}

	// Set creates the map lazily, so the zero Outcome is usable.
	o.Set("unit", "7")
	o.Set("unit", "8") // last write wins
	o.Set("area", "12")
	if got := o.Fields["unit"]; got != "8" {
		t.Errorf("unit = %q, want 8", got)
	}
	if got := o.Fields["area"]; got != "12" {
		t.Errorf("area = %q, want 12", got)
	}
}

// Failed is what decides the exit status, so it must answer for exactly
// one status and not for the others.
func TestOutcomeFailedIsOnlyTheFailedStatus(t *testing.T) {
	for _, s := range []Status{
		StatusCreated, StatusUpdated, StatusSkipped, StatusDryRun, "",
	} {
		if (Outcome{Status: s}).Failed() {
			t.Errorf("status %q reports as failed", s)
		}
	}
	if !(Outcome{Status: StatusFailed}).Failed() {
		t.Error("StatusFailed does not report as failed")
	}
}

// The report is ordered by workbook row so a reviewer can walk it against
// the sheet. The input is not arbitrary: Prepare's parse failures come
// first, already ordered among themselves, followed by the imported rows,
// also ordered. That shape — two sorted runs concatenated — is what the
// insertion sort is chosen for, and it is the case worth testing.
func TestSortOutcomesMergesTwoSortedRuns(t *testing.T) {
	out := []Outcome{
		// Prepare's failures, ascending.
		{ExcelRow: 5, Status: StatusFailed},
		{ExcelRow: 11, Status: StatusFailed},
		{ExcelRow: 40, Status: StatusFailed},
		// The imported rows, ascending, interleaving with the above.
		{ExcelRow: 6, Status: StatusCreated},
		{ExcelRow: 7, Status: StatusCreated},
		{ExcelRow: 20, Status: StatusCreated},
		{ExcelRow: 41, Status: StatusCreated},
	}
	sortOutcomes(out)

	var rows []int
	for _, o := range out {
		rows = append(rows, o.ExcelRow)
	}
	want := []int{5, 6, 7, 11, 20, 40, 41}
	if !reflect.DeepEqual(rows, want) {
		t.Errorf("rows = %v, want %v", rows, want)
	}
}

// sortOutcomes handles an empty and a one-row report, and keeps rows that
// share a number in their original order.
func TestSortOutcomesHandlesEdgeCases(t *testing.T) {
	sortOutcomes(nil)                      // must not panic
	sortOutcomes([]Outcome{{ExcelRow: 1}}) // must not panic

	// Equal row numbers keep their relative order, which matters because
	// a row can produce a failure note and an outcome.
	out := []Outcome{
		{ExcelRow: 3, Label: "first"},
		{ExcelRow: 3, Label: "second"},
		{ExcelRow: 1, Label: "third"},
	}
	sortOutcomes(out)
	if out[0].Label != "third" {
		t.Errorf("out[0] = %q, want third", out[0].Label)
	}
	if out[1].Label != "first" || out[2].Label != "second" {
		t.Errorf("equal rows reordered: %q then %q",
			out[1].Label, out[2].Label)
	}
}

// Register keeps registration order, Lookup builds what was registered,
// and an unknown name reports false.
func TestRegisterLookupAndNames(t *testing.T) {
	withCleanRegistry(t)

	Register("beta", func() Dataset { return stubDataset{name: "beta"} })
	Register("alpha", func() Dataset { return stubDataset{name: "alpha"} })

	// Registration order, not alphabetical: the list output reads better
	// grouped by when things were added.
	if want := []string{"beta", "alpha"}; !reflect.DeepEqual(
		Names(), want) {
		t.Errorf("Names = %v, want %v", Names(), want)
	}

	ds, ok := Lookup("alpha")
	if !ok {
		t.Fatal("Lookup(alpha) = false")
	}
	if got := ds.Info().Name; got != "alpha" {
		t.Errorf("Info().Name = %q, want alpha", got)
	}

	if _, ok := Lookup("absent"); ok {
		t.Error("Lookup(absent) = true")
	}
}

// Names hands back a copy. A caller that sorted the result in place would
// otherwise reorder the registry itself, and the next -dataset list would
// disagree with the one before it.
func TestNamesReturnsACopy(t *testing.T) {
	withCleanRegistry(t)
	Register("one", func() Dataset { return stubDataset{name: "one"} })
	Register("two", func() Dataset { return stubDataset{name: "two"} })

	got := Names()
	got[0] = "clobbered"

	if Names()[0] != "one" {
		t.Error("mutating the returned slice changed the registry")
	}
}

// Each Lookup constructs a FRESH dataset, which is what lets a dataset
// hold per-run state — a parsed row slice, a lookup cache — without two
// runs sharing it.
func TestLookupConstructsAFreshDataset(t *testing.T) {
	withCleanRegistry(t)

	calls := 0
	Register("counting", func() Dataset {
		calls++
		return stubDataset{name: "counting"}
	})

	if calls != 0 {
		t.Errorf("the constructor ran %d times at registration", calls)
	}
	_, _ = Lookup("counting")
	_, _ = Lookup("counting")
	if calls != 2 {
		t.Errorf("the constructor ran %d times for two lookups, want 2",
			calls)
	}
}

// A duplicate name panics rather than replacing, because the alternative
// is one of the two datasets silently never being reachable.
func TestRegisterPanicsOnADuplicateName(t *testing.T) {
	withCleanRegistry(t)
	Register("same", func() Dataset { return stubDataset{name: "same"} })

	defer func() {
		p := recover()
		if p == nil {
			t.Fatal("a duplicate registration did not panic")
		}
		if msg, _ := p.(string); !strings.Contains(msg, "same") {
			t.Errorf("panic %v does not name the dataset", p)
		}
	}()
	Register("same", func() Dataset { return stubDataset{name: "same"} })
}

// Only is what lets a command with a single dataset accept no -dataset at
// all, so it has to answer false for none and for more than one.
func TestOnly(t *testing.T) {
	withCleanRegistry(t)

	if _, ok := Only(); ok {
		t.Error("Only = true with nothing registered")
	}

	Register("solo", func() Dataset { return stubDataset{name: "solo"} })
	name, ok := Only()
	if !ok || name != "solo" {
		t.Errorf("Only = (%q, %v), want (solo, true)", name, ok)
	}

	Register("second", func() Dataset { return stubDataset{name: "x"} })
	if _, ok := Only(); ok {
		t.Error("Only = true with two datasets registered")
	}
}
