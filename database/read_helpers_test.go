package database

// Tests for read_helpers.go.
//
// The clause builders inspect strings and open nothing. GetOne, CountQuery
// and SelectList reach a driver, so they are exercised twice: here against
// the fake at the bottom of this file, which is what makes their contract
// hold on a plain `go test`, and in read_helpers_integration_test.go
// against real servers, which is what shows the SQL they build is accepted.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/jmoiron/sqlx"
)

type queryDriver struct{}

type queryConnector struct{ db *queryDB }

// queryConn implements driver.QueryerContext, which is what lets
// database/sql skip Prepare entirely and hand the statement over in one
// call. A conn with only Prepare would work too, at the cost of a Stmt type
// whose sole job is to carry the query text to the same place.
type queryConn struct{ db *queryDB }

type queryRows struct {
	cols []string
	rows [][]driver.Value
	i    int
}

// A fixture in the shape every repository uses: short names a request may
// ask for, mapped to the SQL each one emits. unit_name comes from a JOIN,
// and key is a second name for the primary-key expression.
var (
	testAllowedCols = map[string]string{
		"id":        "t.id",
		"key":       "t.id",
		"code":      "t.code",
		"title":     "t.title",
		"unit_name": "u.name AS unit_name",
	}
	testDefaultCols = []string{"id", "code", "title", "unit_name"}
	testJoinCols    = []string{"unit_name"}
	testSortCols    = []string{"t.code", "t.title", "u.name"}
)

// queryDB is a driver that answers every query with the same canned result
// and records what it was asked.
//
// It is a second fake rather than a reuse of tx_test.go's, which serves
// begin, commit and rollback and refuses statements outright. The two want
// opposite things from a connection, and one fake doing both would make
// each set of tests read around the other's scaffolding.
//
// Recording the query TEXT is half the point. These three helpers hand
// their SQL to BindNamed before it reaches a driver, and what that rewrites
// — the placeholders, and in CountQuery's case the wrapper around the whole
// statement — is otherwise invisible until a server rejects it.
type queryDB struct {
	mu   sync.Mutex
	cols []string
	rows [][]driver.Value
	err  error
	seen []seenQuery
}

// seenQuery is one query as the driver received it, after BindNamed.
type seenQuery struct {
	query string
	args  []driver.NamedValue
}

// oneRow is the shape a single-row lookup scans into.
type oneRow struct {
	ID   int    `db:"id"`
	Code string `db:"code"`
}

// selected joins expressions the way SelectClause does, so the cases below
// read as column lists rather than as escaped strings.
func selected(exprs ...string) string {
	return strings.Join(exprs, ",\n       ")
}

// open returns a *sqlx.DB backed by this fake, bound as driverName.
//
// driverName never reaches a registry: sql.OpenDB takes the connector
// directly, so no mysql or sqlserver driver is involved and the name's only
// job is to pick the placeholder style through sqlx.BindType. That is
// exactly the coupling doc.go describes, which is why the tests below run
// under both names.
func (q *queryDB) open(t *testing.T, driverName string) *sqlx.DB {
	t.Helper()
	db := sqlx.NewDb(sql.OpenDB(&queryConnector{db: q}), driverName)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// lastQuery returns the most recent query, or a zero value if none ran.
func (q *queryDB) lastQuery() seenQuery {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.seen) == 0 {
		return seenQuery{}
	}
	return q.seen[len(q.seen)-1]
}

// count returns how many queries reached the driver.
func (q *queryDB) count() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.seen)
}

func (c *queryConnector) Driver() driver.Driver { return queryDriver{} }

func (r *queryRows) Columns() []string { return r.cols }

func (c *queryConnector) Connect(context.Context) (driver.Conn, error) {
	return &queryConn{db: c.db}, nil
}

func (queryDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New(
		"fake: this driver is only reachable through its connector")
}

func (c *queryConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("fake: prepare is not supported")
}

func (c *queryConn) Begin() (driver.Tx, error) {
	return nil, errors.New("fake: transactions are not supported")
}

func (c *queryConn) QueryContext(
	_ context.Context,
	query string,
	args []driver.NamedValue,
) (driver.Rows, error) {
	q := c.db
	q.mu.Lock()
	q.seen = append(q.seen, seenQuery{query: query, args: args})
	cols, rows, err := q.cols, q.rows, q.err
	q.mu.Unlock()

	if err != nil {
		return nil, err
	}
	return &queryRows{cols: cols, rows: rows}, nil
}

func (r *queryRows) Next(dest []driver.Value) error {
	if r.i >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.i])
	r.i++
	return nil
}

func (r *queryRows) Close() error { return nil }

func (c *queryConn) Close() error { return nil }

