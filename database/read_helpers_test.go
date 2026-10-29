package database

// Tests for read_helpers.go.
//
// The clause builders inspect strings and open nothing. GetOne,
// CountQuery, SelectList and Insert reach a driver, so they are exercised
// twice: here against the fake at the top of this file, which is what
// makes their contract hold on a plain `go test`, and against real
// servers, in read_helpers_integration_test.go and, for Insert, in the
// transaction suite, which is what shows the SQL they build is accepted.

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jmoiron/sqlx"
	"github.com/mas-ony/go-toolkit/internal/testutil"
	"github.com/mas-ony/go-toolkit/internal/testutil/sqltest"
)

// queryDB is a driver that answers every query with the same canned result
// and every other statement, which only Insert sends, with one row
// written, and records what it was asked.
//
// It is a second fake rather than a reuse of tx_test.go's, which serves
// begin, commit and rollback and refuses statements outright. The two want
// opposite things from a connection, and one fake doing both would make
// each set of tests read around the other's scaffolding. What they do
// share is what neither uses, a connector's Driver and a conn's Prepare
// and Close, and the way they are opened, all of which are sqltest's. The
// Log the statements are kept in is sqltest's as well, the one crud's
// query fake keeps its statements in.
//
// Recording the query TEXT is half the point. The helpers that reach a
// driver hand their SQL to BindNamed before it does, and what that
// rewrites — the placeholders, and in CountQuery's case the wrapper
// around the whole statement — is otherwise invisible until a server
// rejects it.
type queryDB struct {
	sqltest.Log

	mu   sync.Mutex
	cols []string
	rows [][]driver.Value
	err  error
}

// queryConnector hands out connections to one queryDB, so that sql.OpenDB
// can use the fake without registering a driver name.
type queryConnector struct {
	sqltest.ConnectorBase
	db *queryDB
}

// queryConn implements driver.QueryerContext and driver.ExecerContext,
// which is what lets database/sql skip Prepare entirely and hand the
// statement over in one call, so sqltest's refusing Prepare is never
// reached. A conn with only Prepare would work too, at the cost of a Stmt
// type whose sole job is to carry the query text to the same place. No
// helper here starts a transaction, so the Begin is sqltest's refusing
// one as well.
type queryConn struct {
	sqltest.ConnBase
	db *queryDB
}

// oneRow is the shape a single-row lookup scans into.
type oneRow struct {
	ID   int    `db:"id"`
	Code string `db:"code"`
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

// open returns a *sqlx.DB backed by this fake, bound as driverName.
//
// driverName never reaches a registry: sqltest.Open hands sql.OpenDB the
// connector directly, so no mysql or sqlserver driver is involved and the
// name's only job is to pick the placeholder style through sqlx.BindType.
// That is exactly the coupling buildDSN's doc comment describes, which is
// why the tests below run under both names.
func (q *queryDB) open(t *testing.T, driverName string) *sqlx.DB {
	t.Helper()
	return sqltest.Open(t, &queryConnector{db: q}, driverName)
}

// QueryContext records the query as the driver received it and answers
// with the canned result, or the canned error.
func (c *queryConn) QueryContext(
	_ context.Context,
	query string,
	args []driver.NamedValue,
) (driver.Rows, error) {
	q := c.db
	q.Record(query, args)
	q.mu.Lock()
	cols, rows, err := q.cols, q.rows, q.err
	q.mu.Unlock()

	if err != nil {
		return nil, err
	}
	return &sqltest.Rows{Cols: cols, Values: rows}, nil
}

// ExecContext records the statement as the driver received it and
// reports one row written, or the canned error.
func (c *queryConn) ExecContext(
	_ context.Context,
	query string,
	args []driver.NamedValue,
) (driver.Result, error) {
	q := c.db
	q.Record(query, args)
	q.mu.Lock()
	err := q.err
	q.mu.Unlock()

	if err != nil {
		return nil, err
	}
	return driver.RowsAffected(1), nil
}

// Connect opens a connection onto the shared queryDB.
func (c *queryConnector) Connect(context.Context) (driver.Conn, error) {
	return &queryConn{db: c.db}, nil
}

// SelectClause resolves a request the way activeCols does, emits each
// expression once, and puts the key first when the request left it out.
func TestSelectClause(t *testing.T) {
	all := sqltest.Selected("t.id", "t.code", "t.title", "u.name AS unit_name")
	cases := []struct {
		name string
		cols []string
		want string
	}{
		{"no request takes the defaults", nil, all},
		{"empty request takes the defaults", []string{}, all},
		{"a subset keeps the requested order", []string{"title", "code"},
			sqltest.Selected("t.id", "t.title", "t.code")},
		{"unknown names are dropped", []string{"title", "bogus"},
			sqltest.Selected("t.id", "t.title")},
		{"all unknown falls back to the defaults", []string{"bogus"}, all},
		{"the key is not repeated", []string{"id", "code"},
			sqltest.Selected("t.id", "t.code")},
		{"one expression appears once", []string{"key", "id", "code"},
			sqltest.Selected("t.id", "t.code")},
		{"a repeated name appears once", []string{"code", "code"},
			sqltest.Selected("t.id", "t.code")},
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
	testutil.MustPanic(t, "SelectClause with unknown defaults and pk", func() {
		SelectClause([]string{"bogus"}, allowed, []string{"missing"}, "gone")
	})
}

// NeedsJoin answers true exactly when the resolved columns include one the
// JOIN supplies, the defaults included.
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
			t.Errorf("%s: NeedsJoin(%v) = %v, want %v",
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
			t.Errorf("cols %v: NeedsJoin = %v but select list %q",
				cols, joined, list)
		}
	}
}

