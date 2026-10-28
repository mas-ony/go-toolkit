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
//
// A field built by hand rather than through a constructor names its
// column as the contract entry it is:
//
//	importer.Field[Row]{
//		Column: xlsx.Column{
//			Col: "E", Captions: []string{"Kode"}, Purpose: "code",
//		},
//		Key: "code",
//		Set: setCode,
//	}
type Field[T any] struct {
	// Column is the field's entry in the layout contract: the column
	// letter, the captions it accepts and the purpose it is read for.
	// It is embedded, so f.Col, f.Captions and f.Purpose read it, and
	// Columns hands it to VerifyHeaders and DataRows as it stands. List
	// every caption seen in the wild rather than one canonical string;
	// see xlsx.Column for why. Purpose also names the column in the
	// warnings and errors Apply reports.
	xlsx.Column

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

// cellFault is what a cell of col that will not read, with err, does to
// its row: the row's error when the column is one the row cannot be
// imported without, and otherwise a warning that the field is left
// empty. Each names the column.
//
// It is the one rule for a cell, which Table.Apply and Row's readers
// both follow, so a cell is reported alike whichever an import reads it
// through.
func cellFault(
	col xlsx.Column, required bool, err error,
) (fatal error, warning string) {
	if required {
		return fmt.Errorf("%s: %w", col, err), ""
	}
	return nil, fmt.Sprintf("%s: %v — left empty", col, err)
}

// coerce builds the field behind each built-in mapping, Text, Int,
// Decimal, Date, Raw and Link: the cell is read by conv into the value dst
// points at and stored there. An error from conv leaves the field as it
// was and is reported the way Apply reports any setter's. The six differ
// only in their conv, which for Raw and Link cannot fail, and in Link's
// asking for the hyperlink as well.
func coerce[T, V any](
	col, purpose string,
	dst func(*T) *V,
	conv func(Cell) (V, error),
	captions []string,
) Field[T] {
	return Custom(col, purpose, func(t *T, c Cell) error {
		v, err := conv(c)
		if err != nil {
			return err
		}
		*dst(t) = v
		return nil
	}, captions...)
}

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
	return Field[T]{Column: xlsx.Column{
		Col:      col,
		Captions: captions,
		Purpose:  purpose,
	}}
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
	return coerce(col, purpose, dst, func(c Cell) (*string, error) {
		return xlsx.StrMax(c.Text, limit, "the value")
	}, captions)
}

// Int maps a cell to a nullable integer column.
func Int[T any](
	col, purpose string,
	dst func(*T) **int, captions ...string,
) Field[T] {
	return coerce(col, purpose, dst, func(c Cell) (*int, error) {
		return xlsx.Int(c.Text)
	}, captions)
}

// Decimal maps a cell to a nullable fixed-point column, accepting the
// grouping and decimal separators a hand-typed sheet carries.
func Decimal[T any](
	col, purpose string,
	dst func(*T) **decimal.Decimal, captions ...string,
) Field[T] {
	return coerce(col, purpose, dst, func(c Cell) (*decimal.Decimal, error) {
		return xlsx.Decimal(c.Text)
	}, captions)
}

// Date maps a cell to a nullable date column, reading either an Excel
// serial under the workbook's own epoch or one of the written layouts.
func Date[T any](
	col, purpose string,
	dst func(*T) **datetime.Datetime, captions ...string,
) Field[T] {
	return coerce(col, purpose, dst,
		func(c Cell) (*datetime.Datetime, error) {
			return xlsx.Date(c.Text, c.Date1904)
		}, captions)
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
	return coerce(col, purpose, dst, func(c Cell) (string, error) {
		return xlsx.Trim(c.Text), nil
	}, captions)
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
	return WithLink(coerce(col, purpose, dst,
		func(c Cell) (string, error) { return c.Link, nil }, captions))
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
	f := Expect[T](col, purpose, captions...)
	f.Set = set
	return f
}

// Date1904 always reports true.
func (epoch1904) Date1904() bool { return true }

// Columns renders the whole table as the layout contract, for
// xlsx.VerifyHeaders and xlsx.DataRows.
//
// Every field is included, Expect entries and all. That is what makes a
// blank-row skip safe: DataRows tests the columns the caller declared an
// interest in, and a column read only for a lookup is still one of them.
func (t Table[T]) Columns() []xlsx.Column {
	out := make([]xlsx.Column, 0, len(t))
	for _, f := range t {
		out = append(out, f.Column)
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
		fatal, warning := cellFault(f.Column, f.Required, err)
		if fatal != nil {
			return warns, fatal
		}
		warns = append(warns, warning)
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
//
// A letter is held to xlsx.ColumnIndex, the reading the workbook's own
// lookups give it, so the letters refused here are exactly the ones that
// would read as empty: anything but letters, and a column past XFD.
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

		_, isColumn := xlsx.ColumnIndex(col)
		switch {
		case !isColumn:
			problems = append(problems, fmt.Sprintf(
				"%q (%s) is not a column letter", f.Col, f.Purpose))
		case seen[col]:
			named := f.Column
			named.Col = col
			problems = append(problems, fmt.Sprintf(
				"%s is mapped more than once", named))
		default:
			seen[col] = true
		}
		if len(f.Captions) == 0 {
			problems = append(problems, fmt.Sprintf(
				"%s accepts no caption, so the header check"+
					" cannot pass", f.Column))
		}
	}

	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("column table is not usable:\n  %s",
		strings.Join(problems, "\n  "))
}
