package dbtest

// Tests for dbtest.go that need no server: the engines are the two the
// database package opens, a suite runs once per DSN that is set and is
// skipped when none is, a connection answers and is closed when the test
// that opened it ends, scratch names do not repeat, and a scratch table
// whose name has to be quoted is created and dropped under the quoted
// name and handed back unquoted.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"reflect"
	"strings"
	"testing"

	"github.com/jmoiron/sqlx"
	"github.com/mas-ony/go-toolkit/internal/dbdriver"
	"github.com/mas-ony/go-toolkit/internal/testutil/sqltest"
)

// openDriver is a driver whose every connection is a recorder keeping to
// a log nobody reads, so Connect has something to open and ping that needs
// no server.
type openDriver struct{}

// recorderConnector opens recorders that all keep to one log.
type recorderConnector struct {
	sqltest.ConnectorBase
	log *sqltest.Log
}

// recorder is a fake connection that keeps every statement it is handed
// in its log, in order, and runs none of them.
type recorder struct {
	sqltest.ConnBase
	log *sqltest.Log
}

// fakeEngine is the engine openDriver is registered as. It is registered
// once per test binary, because database/sql panics on a second
// registration of one name.
var fakeEngine = func() Engine {
	sql.Register("dbtest-fake", openDriver{})
	return Engine{Driver: "dbtest-fake"}
}()

// Connect opens a recorder onto the connector's log.
func (c recorderConnector) Connect(context.Context) (driver.Conn, error) {
	return recorder{log: c.log}, nil
}

// ExecContext keeps the statement and reports that it changed nothing.
func (r recorder) ExecContext(
	_ context.Context, query string, args []driver.NamedValue,
) (driver.Result, error) {
	r.log.Record(query, args)
	return driver.RowsAffected(0), nil
}

// Open opens a recorder onto a log of its own.
func (openDriver) Open(string) (driver.Conn, error) {
	return recorder{log: &sqltest.Log{}}, nil
}

// Both engines are listed, MySQL first, each under a driver name the
// database package resolves and the variable the README documents.
func TestEngines(t *testing.T) {
	t.Parallel()
	want := []Engine{
		{Driver: "mysql", DSNEnv: "DB_TEST_MYSQL_DSN"},
		{Driver: "sqlserver", DSNEnv: "DB_TEST_SQLSERVER_DSN"},
	}
	if len(Engines) != len(want) {
		t.Fatalf("Engines = %v, want %v", Engines, want)
	}
	for i, e := range Engines {
		if e != want[i] {
			t.Errorf("Engines[%d] = %+v, want %+v", i, e, want[i])
		}
		if dbdriver.Engine(e.Driver) != e.Driver {
			t.Errorf("%q is not an engine the database package opens",
				e.Driver)
		}
	}
}

// With neither variable set, the suite is skipped and nothing runs.
func TestEachSkipsWithoutADSN(t *testing.T) {
	for _, e := range Engines {
		t.Setenv(e.DSNEnv, "")
	}
	ran := false
	skipped := false
	t.Run("suite", func(t *testing.T) {
		t.Cleanup(func() { skipped = t.Skipped() })
		Each(t, func(*testing.T, Engine, string) { ran = true })
	})
	if ran || !skipped {
		t.Errorf("ran %t, skipped %t; want a skip and no run", ran, skipped)
	}
}

// With one variable set, the suite runs once, against that engine, and is
// handed the DSN as it was set.
func TestEachRunsPerDSN(t *testing.T) {
	t.Setenv("DB_TEST_MYSQL_DSN", "user:pass@tcp(db:3306)/scratch")
	t.Setenv("DB_TEST_SQLSERVER_DSN", "")
	var seen []string
	Each(t, func(t *testing.T, e Engine, dsn string) {
		seen = append(seen, e.Driver+" "+dsn)
		if t.Name() != "TestEachRunsPerDSN/mysql" {
			t.Errorf("subtest named %q", t.Name())
		}
	})
	if len(seen) != 1 || seen[0] != "mysql user:pass@tcp(db:3306)/scratch" {
		t.Errorf("ran %v, want one mysql run with its DSN", seen)
	}
}

// Connect hands over a connection that answers, and closes it once the
// test that asked for it ends.
func TestConnect(t *testing.T) {
	t.Parallel()
	var db *sqlx.DB
	opened := t.Run("open", func(t *testing.T) {
		db = Connect(t, fakeEngine, "")
		if err := db.Ping(); err != nil {
			t.Errorf("Ping inside the test: %v", err)
		}
	})
	if !opened {
		return
	}
	if err := db.Ping(); err == nil {
		t.Error("the connection is still open after the test ended")
	}
}

// Names carry the prefix and never repeat within a run.
func TestUnique(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for range 100 {
		name := Unique("scratch_")
		if !strings.HasPrefix(name, "scratch_") {
			t.Fatalf("%q lost its prefix", name)
		}
		if seen[name] {
			t.Fatalf("%q was handed out twice", name)
		}
		seen[name] = true
	}
}

// The table is created and, once the test that asked for it ends,
// dropped under the name quote renders, and the name comes back as it was
// before quoting: the prefix, then the characters that force quoting.
func TestQuotedScratchTable(t *testing.T) {
	t.Parallel()
	var log sqltest.Log
	db := sqltest.Open(t, recorderConnector{log: &log}, "mysql")
	quote := func(name string) string { return "<" + name + ">" }

	var name string
	t.Run("scratch", func(t *testing.T) {
		name = QuotedScratchTable(t, db, quote, "crud row",
			"CREATE TABLE %s (id INT)")
	})
	if !strings.HasPrefix(name, "crud row`] ") {
		t.Errorf("name %q does not start with the prefix and `] ", name)
	}
	want := []sqltest.Statement{
		{Query: "CREATE TABLE <" + name + "> (id INT)", Args: []any{}},
		{Query: "DROP TABLE <" + name + ">", Args: []any{}},
	}
	if got := log.Statements(); !reflect.DeepEqual(got, want) {
		t.Errorf("ran %+v, want %+v", got, want)
	}
}