// activeCols keeps known names in order, drops unknown and repeated ones,
// and falls back to the defaults when nothing known is left.
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

// SortClause emits only allowlisted columns, each once, with a normalised
// direction, and falls back rather than emitting an empty ORDER BY.
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
			t.Errorf("%s: SortClause = %q, want %q", c.name, got, c.want)
		}
	}
}

// Direction is the one reading of a direction: desc in any letter case and
// with any surrounding space is descending, and everything else, nothing
// at all included, is ascending.
func TestSortFieldDirection(t *testing.T) {
	for dir, want := range map[string]string{
		"desc":       "DESC",
		"DeSc":       "DESC",
		" desc\t":    "DESC",
		"":           "ASC",
		"asc":        "ASC",
		"sideways":   "ASC",
		"descending": "ASC",
		"de sc":      "ASC",
	} {
		if got := (SortField{Dir: dir}).Direction(); got != want {
			t.Errorf("Direction for %q = %q, want %q", dir, got, want)
		}
	}
}

// GroupClause groups only by a column spelled exactly as the allowlist
// spells it, and otherwise adds nothing.
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
			t.Errorf("GroupClause(%q) = %q, want %q", col, got, want)
		}
	}
}

// ExistsFlag wraps the subquery in the CASE that both engines accept in a
// select list.
func TestExistsFlag(t *testing.T) {
	got := ExistsFlag("SELECT 1 FROM child c WHERE c.parent_id = t.id",
		"has_children")
	want := "CASE WHEN EXISTS (SELECT 1 FROM child c WHERE " +
		"c.parent_id = t.id) THEN 1 ELSE 0 END AS has_children"
	if got != want {
		t.Errorf("ExistsFlag =\n %q\nwant %q", got, want)
	}
}

// AsText casts with the one spelling both engines accept.
func TestAsText(t *testing.T) {
	if got, want := AsText("t.amount"),
		"CAST(t.amount AS CHAR(20))"; got != want {
		t.Errorf("AsText = %q, want %q", got, want)
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

// A row that exists is scanned into dest and reported as found.
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
		{"Insert", func(db *sqlx.DB) error {
			_, err := Insert(context.Background(), db, "t", "id",
				":missing", map[string]any{})
			return err
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			q := &queryDB{cols: []string{"id", "code"}}
			db := q.open(t, "mysql")

			if err := c.run(db); err == nil {
				t.Error("err = nil, want a binding failure")
			}
			if got := len(q.Statements()); got != 0 {
				t.Errorf("%d statements reached the driver, want 0",
					got)
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
	if got := q.Last().Query; got != want {
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

	got := q.Last().Args
	if len(got) != 1 {
		t.Fatalf("driver saw %d args, want 1: %v", len(got), got)
	}
	if got[0] != int64(7) {
		t.Errorf("arg = %v, want 7", got[0])
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

// Insert hands the driver the plain INSERT, with the placeholders of the
// driver name the pool was opened under and the values in column order,
// and returns what the driver reported, or the driver's error.
func TestInsert(t *testing.T) {
	ctx := context.Background()
	arg := map[string]any{"code": "A-1", "title": "first"}
	for _, c := range []struct{ driver, want string }{
		{"mysql", "INSERT INTO app.t (code, title)\nVALUES (?, ?)"},
		{"sqlserver", "INSERT INTO app.t (code, title)" +
			"\nVALUES (@p1, @p2)"},
	} {
		t.Run(c.driver, func(t *testing.T) {
			q := &queryDB{}
			db := q.open(t, c.driver)

			res, err := Insert(ctx, db, "app.t", "code, title",
				":code, :title", arg)
			if err != nil {
				t.Fatalf("Insert: %v", err)
			}
			if n, err := res.RowsAffected(); err != nil || n != 1 {
				t.Errorf("RowsAffected = %d, %v; want 1", n, err)
			}
			got := q.Last()
			if got.Query != c.want {
				t.Errorf("driver saw %q, want %q", got.Query, c.want)
			}
			if len(got.Args) != 2 || got.Args[0] != "A-1" ||
				got.Args[1] != "first" {
				t.Errorf("args = %v, want [A-1 first]", got.Args)
			}
		})
	}

	boom := errors.New("boom")
	q := &queryDB{err: boom}
	_, err := Insert(ctx, q.open(t, "mysql"), "app.t", "code",
		":code", arg)
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want the driver's error", err)
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
			if got := q.Last().Query; got != c.want {
				t.Errorf("driver saw %q, want %q", got, c.want)
			}
		})
	}
}
