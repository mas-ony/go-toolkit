package database

// Tests for dialect.go.
//
// What the file promises: ParseDialect accepts every spelling of the two
// drivers and nothing else, both grammars are spelled exactly, each
// package-level helper equals its method and refuses to run before
// SetDialect, a method refuses a Dialect that is no grammar, ApplyLimit and
// PageClause agree across engines on what they leave alone and what they
// bind, and the LIKE escape reads the same way on both. Whether a server
// accepts the SQL is dialect_integration_test.go's question.

import (
	"context"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/jmoiron/sqlx"
)

// dialects lists every grammar, for tests that assert something of both.
var dialects = []Dialect{DialectMySQL, DialectSQLServer}

// dialectHelpers pairs each package-level helper with the method it wraps,
// as two closures producing the same string from the same arguments.
//
// Every argument is chosen so the call reaches the grammar: "user" is
// reserved on one engine and not the other, and the query opens with a
// SELECT that ApplyLimit will recognise. A plain name or an unrecognised
// query would take a short-circuit path and prove nothing.
//
// InsertReturningID is absent because it returns no string. Its receiver is
// checked in the panic test below, which is the only part of it that can be
// reached without a connection.
var dialectHelpers = []struct {
	name     string
	selected func() string
	method   func(Dialect) string
}{
	{
		"QuoteIdent",
		func() string { return QuoteIdent("user") },
		func(d Dialect) string { return d.QuoteIdent("user") },
	},
	{
		"NowExpr",
		func() string { return NowExpr() },
		func(d Dialect) string { return d.NowExpr() },
	},
	{
		"ApplyLimit",
		func() string { return ApplyLimit("SELECT a FROM t", 5) },
		func(d Dialect) string {
			return d.ApplyLimit("SELECT a FROM t", 5)
		},
	},
	{
		"PageClause",
		func() string { return PageClause(map[string]any{}, 2, 10) },
		func(d Dialect) string {
			return d.PageClause(map[string]any{}, 2, 10)
		},
	},
	{
		"RecursiveCTE",
		func() string { return RecursiveCTE("tree") },
		func(d Dialect) string { return d.RecursiveCTE("tree") },
	},
	{
		"Qualify",
		func() string { return Qualify("app", "user") },
		func(d Dialect) string { return d.Qualify("app", "user") },
	},
}

// mustPanic reports an error unless f panics.
func mustPanic(t *testing.T, name string, f func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Errorf("%s did not panic", name)
		}
	}()
	f()
}

// withDialect selects d until the test ends and then restores the previous
// selection, including no selection. The selection is package-wide, so a
// test that calls it must not run in parallel.
func withDialect(t *testing.T, d Dialect) {
	t.Helper()
	prev := selectedDialect.Load()
	t.Cleanup(func() { selectedDialect.Store(prev) })
	SetDialect(d)
}

// withoutDialect clears the selection until the test ends.
func withoutDialect(t *testing.T) {
	t.Helper()
	prev := selectedDialect.Load()
	t.Cleanup(func() { selectedDialect.Store(prev) })
	selectedDialect.Store(0)
}

// ParseDialect accepts every spelling of both drivers in any case and
// spacing, round-trips each Dialect's own name, and refuses everything else
// with ErrUnsupportedDriver.
func TestParseDialect(t *testing.T) {
	accepted := map[string]Dialect{
		"mysql":       DialectMySQL,
		" MySQL ":     DialectMySQL,
		"sqlserver":   DialectSQLServer,
		"SqlServer":   DialectSQLServer,
		"mssql":       DialectSQLServer,
		"azuresql\n":  DialectSQLServer,
		"\tAZURESQL ": DialectSQLServer,
	}
	for name, want := range accepted {
		got, err := ParseDialect(name)
		if err != nil || got != want {
			t.Errorf("ParseDialect(%q) = %v, %v; want %v, nil",
				name, got, err, want)
		}
	}

	for _, name := range []string{"", "postgres", "sqlite3", "my sql"} {
		_, err := ParseDialect(name)
		if !errors.Is(err, ErrUnsupportedDriver) {
			t.Errorf("ParseDialect(%q) error = %v, want %v",
				name, err, ErrUnsupportedDriver)
		}
	}

	for _, d := range dialects {
		if got, err := ParseDialect(d.String()); err != nil || got != d {
			t.Errorf("ParseDialect(%q) = %v, %v; want %v, nil",
				d.String(), got, err, d)
		}
	}
}

