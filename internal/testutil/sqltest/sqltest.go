package sqltest

import (
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/jmoiron/sqlx"
)

// Driver is the driver.Driver behind every fake connector. Its Open
// refuses, because a fake is reached only through its connector, which
// Open hands to database/sql directly.
type Driver struct{}

// ConnectorBase supplies the Driver method of a fake driver.Connector,
// which embeds it and adds the Connect that opens a connection onto the
// fake's own state.
type ConnectorBase struct{}

// ConnBase supplies the methods a driver.Conn must have whether a fake
// uses them or not. Prepare refuses, because every statement reaches a
// fake through QueryContext, ExecContext or BeginTx instead. Begin
// refuses too, for a fake that starts no transaction; one that does
// declares a Begin of its own, which takes the place of this one. Close
// releases nothing, because a fake holds nothing to release. A fake
// connection embeds it.
type ConnBase struct{}

// Rows replays a canned result set, one row per Next.
type Rows struct {
	// Cols names the columns, and Values holds the rows, each with one
	// value per column.
	Cols   []string
	Values [][]driver.Value

	// next is the index of the row the next call to Next copies out.
	next int
}

// Statement is one statement a fake connection was handed: its text as
// the driver received it, placeholders already rewritten for the driver
// name the pool was opened under, and the value of each argument in the
// order the driver received them.
type Statement struct {
	Query string
	Args  []any
}

// Log keeps the statements a fake connection is handed, in order, so a
// test can read back the SQL the code under test sent. Its zero value is
// ready to use, and it is safe for the concurrent use database/sql makes
// of a pool's connections.
type Log struct {
	mu   sync.Mutex
	seen []Statement
}

// Selected joins column expressions the way database.SelectClause joins
// them, so a test spells the SELECT list it expects as a list rather than
// as one escaped string.
func Selected(exprs ...string) string {
	return strings.Join(exprs, ",\n       ")
}

// Open returns a *sqlx.DB over c, bound as driverName, and closes it when
// the test ends.
//
// sql.OpenDB with the connector avoids sql.Register, which is a
// process-global map: registering per test would either collide or force
// a unique name scheme, and both are worse than passing the connector
// directly. driverName never reaches a registry, so its only job is to
// pick sqlx's placeholder style: ? for "mysql", @p1 for "sqlserver".
func Open(t testing.TB, c driver.Connector, driverName string) *sqlx.DB {
	t.Helper()
	db := sqlx.NewDb(sql.OpenDB(c), driverName)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// Open refuses: a fake is reachable only through its connector.
func (Driver) Open(string) (driver.Conn, error) {
	return nil, errors.New(
		"fake: this driver is only reachable through its connector")
}

// Driver returns the driver every fake connector belongs to.
func (ConnectorBase) Driver() driver.Driver { return Driver{} }

// Prepare refuses: no fake is handed a statement to prepare.
func (ConnBase) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("fake: statements are not prepared")
}

// Begin refuses: a fake that starts transactions declares its own.
func (ConnBase) Begin() (driver.Tx, error) {
	return nil, errors.New("fake: transactions are not supported")
}

// Close releases nothing; a fake holds no resources.
func (ConnBase) Close() error { return nil }

// Columns returns the canned column names.
func (r *Rows) Columns() []string { return r.Cols }

// Close releases nothing; the rows live in memory.
func (r *Rows) Close() error { return nil }

// Next copies the next canned row into dest, or reports io.EOF once every
// row has been copied out.
func (r *Rows) Next(dest []driver.Value) error {
	if r.next >= len(r.Values) {
		return io.EOF
	}
	copy(dest, r.Values[r.next])
	r.next++
	return nil
}

// Record keeps query and the value of each of args. A statement handed no
// arguments is kept with an empty Args rather than a nil one, so a test
// that spells it []any{} compares equal.
func (l *Log) Record(query string, args []driver.NamedValue) {
	vals := make([]any, len(args))
	for i, a := range args {
		vals[i] = a.Value
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seen = append(l.seen, Statement{Query: query, Args: vals})
}

// Statements returns every statement kept so far, oldest first, in a
// slice the caller may keep.
func (l *Log) Statements() []Statement {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.seen)
}

// Last returns the statement kept most recently, and the zero Statement
// when none has been.
func (l *Log) Last() Statement {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.seen) == 0 {
		return Statement{}
	}
	return l.seen[len(l.seen)-1]
}
