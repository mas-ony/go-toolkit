package importer

import (
	"context"
	"flag"
	"fmt"
	"time"

	"github.com/rs/zerolog"

	"github.com/mas-ony/go-toolkit/config"
	"github.com/mas-ony/go-toolkit/xlsx"
)

const (
	// StatusCreated — a new row was written.
	StatusCreated Status = "created"

	// StatusUpdated — an existing row was matched and rewritten. Only a
	// dataset with a natural key can produce this.
	StatusUpdated Status = "updated"

	// StatusSkipped — the row was recognised and deliberately not
	// written, e.g. already present and unchanged.
	StatusSkipped Status = "skipped"

	// StatusFailed — the row was not written and needs attention.
	StatusFailed Status = "failed"

	// StatusDryRun — nothing was written because -dry-run was given.
	StatusDryRun Status = "dry-run"
)

// Status is what became of one workbook row. The values are written into
// the report as-is, so they are the vocabulary an operator reads.
type Status string

// Counter is one named tally, rendered in the summary.
type Counter struct {
	Name  string
	Value int

	// Always prints the line even at zero, for a counter whose zero is
	// itself worth seeing.
	Always bool
}

// Counters tallies a run. The five fixed fields are the outcomes every
// dataset has; anything else a dataset wants to count is named.
type Counters struct {
	Parsed  int
	Created int
	Updated int
	Skipped int
	Failed  int

	names  []string
	values map[string]int
	always map[string]bool
}

// Options is what the generic flags supply.
type Options struct {
	// Layout is the resolved sheet geometry: the dataset's defaults with
	// any flag override applied. Execute fills in Sheet once the
	// workbook has resolved it.
	Layout xlsx.Layout

	// Date1904 forces the 1904 epoch on for a workbook that has lost the
	// property. It never forces it off; the file's own setting wins
	// whenever it is present.
	Date1904 bool

	// SkipHeader imports even when the header check fails.
	SkipHeader bool

	// DryRun parses, resolves and reports without writing.
	DryRun bool

	// Strict promotes a dataset's soft resolution failures into row
	// failures. What counts as one is the dataset's judgement.
	Strict bool

	// Verbose logs every row rather than only the problem ones.
	Verbose bool

	// Limit stops after that many importable rows. Zero means all of
	// them.
	Limit int
}

// Transport is what the connection flags supply: the same
// fiber.client.* section a service loads from YAML, plus the two
// deadlines a client does not set for itself.
//
// A dataset that writes over HTTP builds its client from this —
// httpclient.New(&t.ClientConfig, httpclient.Options{...}) — and one that
// writes anywhere else ignores it. Embedding the config type rather than
// a shape of this package's own is what lets a flag-built transport and a
// YAML-built one be the same thing.
type Transport struct {
	config.ClientConfig

	// Timeout bounds a metadata call.
	Timeout time.Duration

	// UploadTimeout bounds a request carrying a file, which crosses an
	// office network with megabytes on it and deserves longer.
	UploadTimeout time.Duration
}

// Info names a dataset for the operator.
type Info struct {
	// Name is the value of -dataset that selects it.
	Name string

	// Describe is the one-line summary shown by -dataset list.
	Describe string

	// Caveat is a warning printed on the summary banner, most usefully
	// that a re-run duplicates. Empty for a dataset that can match a
	// stored row and is therefore safe to run twice.
	Caveat string
}

// Run is the state one import shares with its dataset.
type Run struct {
	Log   zerolog.Logger
	Opt   Options
	Count *Counters
}

// Outcome records what happened to one workbook row.
//
// ExcelRow is the reliable anchor: it points at a cell somebody can open.
// Label is a convenience for whoever reads the report and is expected to
// be weak — not unique, and empty on a row whose identifying column would
// not parse.
type Outcome struct {
	ExcelRow int
	Label    string
	Status   Status
	ID       string
	Notes    []string

	// Fields carries the dataset's own report columns, keyed by the
	// names in ReportShape.Fields. A key with no value is written as an
	// empty cell rather than omitted, so every line has the same shape.
	Fields map[string]string
}