// No engine is a safe default, so every helper that depends on the grammar
// must refuse to run before SetDialect, including on its no-op paths.
func TestHelpersPanicWithoutDialect(t *testing.T) {
	var zero Dialect
	for _, d := range dialects {
		if zero == d {
			t.Fatalf("the zero Dialect is %v", d)
		}
	}

	withoutDialect(t)
	helpers := map[string]func(){
		"ApplyLimit":      func() { ApplyLimit("SELECT a FROM t", 1) },
		"ApplyLimit(n=0)": func() { ApplyLimit("SELECT a FROM t", 0) },
		"PageClause":      func() { PageClause(map[string]any{}, 1, 10) },
		"NowExpr":         func() { NowExpr() },
		"RecursiveCTE":    func() { RecursiveCTE("tree") },
		"QuoteIdent":      func() { QuoteIdent("user") },
		"InsertReturningID": func() {
			_, _ = InsertReturningID(context.Background(), nil,
				"t", "a", ":a", "id", struct{}{})
		},
	}
	for name, f := range helpers {
		mustPanic(t, name, f)
	}
}

// SetDialect refuses a value that is no grammar, and stores nothing when
// it does.
func TestSetDialectRejectsInvalidValues(t *testing.T) {
	withoutDialect(t)
	for _, d := range []Dialect{0, -1, DialectSQLServer + 1} {
		mustPanic(t, "SetDialect("+d.String()+")", func() { SetDialect(d) })
	}
	if got := selectedDialect.Load(); got != 0 {
		t.Errorf("a rejected SetDialect stored %d", got)
	}
}

// ApplyLimit appends LIMIT on MySQL and places TOP after SELECT, DISTINCT
// or ALL on SQL Server, whatever the case and spacing of the keywords.
func TestApplyLimit(t *testing.T) {
	cases := []struct {
		name, query, mysql, sqlserver string
	}{
		{
			name:      "plain",
			query:     "SELECT a\nFROM t\nORDER BY a",
			mysql:     "SELECT a\nFROM t\nORDER BY a\nLIMIT 2",
			sqlserver: "SELECT TOP (2) a\nFROM t\nORDER BY a",
		},
		{
			name:      "distinct",
			query:     "SELECT DISTINCT a FROM t",
			mysql:     "SELECT DISTINCT a FROM t\nLIMIT 2",
			sqlserver: "SELECT DISTINCT TOP (2) a FROM t",
		},
		{
			name:      "all",
			query:     "SELECT ALL a FROM t",
			mysql:     "SELECT ALL a FROM t\nLIMIT 2",
			sqlserver: "SELECT ALL TOP (2) a FROM t",
		},
		{
			name:      "letter case and white space",
			query:     "\n  select\n\tdistinct\ta FROM t",
			mysql:     "\n  select\n\tdistinct\ta FROM t\nLIMIT 2",
			sqlserver: "\n  select\n\tdistinct\tTOP (2) a FROM t",
		},
		{
			name:      "column named like a keyword",
			query:     "SELECT allowance FROM t",
			mysql:     "SELECT allowance FROM t\nLIMIT 2",
			sqlserver: "SELECT TOP (2) allowance FROM t",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			withDialect(t, DialectMySQL)
			if got := ApplyLimit(c.query, 2); got != c.mysql {
				t.Errorf("MySQL:\n got %q\nwant %q", got, c.mysql)
			}
			withDialect(t, DialectSQLServer)
			if got := ApplyLimit(c.query, 2); got != c.sqlserver {
				t.Errorf("SQL Server:\n got %q\nwant %q", got, c.sqlserver)
			}
		})
	}
}

