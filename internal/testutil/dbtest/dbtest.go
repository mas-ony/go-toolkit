package dbtest

import (
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
	"github.com/mas-ony/go-toolkit/internal/dbdriver"
	_ "github.com/microsoft/go-mssqldb"
)

// Engine is one server an integration suite can run against.
type Engine struct {
	// Driver is the database/sql driver name the server is opened with,
	// dbdriver.MySQL or dbdriver.SQLServer, and the name of the subtest
	// that runs against it.
	Driver string

	// DSNEnv is the variable the server's DSN is read from.
	DSNEnv string
}

// seq keeps scratch table names unique within one run.
var seq atomic.Int64

// Engines lists the servers a suite can run against, MySQL first.
var Engines = []Engine{
	{Driver: dbdriver.MySQL, DSNEnv: "DB_TEST_MYSQL_DSN"},
	{Driver: dbdriver.SQLServer, DSNEnv: "DB_TEST_SQLSERVER_DSN"},
}

// Each runs fn as a subtest for every engine whose DSN variable is set,
// handing it that DSN, and skips t when none is set.
func Each(t *testing.T, fn func(t *testing.T, e Engine, dsn string)) {
	t.Helper()
	ran := false
	for _, e := range Engines {
		dsn := os.Getenv(e.DSNEnv)
		if dsn == "" {
			continue
		}
		ran = true
		t.Run(e.Driver, func(t *testing.T) { fn(t, e, dsn) })
	}
	if !ran {
		t.Skip("no DB_TEST_*_DSN variable is set")
	}
}

// ForEach is Each with the connection opened: fn is handed an open
// database rather than a DSN. A server that refuses the connection fails
// its subtest, and the connection is closed when the subtest ends.
func ForEach(t *testing.T, fn func(t *testing.T, e Engine, db *sqlx.DB)) {
	t.Helper()
	Each(t, func(t *testing.T, e Engine, dsn string) {
		db, err := sqlx.Connect(e.Driver, dsn)
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		fn(t, e, db)
	})
}

// Unique returns prefix followed by a number no other call in this process
// returns, and one an earlier run is unlikely to have left behind, for
// naming a scratch table or trigger, or an object a test needs the server
// not to hold.
func Unique(prefix string) string {
	return fmt.Sprintf("%s%d_%d",
		prefix,
		time.Now().UnixNano()%1e9,
		seq.Add(1))
}

// Exec runs each of stmts on db in turn, and stops the test at the first
// one the server refuses, printing the statement under the server's own
// error so the failure names what was sent.
func Exec(t *testing.T, db *sqlx.DB, stmts ...string) {
	t.Helper()
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%v\n%s", err, stmt)
		}
	}
}

// ScratchTable runs ddl, in which %s stands for name, through Exec, and
// drops the table when the test ends. name is used as given, so the
// caller quotes it as its engine needs; it is returned for the caller's
// queries.
func ScratchTable(t *testing.T, db *sqlx.DB, name, ddl string) string {
	t.Helper()
	Exec(t, db, fmt.Sprintf(ddl, name))
	t.Cleanup(func() { _, _ = db.Exec("DROP TABLE " + name) })
	return name
}