// ReportShape describes the dataset's columns in the CSV report.
type ReportShape struct {
	// Label heads the Outcome.Label column.
	Label string

	// ID heads the Outcome.ID column.
	ID string

	// Fields are the dataset's own columns, in the order they are
	// written, matching the keys it sets through Outcome.Set.
	Fields []string
}

// Dataset is one workbook shape and where its rows go.
//
// The methods are called in the order they are listed, once each per run
// except Import. A dataset may hold state across them — a parsed row
// slice, a lookup cache, an index of a scan directory — because exactly
// one run uses one instance.
type Dataset interface {
	// Info names the dataset and states whether a re-run is safe.
	Info() Info

	// Flags registers the dataset's own flags. It is called before
	// parsing, on the same set as the generic flags, so a name that
	// collides with one of those panics at start-up rather than being
	// silently shadowed.
	Flags(fs *flag.FlagSet)

	// Validate checks those flags after parsing. Returning an error
	// exits with the usage status, so it is the place to refuse a
	// combination that cannot be honoured rather than approximating it.
	Validate() error

	// Layout is the dataset's default geometry, before any flag
	// override.
	Layout() xlsx.Layout

	// Columns is the layout contract the header check runs against.
	Columns() []xlsx.Column

	// HeaderAdvice wraps a failed header check with the advice only the
	// dataset can give: where its column table lives, and what to do
	// about a workbook that really has been restructured.
	HeaderAdvice(err error) error

	// Connect opens whatever the dataset writes to and proves it is
	// reachable. It runs before the workbook is parsed, so a wrong URL
	// or a rejected token fails in a second rather than after a
	// three-thousand-row parse.
	Connect(ctx context.Context, t Transport, run *Run) error

	// Prepare parses the sheet and does any whole-run work: batch
	// lookups, indexes, caches. The outcomes it returns are the rows
	// that could not be parsed at all; it is responsible for counting
	// them.
	Prepare(
		ctx context.Context, wb *xlsx.File, run *Run,
	) ([]Outcome, error)

	// Len is the number of importable rows Prepare found.
	Len() int

	// Import handles row i, writing unless run.Opt.DryRun, and returns
	// the row's outcome.
	//
	// Every counter is the dataset's to move, the fixed ones included:
	// only it knows whether a row that landed but whose follow-up call
	// did not is Created or Failed, and a driver that guessed would be
	// wrong on half the datasets.
	Import(ctx context.Context, i int, run *Run) Outcome

	// Report describes the dataset's columns in the CSV.
	Report() ReportShape
}

// registry holds every dataset the command can run, keyed by the value of
// -dataset that selects it.
//
// A dataset registers itself from an init in its own file, so adding one
// is a single new file and no edit to main.
var registry = map[string]func() Dataset{}

// order preserves registration order for the -dataset list output, which
// reads better grouped by when things were added than alphabetically.
var order []string

// touch registers a name, keeping first-use order.
func (c *Counters) touch(name string) {
	if c.values == nil {
		c.values = make(map[string]int, 8)
		c.always = make(map[string]bool, 8)
	}
	if _, seen := c.values[name]; !seen {
		c.values[name] = 0
		c.names = append(c.names, name)
	}
}

// sortOutcomes orders the report by workbook row. Insertion sort because
// the slice is already almost ordered — only the parse failures, which
// Prepare returns up front, are out of place.
func sortOutcomes(out []Outcome) {
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].ExcelRow < out[j-1].ExcelRow; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
}

// Note appends a line to the row's notes.
func (o *Outcome) Note(format string, args ...any) {
	o.Notes = append(o.Notes, fmt.Sprintf(format, args...))
}

// Set records one of the dataset's report fields.
func (o *Outcome) Set(field, value string) {
	if o.Fields == nil {
		o.Fields = make(map[string]string, 4)
	}
	o.Fields[field] = value
}

// Failed reports whether the row needs attention, which is what the
// summary lists and what decides the exit status.
func (o Outcome) Failed() bool { return o.Status == StatusFailed }