// Both engines must agree on what is left alone, so a caller bug shows up
// the same way whichever engine the tests run against.
func TestApplyLimitLeavesQueryUnchanged(t *testing.T) {
	const q = "SELECT a FROM t"
	notSelect := []string{
		"",
		"WITH x AS (SELECT 1 AS a) SELECT a FROM x",
		"(SELECT a FROM t)",
		"SELECT*FROM t",
		"-- note\nSELECT a FROM t",
	}
	for _, d := range dialects {
		withDialect(t, d)
		for _, n := range []int{0, -1} {
			if got := ApplyLimit(q, n); got != q {
				t.Errorf("%v: ApplyLimit(q, %d) = %q", d, n, got)
			}
		}
		for _, s := range notSelect {
			if got := ApplyLimit(s, 1); got != s {
				t.Errorf("%v: ApplyLimit(%q, 1) = %q", d, s, got)
			}
		}
	}
}

// PageClause emits each engine's tail and binds an offset and a limit that
// both engines accept, raising a page or limit below 1 and saturating an
// offset that would overflow.
func TestPageClause(t *testing.T) {
	tails := map[Dialect]string{
		DialectMySQL:     "\nLIMIT :limit OFFSET :offset",
		DialectSQLServer: "\nOFFSET :offset ROWS FETCH NEXT :limit ROWS ONLY",
	}
	cases := []struct {
		name                  string
		page, limit           int
		wantOffset, wantLimit int
	}{
		{"first page", 1, 10, 0, 10},
		{"third page", 3, 10, 20, 10},
		{"page zero", 0, 10, 0, 10},
		{"negative page", -5, 10, 0, 10},
		{"limit zero", 2, 0, 1, 1},
		{"overflowing offset", math.MaxInt, 10, math.MaxInt, 10},
		{"largest exact offset", math.MaxInt, 1, math.MaxInt - 1, 1},
	}
	for _, d := range dialects {
		withDialect(t, d)
		for _, c := range cases {
			args := map[string]any{}
			if got := PageClause(args, c.page, c.limit); got != tails[d] {
				t.Errorf("%v %s: tail = %q", d, c.name, got)
			}
			if args["offset"] != c.wantOffset ||
				args["limit"] != c.wantLimit {
				t.Errorf("%v %s: offset, limit = %v, %v; want %d, %d",
					d, c.name, args["offset"], args["limit"],
					c.wantOffset, c.wantLimit)
			}
		}
	}
}

// PageClause relies on two sqlx.Named behaviours: positional arguments
// follow the order of the names in the text, and keys the text does not
// reference are ignored.
func TestPageClauseBindsThroughSqlxNamed(t *testing.T) {
	const query = "SELECT a FROM t WHERE b = :b ORDER BY a"
	want := map[Dialect][]any{
		DialectMySQL:     {"x", 10, 20},
		DialectSQLServer: {"x", 20, 10},
	}
	for _, d := range dialects {
		withDialect(t, d)
		args := map[string]any{"b": "x"}
		tail := PageClause(args, 3, 10)

		_, got, err := sqlx.Named(query+tail, args)
		if err != nil {
			t.Fatalf("%v: %v", d, err)
		}
		if !reflect.DeepEqual(got, want[d]) {
			t.Errorf("%v: page arguments = %v, want %v", d, got, want[d])
		}

		_, got, err = sqlx.Named("SELECT COUNT(*) FROM t WHERE b = :b", args)
		if err != nil || !reflect.DeepEqual(got, []any{"x"}) {
			t.Errorf("%v: count arguments = %v, %v; want [x], nil",
				d, got, err)
		}
	}
}

// The fixed spellings each grammar emits, including the doubled closing
// quote inside a quoted identifier.
func TestDialectSpellings(t *testing.T) {
	cases := []struct {
		name             string
		emit             func() string
		mysql, sqlserver string
	}{
		{
			name:      "NowExpr",
			emit:      NowExpr,
			mysql:     "NOW()",
			sqlserver: "SYSDATETIME()",
		},
		{
			name:      "RecursiveCTE",
			emit:      func() string { return RecursiveCTE("tree") },
			mysql:     "WITH RECURSIVE tree AS (",
			sqlserver: "WITH tree AS (",
		},
		{
			name:      "QuoteIdent",
			emit:      func() string { return QuoteIdent("user") },
			mysql:     "`user`",
			sqlserver: "[user]",
		},
		{
			name:      "QuoteIdent doubles the closing quote",
			emit:      func() string { return QuoteIdent("a`b]c[d") },
			mysql:     "`a``b]c[d`",
			sqlserver: "[a`b]]c[d]",
		},
	}
	for _, c := range cases {
		withDialect(t, DialectMySQL)
		if got := c.emit(); got != c.mysql {
			t.Errorf("%s on MySQL = %q, want %q", c.name, got, c.mysql)
		}
		withDialect(t, DialectSQLServer)
		if got := c.emit(); got != c.sqlserver {
			t.Errorf("%s on SQL Server = %q, want %q",
				c.name, got, c.sqlserver)
		}
	}
}

