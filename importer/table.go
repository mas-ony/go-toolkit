package importer

import (
	"fmt"
	"strings"

	"github.com/mas-ony/go-toolkit/datetime"
	"github.com/mas-ony/go-toolkit/xlsx"
	"github.com/shopspring/decimal"
)

// epoch1904 is a workbook read as though it declared the 1904 epoch.
type epoch1904 struct{ *xlsx.File }

// Cell is one mapped cell as a setter sees it.
//
// Link and Date1904 are carried alongside the text because the two
// coercions that need them cannot get them any other way: a hyperlink
// lives outside the cell's value, and an Excel serial means a different
// day under each of the two epochs. Passing both to every setter costs
// two words and removes the reason for a setter to reach back at the
// file.
type Cell struct {
	Row      int
	Col      string
	Text     string
	Link     string
	Date1904 bool
}

// Source is the part of a workbook a Table reads. It is an interface
// rather than *xlsx.File so a table can be tested against a grid held
// in the test, without a file on disk.
//
// NewSource adapts an open workbook to it.
type Source interface {
	Cell(row int, col string) string
	Hyperlink(row int, col string) string
	Date1904() bool
}

// Table is a dataset's full column mapping: the letter, the accepted
// captions and the coercion for every column it reads, in sheet order so
// problems are reported left to right rather than in map-iteration order.
//
// It yields the two things an import needs from a spreadsheet — the
// layout contract the header check runs against, through Columns, and the
// code that fills one row's worth of a domain type, through Apply.
//
// # Why a table rather than a parse function
//
// A parser written by hand states every column three times: once as a
// letter constant, once in the caption contract, and once in the parse
// function. The three drift. A column moved in the contract but not in
// the parser passes the header check and then reads the wrong cell, which
// is precisely the failure the header check exists to catch. Here the
// letter, the captions and the coercion are one entry, so a column moves
// in a single edit or not at all.
type Table[T any] []Field[T]

// Field is one column of the layout contract plus the rule for what its
// cell becomes.
//
// Fields cover the columns that map straight onto the destination type. A
// column whose meaning depends on the rest of the row — a date taken from
// a neighbour when the cell holds a word, a value assembled from two
// columns — is the dataset's own work, done after Apply returns. Custom
// is for the cases in between: one column, one rule, too specific to
// name.
type Field[T any] struct {
	// Col is the column letter: "C", or "AB".
	Col string

	// Captions are the spellings this column accepts, passed through to
	// xlsx.Column. List every spelling seen in the wild rather than
	// one canonical string; see that type for why.
	Captions []string

	// Purpose is a short phrase naming what the column is read for. It
	// appears in header-check failures and in warnings.
	Purpose string

	// Required makes a coercion failure fatal to the row rather than a
	// warning. Set it through Must.
	Required bool

	// Key names the value this column writes. No constructor in this
	// package sets it: a field that writes into a typed struct has its
	// destination in an accessor, where there is no name to record. A
	// dataset whose fields write into a map or a keyed record sets Key
	// itself, on the struct literal or through constructors of its own.
	//
	// Nothing reads it at run time. It exists so Validate can see what
	// a closure would otherwise hide, and catch two columns writing the
	// same value — where the second silently wins and the first column
	// is read for nothing.
	Key string

	// WantLink asks for the cell's hyperlink target to be resolved and
	// carried on the Cell. Set it through WithLink.
	//
	// It is off by default because resolving one is two lookups
	// through the sheet's relationships, and on a wide sheet of a few
	// thousand rows that is tens of thousands of lookups bought for
	// nothing. Only the columns that carry a document reference need
	// it.
	WantLink bool

	// Set writes the cell onto the destination. A nil Set means the
	// column is in the contract but read for nothing; see Expect.
	Set func(dst *T, c Cell) error
}