// Declare pins the order of a dataset's counters and marks them as
// printed even at zero. Call it once, before the run, for the counters
// whose absence is information: "files uploaded 0" says the scans were
// not found, where a missing line says nothing at all.
func (c *Counters) Declare(names ...string) {
	for _, n := range names {
		c.touch(n)
		c.always[n] = true
	}
}

// Lookup constructs the named dataset.
func Lookup(name string) (Dataset, bool) {
	newDataset, ok := registry[name]
	if !ok {
		return nil, false
	}
	return newDataset(), true
}

// Names lists the registered datasets in registration order.
func Names() []string {
	out := make([]string, len(order))
	copy(out, order)
	return out
}

// Value reads a named counter.
func (c *Counters) Value(name string) int { return c.values[name] }

// Add adds n to a named counter.
func (c *Counters) Add(name string, n int) {
	c.touch(name)
	c.values[name] += n
}

// Inc adds one to a named counter, declaring it on first use. A counter
// that arrives this way is printed only when it is non-zero.
func (c *Counters) Inc(name string) { c.Add(name, 1) }

// Extras returns the named counters in declaration order.
func (c *Counters) Extras() []Counter {
	out := make([]Counter, 0, len(c.names))
	for _, n := range c.names {
		out = append(out, Counter{
			Name:   n,
			Value:  c.values[n],
			Always: c.always[n],
		})
	}
	return out
}

// Only returns the single registered dataset's name, when there is
// exactly one.
//
// It is what lets a command with a single dataset accept no -dataset
// at all, so a one-dataset importer keeps the command line it would
// have had without a registry behind it.
func Only() (string, bool) {
	if len(order) != 1 {
		return "", false
	}
	return order[0], true
}

// Register adds a dataset under the given name. A duplicate name panics:
// it is a programming error, and the alternative is one of the two
// datasets silently never being reachable.
func Register(name string, newDataset func() Dataset) {
	if _, dup := registry[name]; dup {
		panic("importer: dataset " + name + " registered twice")
	}
	registry[name] = newDataset
	order = append(order, name)
}

// Execute runs one import: open, check, parse, then one row at a time.
//
// The header check comes before any parsing because everything
// downstream of a moved column is confidently wrong rather than visibly
// broken. It is the only gate; a dataset that wants to import anyway
// gets -skip-header-check, which warns and continues.
func Execute(
	ctx context.Context,
	ds Dataset,
	path string,
	run *Run,
) ([]Outcome, error) {
	wb, err := xlsx.Open(path, run.Opt.Layout)
	if err != nil {
		return nil, err
	}
	defer func() { _ = wb.Close() }()

	run.Opt.Layout.Sheet = wb.SheetName()

	if err := wb.VerifyHeaders(ds.Columns()); err != nil {
		if !run.Opt.SkipHeader {
			return nil, ds.HeaderAdvice(err)
		}
		run.Log.Warn().Err(err).Msg(
			"sheet layout does not match — importing anyway " +
				"(-skip-header-check)")
	}

	if wb.Date1904() {
		run.Log.Info().Msg("the workbook uses the 1904 date system")
	} else if run.Opt.Date1904 {
		run.Log.Warn().Msg(
			"reading dates under the 1904 epoch (-date1904); the " +
				"workbook itself does not say so")
	}

	outcomes, err := ds.Prepare(ctx, wb, run)
	if err != nil {
		return nil, err
	}

	n := ds.Len()
	run.Count.Parsed = n
	if lim := run.Opt.Limit; lim > 0 && lim < n {
		run.Log.Warn().Int("limit", lim).Int("parsed", n).
			Msg("stopping early (-limit)")
		n = lim
	}

	for i := 0; i < n; i++ {
		if ctx.Err() != nil {
			run.Log.Warn().Int("row", i+1).
				Msg("interrupted — stopping")
			break
		}
		outcomes = append(outcomes, ds.Import(ctx, i, run))
	}

	sortOutcomes(outcomes)
	return outcomes, nil
}