// LikeArg escapes the escape character, both wildcards and T-SQL's
// character-class bracket, and nothing else.
func TestLikeArg(t *testing.T) {
	cases := map[string]string{
		"":         "%%",
		"ordinary": "%ordinary%",
		"50%":      "%50!%%",
		"a_b":      "%a!_b%",
		"[x]":      "%![x]%",
		"a!b":      "%a!!b%",
		`a\b`:      `%a\b%`,
		"!%_[":     "%!!!%!_![%",
	}
	for in, want := range cases {
		if got := LikeArg(in); got != want {
			t.Errorf("LikeArg(%q) = %q, want %q", in, got, want)
		}
	}
}

// LikeArg and LikePredicate meet only inside the database, so the escape
// character they share is pinned here: one character, ordinary inside a
// string literal on both engines and a LIKE wildcard on neither.
func TestLikePredicate(t *testing.T) {
	const want = "t.name LIKE :q ESCAPE '!'"
	if got := LikePredicate("t.name", "q"); got != want {
		t.Errorf("LikePredicate = %q, want %q", got, want)
	}
	if len(likeEscape) != 1 ||
		strings.ContainsAny(likeEscape, `\'"%_[]^-`) {
		t.Errorf("likeEscape %q is not safe on both engines", likeEscape)
	}
}

// The contract between the two forms: the package-level function is a
// wrapper that adds the selection and nothing else. Without this, the two
// could drift into spelling the same clause differently.
func TestDialectMethodsMatchTheSelectedForm(t *testing.T) {
	for _, d := range dialects {
		t.Run(d.String(), func(t *testing.T) {
			withDialect(t, d)
			for _, h := range dialectHelpers {
				selected, method := h.selected(), h.method(d)
				if selected != method {
					t.Errorf("%s: selected form %q, method %q",
						h.name, selected, method)
				}
			}
		})
	}
}

// The reason the method form exists: it is usable where SetDialect has
// never run, which is what lets one process spell SQL for both engines.
//
// The assertion is mostly the absence of a panic — every one of these calls
// would panic through activeDialect in its package-level form. The compared
// values are the second half: a method that quietly returned the same text
// for both grammars would pass a no-panic check alone.
func TestDialectMethodsNeedNoSelection(t *testing.T) {
	withoutDialect(t)
	for _, h := range dialectHelpers {
		mysql := h.method(DialectMySQL)
		sqlserver := h.method(DialectSQLServer)
		if mysql == "" || sqlserver == "" {
			t.Errorf("%s returned an empty clause", h.name)
		}
		if mysql == sqlserver {
			t.Errorf("%s spelled both grammars as %q", h.name, mysql)
		}
	}
}

// A Dialect that is no grammar must panic rather than fall into the T-SQL
// side of an if/else, and a valid SELECTION must not rescue an invalid
// RECEIVER — which is why a dialect is selected first here.
func TestDialectMethodsPanicOnANonGrammar(t *testing.T) {
	withDialect(t, DialectMySQL)
	for _, d := range []Dialect{0, -1, DialectSQLServer + 1} {
		for _, h := range dialectHelpers {
			mustPanic(t, h.name+" on "+d.String(),
				func() { _ = h.method(d) })
		}
		mustPanic(t, "InsertReturningID on "+d.String(), func() {
			_, _ = d.InsertReturningID(context.Background(), nil,
				"t", "a", ":a", "id", struct{}{})
		})
	}
}
