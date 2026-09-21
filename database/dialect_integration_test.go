//go:build integration

package database

// The shared harness for every *_integration_test.go file in this package,
// plus the integration tests for dialect.go.
//
// Each test runs once per engine whose DSN is set and skips when neither
// is:
//
//	DB_TEST_MYSQL_DSN      user:pass@tcp(host:3306)/scratch
//	DB_TEST_SQLSERVER_DSN  sqlserver://user:pass@host?database=scratch
//
//	go test -tags integration -run Integration ./database
//
// The build tag is what keeps a normal `go test` from compiling any of
// this. The env-var skip alone would be enough to make the suite quiet, so
// the tag buys something else: a scratch server, two drivers and a DDL
// round trip stay out of the default build entirely. The cost is that a
// change breaking these files compiles clean until someone runs with the
// tag, which is what CI is for.
//
// The tests create and drop their own tables. Point the DSNs at a scratch
// database, never at one that holds data.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
	_ "github.com/microsoft/go-mssqldb"
)

// integrationEngines maps each grammar to its driver name and DSN variable.
var integrationEngines = []struct {
	dialect Dialect
	driver  string
	dsnEnv  string
}{
	{DialectMySQL, "mysql", "DB_TEST_MYSQL_DSN"},
	{DialectSQLServer, "sqlserver", "DB_TEST_SQLSERVER_DSN"},
}

// forEachEngine runs fn as a subtest for every engine with a DSN, with that
// engine's grammar selected and a connection open.
func forEachEngine(t *testing.T, fn func(t *testing.T, db *sqlx.DB)) {
	ran := false
	for _, e := range integrationEngines {
		dsn := os.Getenv(e.dsnEnv)
		if dsn == "" {
			continue
		}
		ran = true
		t.Run(e.dialect.String(), func(t *testing.T) {
			db, err := sqlx.Connect(e.driver, dsn)
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			t.Cleanup(func() { _ = db.Close() })
			withDialect(t, e.dialect)
			fn(t, db)
		})
	}
	if !ran {
		t.Skip("no DB_TEST_*_DSN variable is set")
	}
}

// tableSeq keeps scratch table names unique within one run.
var tableSeq atomic.Int64

// scratchTable creates a table from ddl, in which %s stands for the name,
// and drops it when the test ends. The name is quoted with quoteIdent, so
// it doubles as a check that a quoted identifier works in DDL and DML.
func scratchTable(t *testing.T, db *sqlx.DB, base, ddl string) string {
	t.Helper()
	name := QuoteIdent(fmt.Sprintf("dialect %s`] %d_%d",
		base, time.Now().UnixNano()%1e9, tableSeq.Add(1)))
	mustExec(t, db, fmt.Sprintf(ddl, name))
	t.Cleanup(func() { _, _ = db.Exec("DROP TABLE " + name) })
	return name
}

// mustExec runs each statement and stops the test at the first error.
func mustExec(t *testing.T, db *sqlx.DB, stmts ...string) {
	t.Helper()
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%v\n%s", err, s)
		}
	}
}

// onEngine returns mysql or sqlserver, whichever the selected grammar needs.
func onEngine(mysql, sqlserver string) string {
	if activeDialect() == DialectMySQL {
		return mysql
	}
	return sqlserver
}

type labelRow struct {
	ID    int    `db:"id"`
	Label string `db:"label"`
}

