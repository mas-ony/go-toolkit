package sqltest

// Tests for sqltest.go: a fake assembled from the parts here answers a
// query through sqlx, Rows replays its rows and then ends, the parts a
// fake may not use refuse or do nothing, Log keeps what a connection is
// handed, the pool Open returns is bound as the name it was given and
// closed when the test ends, and Selected spells a list the way the
// database package does.

import (
	"context"
	"database/sql/driver"
	"io"
	"reflect"
	"testing"

	"github.com/jmoiron/sqlx"
)

// connector is the smallest fake the parts here make: every connection it
// opens answers every query with the same canned rows.
type connector struct {
	ConnectorBase
	rows Rows
}

// conn is a connection connector opens.
type conn struct {
	ConnBase
	rows Rows
}

// Connect opens a connection that answers with c's rows.
func (c connector) Connect(context.Context) (driver.Conn, error) {
	return conn{rows: c.rows}, nil
}

// QueryContext answers with a fresh copy of the canned rows.
func (c conn) QueryContext(
	context.Context, string, []driver.NamedValue,
) (driver.Rows, error) {
	r := c.rows
	return &r, nil
}

// A fake built from the parts here is opened under the name it is given
// and answers through sqlx with the rows it holds, in order.
func TestAFakeAnswersThroughSqlx(t *testing.T) {
	t.Parallel()
	db := Open(t, connector{rows: Rows{
		Cols:   []string{"id", "code"},
		Values: [][]driver.Value{{int64(1), "a"}, {int64(2), "b"}},
	}}, "mysql")
	if got := db.DriverName(); got != "mysql" {
		t.Errorf("DriverName = %q, want mysql", got)
	}

	var got []struct {
		ID   int    `db:"id"`
		Code string `db:"code"`
	}
	if err := db.Select(&got, "SELECT id, code FROM t"); err != nil {
		t.Fatalf("Select: %v", err)
	}
	if len(got) != 2 || got[0].ID != 1 || got[0].Code != "a" ||
		got[1].ID != 2 || got[1].Code != "b" {
		t.Errorf("read %+v, want rows 1 a and 2 b", got)
	}
}

// Rows hands over each row once, then io.EOF for every later call.
func TestRowsEndsWithEOF(t *testing.T) {
	t.Parallel()
	r := &Rows{
		Cols:   []string{"n"},
		Values: [][]driver.Value{{int64(7)}},
	}
	if got := r.Columns(); !reflect.DeepEqual(got, []string{"n"}) {
		t.Errorf("Columns = %v, want [n]", got)
	}
	dest := make([]driver.Value, 1)
	if err := r.Next(dest); err != nil || dest[0] != int64(7) {
		t.Fatalf("Next = %v, %v; want the row 7", dest[0], err)
	}
	for range 2 {
		if err := r.Next(dest); err != io.EOF {
			t.Errorf("Next past the end = %v, want io.EOF", err)
		}
	}
	if err := r.Close(); err != nil {
		t.Errorf("Close = %v", err)
	}
}

// The parts a fake may not use refuse, or do nothing, and every connector
// belongs to the one Driver.
func TestUnusedPartsRefuse(t *testing.T) {
	t.Parallel()
	if c, err := (Driver{}).Open("dsn"); c != nil || err == nil {
		t.Errorf("Driver.Open = %v, %v; want a refusal", c, err)
	}
	if s, err := (ConnBase{}).Prepare("SELECT 1"); s != nil || err == nil {
		t.Errorf("Prepare = %v, %v; want a refusal", s, err)
	}
	if tx, err := (ConnBase{}).Begin(); tx != nil || err == nil {
		t.Errorf("Begin = %v, %v; want a refusal", tx, err)
	}
	if err := (ConnBase{}).Close(); err != nil {
		t.Errorf("Close = %v, want nil", err)
	}
	if d := (ConnectorBase{}).Driver(); d != (Driver{}) {
		t.Errorf("Driver = %#v, want Driver{}", d)
	}
}

// Log keeps each statement in the order it was handed over, with the
// value of each argument and an empty Args for a statement that has none,
// and hands out copies rather than its own slice.
func TestLogKeepsWhatAConnectionIsHanded(t *testing.T) {
	t.Parallel()
	var log Log
	if got := log.Last(); !reflect.DeepEqual(got, Statement{}) {
		t.Errorf("Last of an empty Log = %+v, want none", got)
	}

	log.Record("SELECT 1", nil)
	log.Record("UPDATE t SET a = ? WHERE id = ?", []driver.NamedValue{
		{Ordinal: 1, Value: "x"},
		{Ordinal: 2, Value: int64(7)},
	})
	want := []Statement{
		{Query: "SELECT 1", Args: []any{}},
		{
			Query: "UPDATE t SET a = ? WHERE id = ?",
			Args:  []any{"x", int64(7)},
		},
	}
	got := log.Statements()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Statements = %#v, want %#v", got, want)
	}
	if last := log.Last(); !reflect.DeepEqual(last, want[1]) {
		t.Errorf("Last = %#v, want %#v", last, want[1])
	}

	got[0] = Statement{Query: "changed"}
	if again := log.Statements(); !reflect.DeepEqual(again, want) {
		t.Errorf("Statements handed out the Log's own slice: %#v", again)
	}
}

// The pool Open returns is closed once the test that opened it ends.
func TestOpenClosesWhenTheTestEnds(t *testing.T) {
	t.Parallel()
	var db *sqlx.DB
	t.Run("open", func(t *testing.T) {
		db = Open(t, connector{}, "sqlserver")
		if err := db.Ping(); err != nil {
			t.Fatalf("Ping inside the test: %v", err)
		}
	})
	if err := db.Ping(); err == nil {
		t.Error("the pool still answers after the test ended")
	}
}

// Selected separates expressions as SelectClause does, and one expression
// stands alone.
func TestSelected(t *testing.T) {
	t.Parallel()
	if got, want := Selected("t.id", "t.code"),
		"t.id,\n       t.code"; got != want {
		t.Errorf("Selected = %q, want %q", got, want)
	}
	if got := Selected("t.id"); got != "t.id" {
		t.Errorf("Selected of one = %q, want t.id", got)
	}
}