// isColumnLetter reports whether s is a spreadsheet column name: one to
// three ASCII letters and nothing else.
func isColumnLetter(s string) bool {
	if s == "" || len(s) > 3 {
		return false
	}
	for _, r := range s {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return true
}

// Date1904 always reports true.
func (epoch1904) Date1904() bool { return true }

// NewSource adapts an open workbook to the Source a Table reads,
// honouring the -date1904 override.
//
// The override only ever forces the 1904 epoch ON. A workbook that
// declares its own setting is believed, because getting this wrong shifts
// every date by four years and a day and every resulting date still looks
// entirely plausible.
func NewSource(wb *xlsx.File, force bool) Source {
	if force && !wb.Date1904() {
		return epoch1904{wb}
	}
	return wb
}

// Column renders the field as its entry in the layout contract: the
// xlsx.Column that VerifyHeaders and DataRows read.
func (f Field[T]) Column() xlsx.Column {
	return xlsx.Column{
		Col:      f.Col,
		Captions: f.Captions,
		Purpose:  f.Purpose,
	}
}

// Columns renders the whole table as the layout contract, for
// xlsx.VerifyHeaders and xlsx.DataRows.
//
// Every field is included, Expect entries and all. That is what makes a
// blank-row skip safe: DataRows tests the columns the caller declared an
// interest in, and a column read only for a lookup is still one of them.
func (t Table[T]) Columns() []xlsx.Column {
	out := make([]xlsx.Column, 0, len(t))
	for _, f := range t {
		out = append(out, f.Column())
	}
	return out
}

// Apply reads one row into dst, returning the warnings it collected.
//
// A cell that will not coerce is a warning, not a failed row: the field
// is left unset and the row goes on. A record whose one numeric cell
// holds a stray note is still worth importing with its other twenty
// columns intact, and the warning names the cell somebody has to go and
// fix. Must inverts that for a column a row genuinely cannot be imported
// without.
//
// The destination is supplied rather than allocated here because a row
// type that holds a pointer to its model must have that pointer set
// before any setter runs, and only the dataset knows what it needs to
// build.
//
// A non-nil error means a Required column would not coerce. dst is left
// partly filled in that case: the caller is dropping the row, and
// rolling back the fields already written would buy nothing.
func (t Table[T]) Apply(
	src Source, row int, dst *T,
) ([]string, error) {
	var warns []string

	date1904 := src.Date1904()
	for _, f := range t {
		if f.Set == nil {
			continue
		}
		c := Cell{
			Row:      row,
			Col:      f.Col,
			Text:     src.Cell(row, f.Col),
			Date1904: date1904,
		}
		if f.WantLink {
			c.Link = src.Hyperlink(row, f.Col)
		}

		err := f.Set(dst, c)
		if err == nil {
			continue
		}
		if f.Required {
			return warns, fmt.Errorf("column %s (%s): %w",
				f.Col, f.Purpose, err)
		}
		warns = append(warns, fmt.Sprintf(
			"column %s (%s): %v — left empty",
			f.Col, f.Purpose, err))
	}
	return warns, nil
}

// Validate checks the table itself: a column letter that is not one, a
// letter mapped twice, a field with no accepted caption, and — among the
// fields whose Key is set — two columns writing the same value.
//
// Worth calling once at start-up. Every one of these is a typo in a
// table nobody reads end to end, and each fails in a way that looks like
// a problem with the workbook rather than with the code: a bad letter
// reads as an empty cell on every row, a duplicate letter reports one
// column twice in a header failure while the second setter quietly
// overwrites the first, and a duplicate key loses a whole column's worth
// of data with nothing to show for it.
func (t Table[T]) Validate() error {
	var problems []string

	seen := make(map[string]bool, len(t))
	wrote := make(map[string]string, len(t))
	for _, f := range t {
		// Upper-cased, because the cell lookup is case-insensitive, so "c"
		// and "C" name one column and have to collide here. NOT trimmed,
		// because the lookup trims nothing: " C" reads as an empty cell on
		// every row and fails the header check, the kind of typo this
		// method exists to catch before the workbook is blamed for it.
		col := strings.ToUpper(f.Col)

		if f.Key != "" {
			if first, dup := wrote[f.Key]; dup {
				problems = append(problems, fmt.Sprintf(
					"columns %s and %s both write %q; the second"+
						" wins and the first is read for nothing",
					first, col, f.Key))
			} else {
				wrote[f.Key] = col
			}
		}

		switch {
		case !isColumnLetter(col):
			problems = append(problems, fmt.Sprintf(
				"%q (%s) is not a column letter", f.Col, f.Purpose))
		case seen[col]:
			problems = append(problems, fmt.Sprintf(
				"column %s (%s) is mapped more than once",
				col, f.Purpose))
		default:
			seen[col] = true
		}
		if len(f.Captions) == 0 {
			problems = append(problems, fmt.Sprintf(
				"column %s (%s) accepts no caption, so the header"+
					" check cannot pass", f.Col, f.Purpose))
		}
	}

	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("column table is not usable:\n  %s",
		strings.Join(problems, "\n  "))
}

// Must marks a field as one the row cannot be imported without, turning
// a coercion failure from a warning into the row's error.
//
// It reads at the call site as the exception it should be:
//
//	importer.Must(importer.Text("B", "reference", 32, ...)),
func Must[T any](f Field[T]) Field[T] {
	f.Required = true
	return f
}

// Expect declares a column that is checked but not read.
//
// Two uses. A column the importer genuinely ignores can still be worth
// pinning, because its caption is evidence the sheet has not been
// restructured. And a column read by hand after Apply — one whose value
// depends on the rest of the row — belongs in the contract even though
// no setter touches it.
//
// Pin sparingly. Every declared column is one more caption that has to
// keep matching, and the groups an importer ignores tend to be the ones
// whose captions move most.
func Expect[T any](col, purpose string, captions ...string) Field[T] {
	return Field[T]{
		Col:      col,
		Captions: captions,
		Purpose:  purpose,
	}
}

// WithLink asks for the cell's hyperlink to be resolved and handed to the
// setter, for a Custom rule that reads both the text and the target:
//
//	importer.WithLink(importer.Custom("D", "scan", setScan, "File")),
func WithLink[T any](f Field[T]) Field[T] {
	f.WantLink = true
	return f
}

// Text maps a cell to a nullable string column, capped at the width the
// destination column holds. An over-long value is an error rather than a
// truncation, so the cell gets looked at instead of being silently
// shortened by the driver.
//
// dst returns the address OF the pointer field, which is what lets an
// empty cell stay NULL rather than become "".
func Text[T any](
	col, purpose string, limit int,
	dst func(*T) **string, captions ...string,
) Field[T] {
	return Field[T]{
		Col:      col,
		Captions: captions,
		Purpose:  purpose,
		Set: func(t *T, c Cell) error {
			v, err := xlsx.StrMax(c.Text, limit, "the value")
			if err != nil {
				return err
			}
			*dst(t) = v
			return nil
		},
	}
}

// Int maps a cell to a nullable integer column.
func Int[T any](
	col, purpose string,
	dst func(*T) **int, captions ...string,
) Field[T] {
	return Field[T]{
		Col:      col,
		Captions: captions,
		Purpose:  purpose,
		Set: func(t *T, c Cell) error {
			v, err := xlsx.Int(c.Text)
			if err != nil {
				return err
			}
			*dst(t) = v
			return nil
		},
	}
}

// Decimal maps a cell to a nullable fixed-point column, accepting the
// grouping and decimal separators a hand-typed sheet carries.
func Decimal[T any](
	col, purpose string,
	dst func(*T) **decimal.Decimal, captions ...string,
) Field[T] {
	return Field[T]{
		Col:      col,
		Captions: captions,
		Purpose:  purpose,
		Set: func(t *T, c Cell) error {
			v, err := xlsx.Decimal(c.Text)
			if err != nil {
				return err
			}
			*dst(t) = v
			return nil
		},
	}
}

// Date maps a cell to a nullable date column, reading either an Excel
// serial under the workbook's own epoch or one of the written layouts.
func Date[T any](
	col, purpose string,
	dst func(*T) **datetime.Datetime, captions ...string,
) Field[T] {
	return Field[T]{
		Col:      col,
		Captions: captions,
		Purpose:  purpose,
		Set: func(t *T, c Cell) error {
			v, err := xlsx.Date(c.Text, c.Date1904)
			if err != nil {
				return err
			}
			*dst(t) = v
			return nil
		},
	}
}

// Raw maps a cell to a plain string field, trimmed, empty when the cell
// is.
//
// It is for the values that never reach the destination table: a place
// name read only to resolve an id, a code resolved against a lookup
// endpoint. Those live on the dataset's row type, not on its model, and
// they are strings rather than pointers because an absent one is "" and
// not NULL.
func Raw[T any](
	col, purpose string,
	dst func(*T) *string, captions ...string,
) Field[T] {
	return Field[T]{
		Col:      col,
		Captions: captions,
		Purpose:  purpose,
		Set: func(t *T, c Cell) error {
			*dst(t) = xlsx.Trim(c.Text)
			return nil
		},
	}
}

// Link maps the hyperlink target behind a cell, ignoring the cell's own
// text.
//
// The two are routinely unrelated. A column captioned "Link" may display
// a status code while the reference to the document lives in the
// hyperlink underneath it, and an importer that reads the text as a
// filename looks for the same file on every row of the sheet. A dataset
// that needs both takes Custom and writes both.
func Link[T any](
	col, purpose string,
	dst func(*T) *string, captions ...string,
) Field[T] {
	return Field[T]{
		Col:      col,
		Captions: captions,
		Purpose:  purpose,
		WantLink: true,
		Set: func(t *T, c Cell) error {
			*dst(t) = c.Link
			return nil
		},
	}
}

// Custom maps a cell by a rule of the caller's own: two fields from one
// column, a code matched against a set, a unit stripped before parsing.
//
// The setter gets the whole Cell, and its error is treated exactly as a
// built-in coercion's is — a warning, or the row's error under Must. Wrap
// the field in WithLink when the rule needs the hyperlink as well as the
// text.
func Custom[T any](
	col, purpose string,
	set func(dst *T, c Cell) error, captions ...string,
) Field[T] {
	return Field[T]{
		Col:      col,
		Captions: captions,
		Purpose:  purpose,
		Set:      set,
	}
}