func TestIntegrationInsertReturningID(t *testing.T) {
	forEachEngine(t, func(t *testing.T, db *sqlx.DB) {
		ctx := context.Background()
		table := scratchTable(t, db, "identity", onEngine(
			"CREATE TABLE %s (id BIGINT AUTO_INCREMENT PRIMARY KEY, "+
				"label VARCHAR(50) NOT NULL, "+
				"created_at DATETIME(0) NOT NULL)",
			"CREATE TABLE %s (id BIGINT IDENTITY(1,1) PRIMARY KEY, "+
				"label NVARCHAR(50) NOT NULL, "+
				"created_at DATETIME2(0) NOT NULL)",
		))
		if activeDialect() == DialectSQLServer {
			// An enabled INSERT trigger is what makes SQL Server reject
			// OUTPUT without INTO, so the insert has to work with one.
			// MySQL has no such rule, and creating a trigger there needs
			// SUPER when binary logging is on, so it is skipped.
			trigger := QuoteIdent(fmt.Sprintf("dialect_trg_%d",
				time.Now().UnixNano()%1e9))
			mustExec(t, db, "CREATE TRIGGER "+trigger+" ON "+table+
				" AFTER INSERT AS SET NOCOUNT ON;")
		}

		var ids []int
		for _, label := range []string{"first", "second"} {
			id, err := InsertReturningID(ctx, db, table,
				"label, created_at",
				":label, COALESCE(:created_at, "+NowExpr()+")",
				"id",
				map[string]any{"label": label, "created_at": nil})
			if err != nil {
				t.Fatalf("insert %s: %v", label, err)
			}
			ids = append(ids, id)
		}
		if ids[0] <= 0 || ids[1] <= ids[0] {
			t.Errorf("ids = %v, want two increasing positive keys", ids)
		}

		var got []labelRow
		if err := db.Select(&got, "SELECT id, label FROM "+table+
			" ORDER BY id"); err != nil {
			t.Fatal(err)
		}
		want := []labelRow{{ids[0], "first"}, {ids[1], "second"}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("rows = %v, want %v", got, want)
		}

		if activeDialect() != DialectMySQL {
			return
		}
		plain := scratchTable(t, db, "no_identity",
			"CREATE TABLE %s (id BIGINT PRIMARY KEY, label VARCHAR(50))")
		_, err := InsertReturningID(ctx, db, plain, "id, label",
			":id, :label", "id", labelRow{ID: 7, Label: "x"})
		if !errors.Is(err, ErrNoIdentityValue) {
			t.Errorf("insert without AUTO_INCREMENT: err = %v, want %v",
				err, ErrNoIdentityValue)
		}
	})
}

func TestIntegrationApplyLimitAndPageClause(t *testing.T) {
	forEachEngine(t, func(t *testing.T, db *sqlx.DB) {
		table := scratchTable(t, db, "paging",
			"CREATE TABLE %s (id INT PRIMARY KEY, grp INT NOT NULL)")
		for i := 1; i <= 25; i++ {
			mustExec(t, db, fmt.Sprintf(
				"INSERT INTO %s (id, grp) VALUES (%d, %d)", table, i, i%3))
		}

		var grps []int
		distinct := ApplyLimit("SELECT DISTINCT grp FROM "+table+
			"\nORDER BY grp", 2)
		if err := db.Select(&grps, distinct); err != nil {
			t.Fatalf("%v\n%s", err, distinct)
		}
		if !reflect.DeepEqual(grps, []int{0, 1}) {
			t.Errorf("capped DISTINCT = %v, want [0 1]", grps)
		}

		base := "SELECT id FROM " + table + "\nWHERE grp >= :min_grp"
		args := map[string]any{"min_grp": 0}

		var total int
		countQ, countArgs, err := sqlx.Named(
			"SELECT COUNT(*) FROM ("+base+") sub", args)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Get(&total, db.Rebind(countQ),
			countArgs...); err != nil {
			t.Fatal(err)
		}
		if total != 25 {
			t.Errorf("count = %d, want 25", total)
		}

		for _, c := range []struct {
			page, limit int
			want        []int
		}{
			{3, 10, []int{21, 22, 23, 24, 25}},
			{4, 10, nil},
			{0, 0, []int{1}},
		} {
			query := base + "\nORDER BY id" + PageClause(args, c.page, c.limit)
			q, a, err := sqlx.Named(query, args)
			if err != nil {
				t.Fatal(err)
			}
			var ids []int
			if err := db.Select(&ids, db.Rebind(q), a...); err != nil {
				t.Fatalf("page %d: %v\n%s", c.page, err, query)
			}
			if !reflect.DeepEqual(ids, c.want) {
				t.Errorf("page %d limit %d = %v, want %v",
					c.page, c.limit, ids, c.want)
			}
		}
	})
}