func TestSelectClause(t *testing.T) {
	all := selected("t.id", "t.code", "t.title", "u.name AS unit_name")
	cases := []struct {
		name string
		cols []string
		want string
	}{
		{"no request takes the defaults", nil, all},
		{"empty request takes the defaults", []string{}, all},
		{"a subset keeps the requested order", []string{"title", "code"},
			selected("t.id", "t.title", "t.code")},
		{"unknown names are dropped", []string{"title", "bogus"},
			selected("t.id", "t.title")},
		{"all unknown falls back to the defaults", []string{"bogus"}, all},
		{"the key is not repeated", []string{"id", "code"},
			selected("t.id", "t.code")},
		{"one expression appears once", []string{"key", "id", "code"},
			selected("t.id", "t.code")},
		{"a repeated name appears once", []string{"code", "code"},
			selected("t.id", "t.code")},
	}
	for _, c := range cases {
		got := SelectClause(c.cols, testAllowedCols, testDefaultCols, "id")
		if got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
}

// The panic can only follow from arguments that disagree, all of them
// written in code, so it is the loud end of a misconfiguration.
func TestSelectClausePanicsWhenNothingResolves(t *testing.T) {
	allowed := map[string]string{"code": "t.code"}
	mustPanic(t, "selectClause with unknown defaults and pk", func() {
		SelectClause([]string{"bogus"}, allowed, []string{"missing"}, "gone")
	})
}

func TestNeedsJoin(t *testing.T) {
	cases := map[string]struct {
		cols []string
		want bool
	}{
		"defaults include the join": {nil, true},
		"only a local column":       {[]string{"code"}, false},
		"the joined column":         {[]string{"unit_name"}, true},
		"local plus joined":         {[]string{"code", "unit_name"}, true},
		"unknown falls back":        {[]string{"bogus"}, true},
		"the key alone":             {[]string{"key"}, false},
	}
	for name, c := range cases {
		got := NeedsJoin(c.cols, testAllowedCols, testDefaultCols,
			testJoinCols)
		if got != c.want {
			t.Errorf("%s: needsJoin(%v) = %v, want %v",
				name, c.cols, got, c.want)
		}
	}
}

// The property that keeps a query valid: the JOIN is emitted exactly when
// the select list names one of its columns. Both helpers resolve the request
// the same way, so no request can satisfy one and not the other.
func TestSelectClauseAndNeedsJoinAgree(t *testing.T) {
	requests := [][]string{
		nil,
		{},
		{"code"},
		{"unit_name"},
		{"code", "unit_name"},
		{"bogus"},
		{"bogus", "code"},
		{"key"},
	}
	for _, cols := range requests {
		list := SelectClause(cols, testAllowedCols, testDefaultCols, "id")
		selectsJoined := strings.Contains(list, "u.name")
		if joined := NeedsJoin(cols, testAllowedCols, testDefaultCols,
			testJoinCols); joined != selectsJoined {
			t.Errorf("cols %v: needsJoin = %v but select list %q",
				cols, joined, list)
		}
	}
}

func TestActiveCols(t *testing.T) {
	cases := []struct {
		name string
		cols []string
		want []string
	}{
		{"empty takes the defaults", nil, testDefaultCols},
		{"known names are kept in order", []string{"title", "code"},
			[]string{"title", "code"}},
		{"unknown names are dropped", []string{"title", "bogus"},
			[]string{"title"}},
		{"a repeat is dropped", []string{"code", "code"},
			[]string{"code"}},
		{"all unknown takes the defaults", []string{"bogus"},
			testDefaultCols},
	}
	for _, c := range cases {
		got := activeCols(c.cols, testDefaultCols, testAllowedCols)
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("%s: activeCols(%v) = %v, want %v",
				c.name, c.cols, got, c.want)
		}
	}
}

func TestSortClause(t *testing.T) {
	const fallback = "t.id DESC"
	cases := []struct {
		name   string
		fields []SortField
		want   string
	}{
		{"no fields uses the fallback", nil, "\nORDER BY " + fallback},
		{"ascending by default",
			[]SortField{{Col: "t.code", Dir: ""}},
			"\nORDER BY t.code ASC"},
		{"an unexpected direction sorts ascending",
			[]SortField{{Col: "t.code", Dir: "sideways"}},
			"\nORDER BY t.code ASC"},
		{"descending in any letter case",
			[]SortField{{Col: "t.code", Dir: "DeSc"}},
			"\nORDER BY t.code DESC"},
		{"several fields keep their order",
			[]SortField{
				{Col: "u.name", Dir: "desc"},
				{Col: "t.title", Dir: "asc"},
			},
			"\nORDER BY u.name DESC, t.title ASC"},
		{"unknown columns are dropped",
			[]SortField{
				{Col: "t.secret", Dir: "desc"},
				{Col: "t.code", Dir: "desc"},
			},
			"\nORDER BY t.code DESC"},
		{"all unknown uses the fallback",
			[]SortField{{Col: "1; DROP TABLE t", Dir: "desc"}},
			"\nORDER BY " + fallback},
		{"a repeated column keeps its first direction",
			[]SortField{
				{Col: "t.code", Dir: "desc"},
				{Col: "t.code", Dir: "asc"},
			},
			"\nORDER BY t.code DESC"},
	}
	for _, c := range cases {
		got := SortClause(c.fields, testSortCols, fallback)
		if got != c.want {
			t.Errorf("%s: sortClause = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestGroupClause(t *testing.T) {
	allowed := []string{"t.code", "t.unit_id"}
	cases := map[string]string{
		"t.code":    "\nGROUP BY t.code",
		"t.unit_id": "\nGROUP BY t.unit_id",
		"":          "",
		"code":      "", // not spelled as the allowlist spells it
		"t.secret":  "",
	}
	for col, want := range cases {
		if got := GroupClause(col, allowed); got != want {
			t.Errorf("groupClause(%q) = %q, want %q", col, got, want)
		}
	}
}

func TestExistsFlag(t *testing.T) {
	got := ExistsFlag("SELECT 1 FROM child c WHERE c.parent_id = t.id",
		"has_children")
	want := "CASE WHEN EXISTS (SELECT 1 FROM child c WHERE " +
		"c.parent_id = t.id) THEN 1 ELSE 0 END AS has_children"
	if got != want {
		t.Errorf("existsFlag =\n %q\nwant %q", got, want)
	}
}

func TestAsText(t *testing.T) {
	if got, want := AsText("t.amount"),
		"CAST(t.amount AS CHAR(20))"; got != want {
		t.Errorf("asText = %q, want %q", got, want)
	}
}

// The centrepiece of the Read contract in doc.go: a missing row is (false,
// nil), never sql.ErrNoRows. Callers test the bool, so a regression here
// turns every empty lookup into a 500 rather than a 404.
func TestGetOneReportsAMissingRowAsFalse(t *testing.T) {
	q := &queryDB{cols: []string{"id", "code"}}
	db := q.open(t, "mysql")

	dest := oneRow{ID: 7, Code: "untouched"}
	found, err := GetOne(context.Background(), db,
		"SELECT t.id, t.code FROM t WHERE t.id = :id",
		map[string]any{"id": 1}, &dest)

	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if found {
		t.Error("found = true, want false")
	}
	// sqlx leaves dest alone when there is no row, which is what lets a
	// caller reuse a zero value it has already built.
	if dest.ID != 7 || dest.Code != "untouched" {
		t.Errorf("dest = %+v, want it left alone", dest)
	}
}

func TestGetOneScansASingleRow(t *testing.T) {
	q := &queryDB{
		cols: []string{"id", "code"},
		rows: [][]driver.Value{{int64(42), "AB-1"}},
	}
	db := q.open(t, "mysql")

	var dest oneRow
	found, err := GetOne(context.Background(), db,
		"SELECT t.id, t.code FROM t WHERE t.id = :id",
		map[string]any{"id": 42}, &dest)

	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !found {
		t.Fatal("found = false, want true")
	}
	if dest.ID != 42 || dest.Code != "AB-1" {
		t.Errorf("dest = %+v, want {42 AB-1}", dest)
	}
}

// Only sql.ErrNoRows is normalised. Anything else is a real failure and has
// to reach the caller, or a dropped connection reads as an empty result.
func TestGetOnePropagatesOtherErrors(t *testing.T) {
	want := errors.New("connection reset")
	q := &queryDB{cols: []string{"id", "code"}, err: want}
	db := q.open(t, "mysql")

	var dest oneRow
	found, err := GetOne(context.Background(), db,
		"SELECT t.id FROM t WHERE t.id = :id",
		map[string]any{"id": 1}, &dest)

	if !errors.Is(err, want) {
		t.Errorf("err = %v, want %v", err, want)
	}
	if found {
		t.Error("found = true, want false")
	}
}

// A name the args map does not carry is caught by BindNamed, before any
// statement is sent. That ordering is the useful part: the failure names the
// placeholder instead of arriving as a driver error about an argument count.
func TestReadHelpersRejectAnUnboundName(t *testing.T) {
	const query = "SELECT t.id, t.code FROM t WHERE t.id = :missing"

	for _, c := range []struct {
		name string
		run  func(db *sqlx.DB) error
	}{
		{"GetOne", func(db *sqlx.DB) error {
			var dest oneRow
			_, err := GetOne(context.Background(), db, query,
				map[string]any{}, &dest)
			return err
		}},
		{"CountQuery", func(db *sqlx.DB) error {
			_, err := CountQuery(context.Background(), db, query,
				map[string]any{})
			return err
		}},
		{"SelectList", func(db *sqlx.DB) error {
			var dest []oneRow
			return SelectList(context.Background(), db, query,
				map[string]any{}, &dest)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			q := &queryDB{cols: []string{"id", "code"}}
			db := q.open(t, "mysql")

			if err := c.run(db); err == nil {
				t.Error("err = nil, want a binding failure")
			}
			if got := q.count(); got != 0 {
				t.Errorf("%d queries reached the driver, want 0", got)
			}
		})
	}
}

// CountQuery's wrapper is documented as a COUNT(*) over a derived table
// aliased sub, and every precondition in its doc comment is stated in terms
// of that exact shape. This is what holds the shape itself in place.
func TestCountQueryWrapsTheQueryInADerivedTable(t *testing.T) {
	q := &queryDB{
		cols: []string{""},
		rows: [][]driver.Value{{int64(25)}},
	}
	db := q.open(t, "mysql")

	const inner = "SELECT t.id\nFROM t\nWHERE t.grp >= :min_grp"
	total, err := CountQuery(context.Background(), db, inner,
		map[string]any{"min_grp": 0})
	if err != nil {
		t.Fatalf("CountQuery: %v", err)
	}
	if total != 25 {
		t.Errorf("total = %d, want 25", total)
	}

	want := "\nSELECT COUNT(*)\nFROM (" +
		strings.Replace(inner, ":min_grp", "?", 1) + ") sub"
	if got := q.lastQuery().query; got != want {
		t.Errorf("driver saw:\n%q\nwant:\n%q", got, want)
	}
}

// "args is not modified here, and keys the text never mentions do no harm"
// — the claim that lets a builder call CountQuery and PageClause with one
// shared map, in either order.
func TestCountQueryIgnoresArgsTheTextDoesNotMention(t *testing.T) {
	q := &queryDB{cols: []string{""}, rows: [][]driver.Value{{int64(3)}}}
	db := q.open(t, "mysql")

	args := map[string]any{"min_grp": 7, "offset": 40, "limit": 20}
	if _, err := CountQuery(context.Background(), db,
		"SELECT t.id FROM t WHERE t.grp >= :min_grp", args); err != nil {
		t.Fatalf("CountQuery: %v", err)
	}

	got := q.lastQuery().args
	if len(got) != 1 {
		t.Fatalf("driver saw %d args, want 1: %v", len(got), got)
	}
	if got[0].Value != int64(7) {
		t.Errorf("arg = %v, want 7", got[0].Value)
	}
	if len(args) != 3 {
		t.Errorf("args map is now %v, want its three keys intact", args)
	}
}

// An empty result leaves a nil slice nil rather than making it empty. The
// response layer depends on knowing which it has, since the two serialise
// as null and [].
func TestSelectListLeavesANilSliceNil(t *testing.T) {
	q := &queryDB{cols: []string{"id", "code"}}
	db := q.open(t, "mysql")

	var dest []oneRow
	if err := SelectList(context.Background(), db,
		"SELECT t.id, t.code FROM t WHERE t.grp = :grp",
		map[string]any{"grp": 1}, &dest); err != nil {
		t.Fatalf("SelectList: %v", err)
	}
	if dest != nil {
		t.Errorf("dest = %v, want nil", dest)
	}
}

// Every one of these helpers binds through ext, so the placeholder style
// follows the driver name rather than anything they spell themselves. doc.go
// makes that coupling load-bearing — it is why buildDSN returns "sqlserver"
// for a configured "mssql" — and this is where it is observable without a
// server.
func TestBindNamedFollowsTheDriverName(t *testing.T) {
	for _, c := range []struct{ driver, want string }{
		{"mysql", "SELECT t.id FROM t WHERE t.id = ?"},
		{"sqlserver", "SELECT t.id FROM t WHERE t.id = @p1"},
	} {
		t.Run(c.driver, func(t *testing.T) {
			q := &queryDB{
				cols: []string{"id", "code"},
				rows: [][]driver.Value{{int64(1), "x"}},
			}
			db := q.open(t, c.driver)

			var dest oneRow
			if _, err := GetOne(context.Background(), db,
				"SELECT t.id FROM t WHERE t.id = :id",
				map[string]any{"id": 1}, &dest); err != nil {
				t.Fatalf("GetOne: %v", err)
			}
			if got := q.lastQuery().query; got != c.want {
				t.Errorf("driver saw %q, want %q", got, c.want)
			}
		})
	}
}