func TestIntegrationLike(t *testing.T) {
	forEachEngine(t, func(t *testing.T, db *sqlx.DB) {
		table := scratchTable(t, db, "like",
			"CREATE TABLE %s (v VARCHAR(20) NOT NULL)")
		values := []string{
			"50%", "50x", "a_b", "axb", "[x]", "x]", "a!b", "ab", `a\b`,
		}
		for _, v := range values {
			if _, err := db.NamedExec("INSERT INTO "+table+
				" (v) VALUES (:v)", map[string]any{"v": v}); err != nil {
				t.Fatal(err)
			}
		}

		for search, want := range map[string][]string{
			"50%": {"50%"},
			"a_b": {"a_b"},
			"[x":  {"[x]"},
			"a!b": {"a!b"},
			`a\b`: {`a\b`},
		} {
			query := "SELECT v FROM " + table + " WHERE " +
				LikePredicate("v", "search")
			q, a, err := sqlx.Named(query,
				map[string]any{"search": LikeArg(search)})
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			if err := db.Select(&got, db.Rebind(q), a...); err != nil {
				t.Fatalf("%v\n%s", err, query)
			}
			sort.Strings(got)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("search %q matched %q, want %q",
					search, got, want)
			}
		}
	})
}

func TestIntegrationRecursiveCTE(t *testing.T) {
	forEachEngine(t, func(t *testing.T, db *sqlx.DB) {
		table := scratchTable(t, db, "tree",
			"CREATE TABLE %s (id INT PRIMARY KEY, parent_id INT NULL)")
		mustExec(t, db, "INSERT INTO "+table+" (id, parent_id) VALUES "+
			"(1, NULL), (2, 1), (3, 2), (4, 3), (5, NULL)")

		query := RecursiveCTE("tree") +
			"\n  SELECT n.id, 0 AS depth FROM " + table + " n" +
			"\n  WHERE n.id = :root" +
			"\n  UNION ALL" +
			"\n  SELECT c.id, t.depth + 1 FROM " + table + " c" +
			"\n  JOIN tree t ON c.parent_id = t.id" +
			"\n  WHERE t.depth < :max_depth" +
			"\n)" +
			"\nSELECT id FROM tree ORDER BY id"
		for maxDepth, want := range map[int][]int{
			10: {1, 2, 3, 4},
			1:  {1, 2},
		} {
			q, a, err := sqlx.Named(query,
				map[string]any{"root": 1, "max_depth": maxDepth})
			if err != nil {
				t.Fatal(err)
			}
			var got []int
			if err := db.Select(&got, db.Rebind(q), a...); err != nil {
				t.Fatalf("%v\n%s", err, query)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("max depth %d: ids = %v, want %v",
					maxDepth, got, want)
			}
		}
	})
}

func TestIntegrationQuoteIdentAndNowExpr(t *testing.T) {
	forEachEngine(t, func(t *testing.T, db *sqlx.DB) {
		user := QuoteIdent("user")
		table := scratchTable(t, db, "quote", onEngine(
			"CREATE TABLE %s ("+user+" INT, stamp DATETIME(0))",
			"CREATE TABLE %s ("+user+" INT, stamp DATETIME2(0))",
		))
		mustExec(t, db, "INSERT INTO "+table+" ("+user+", stamp) "+
			"VALUES (1, "+NowExpr()+")")

		var got struct {
			User  int    `db:"user"`
			Stamp string `db:"stamp"`
		}
		if err := db.Get(&got, "SELECT "+user+", stamp FROM "+
			table); err != nil {
			t.Fatal(err)
		}
		if got.User != 1 || got.Stamp == "" {
			t.Errorf("row = %+v, want user 1 and a stamp", got)
		}
	})
}
