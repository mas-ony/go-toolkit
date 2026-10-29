package crud

// Tests for repo.go and table.go, against a fake driver defined below
// that records every statement as the driver receives it.
//
// The fake sits at the driver rather than at Ext because the placeholders
// a statement ends up with exist only past sqlx's binding, and a
// statement's text is all a string-building method can be held to
// without a server. Every statement is therefore asserted in both
// grammars, under the driver name that picks its placeholder style.
//
// What the file promises: the statements BaseRepo derives from a
// TableSpec, reserved names quoted and plain ones bare; the three Fetch
// modes and the refusal of a prefixed page; sql.ErrNoRows for a write
// that matched nothing and the "<table>.<Method>" form of every other
// error; the With* copies; and the panics of a TableSpec that describes
// its key inconsistently.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/jmoiron/sqlx"
	"github.com/mas-ony/go-toolkit/database"
	"github.com/mas-ony/go-toolkit/internal/testutil"
	"github.com/mas-ony/go-toolkit/internal/testutil/sqltest"
)

// fakeDB answers every statement from canned results and keeps it in its
// Log.
//
// A query that counts or that returns a generated key gets one column
// holding count or lastID, so CountQuery and the SQL Server form of
// InsertReturningID can scan it; every other query gets cols and rows.
type fakeDB struct {
	sqltest.Log

	mu   sync.Mutex
	cols []string
	rows [][]driver.Value

	count    int64
	lastID   int64
	affected int64
	// noCount makes RowsAffected fail, as a driver that cannot count.
	noCount bool
	// err fails every statement.
	err error
}

// fakeConnector hands out connections to one fakeDB, so that sql.OpenDB
// can use the fake without registering a driver name. Its Driver is
// sqltest's.
type fakeConnector struct {
	sqltest.ConnectorBase
	db *fakeDB
}

// fakeConn implements QueryerContext and ExecerContext, which lets
// database/sql hand each statement over in one call, so Prepare is never
// reached. The Prepare and Close it never uses are sqltest's.
type fakeConn struct {
	sqltest.ConnBase
	db *fakeDB
}

// fakeTx commits and rolls back nothing; the statements inside it reach
// the same fakeDB as any other.
type fakeTx struct{}

// fakeResult is what every Exec returns.
type fakeResult struct {
	affected int64
	lastID   int64
	noCount  bool
}

// item is a row of a table with a generated key.
type item struct {
	ID    *int    `db:"id"`
	Code  *string `db:"code"`
	Title *string `db:"title"`
}

// itemReadOption is the read option of itemRepository.
type itemReadOption struct {
	ReadOption[int]

	// Code is an exact match on code.
	Code *string
}

// itemRepo is the interface a repository over item exposes, in the shape
// a service declares one.
type itemRepo interface {
	Scoped[itemRepo]
	Create(ctx context.Context, m *item) (int, error)
	Read(ctx context.Context, opt itemReadOption) (ReadResult[item], error)
	Update(ctx context.Context, m *item) error
	Patch(ctx context.Context, id int, fields map[string]any) error
	Delete(ctx context.Context, id int) error
}

// itemRepository is a repository as a service writes one: a BaseRepo
// embedded by value, and a Read of its own.
type itemRepository struct {
	BaseRepo[item, int, itemRepo]
}

// holiday is a row of a table keyed by a natural string key.
type holiday struct {
	Code *string `db:"code"`
	Note *string `db:"note"`
}

// ranked is a row of a table whose name, key and columns are all reserved
// words on at least one engine.
type ranked struct {
	Key  *int    `db:"key"`
	Rank *int    `db:"rank"`
	User *string `db:"user"`
}

// errFake is the failure a fakeDB with err set answers with.
var errFake = errors.New("fake: connection reset")

var itemSpec = TableSpec[item, int]{
	Name:  "item",
	Alias: "i",
	Key:   "id",
	Cols: map[string]string{
		"id":    "i.id",
		"code":  "i.code",
		"title": "i.title",
	},
	DefaultCols: []string{"id", "code", "title"},
	Writable:    []string{"code", "title"},
	Updatable:   []string{"title"},
	Patchable:   []string{"code", "title"},
	SearchCols:  []string{"i.code", "i.title"},
	SortCols:    []string{"i.code", "i.title"},
	DefaultSort: "i.id",
}

var holidaySpec = TableSpec[holiday, string]{
	Name:  "holiday",
	Alias: "h",
	Key:   "code",
	KeyOf: func(m *holiday) (string, bool) {
		if m.Code == nil {
			return "", false
		}
		return *m.Code, true
	},
	Cols: map[string]string{
		"code": "h.code",
		"note": "h.note",
	},
	DefaultCols: []string{"code", "note"},
	Writable:    []string{"code", "note"},
	Updatable:   []string{"note"},
	DefaultSort: "h.code",
}

// The interface checks a service relies on: the promoted methods and a
// Read of the repository's own make up itemRepo, and a repository keyed
// by an int is a Repository for the service layer.
var (
	_ itemRepo                         = (*itemRepository)(nil)
	_ Repository[item, itemReadOption] = (*itemRepository)(nil)
)

// wrapItem is the function NewBaseRepo is handed for itemRepository.
func wrapItem(b BaseRepo[item, int, itemRepo]) itemRepo {
	return &itemRepository{b}
}

// newItemRepo returns an itemRepository over db.
func newItemRepo(db *sqlx.DB) *itemRepository {
	return &itemRepository{NewBaseRepo(db, &itemSpec, wrapItem)}
}

// newHolidayRepo returns a bare BaseRepo over holidaySpec.
func newHolidayRepo(db *sqlx.DB) BaseRepo[holiday, string, any] {
	return NewBaseRepo(db, &holidaySpec,
		func(b BaseRepo[holiday, string, any]) any { return b })
}

// rankedSpec describes the table named order, keyed by key, which writes
// rank and user. Its SELECT list quotes the key itself, as a repository
// with such a column has to, so it is built after the grammar is
// selected.
func rankedSpec() *TableSpec[ranked, int] {
	return &TableSpec[ranked, int]{
		Name:  "order",
		Alias: "o",
		Key:   "key",
		Cols: map[string]string{
			"key": "o." + database.QuoteIdent("key"),
		},
		DefaultCols: []string{"key"},
		Writable:    []string{"rank", "user"},
		Patchable:   []string{"rank"},
		DefaultSort: "o." + database.QuoteIdent("key"),
	}
}

// newRankedRepo returns a bare BaseRepo over rankedSpec.
func newRankedRepo(db *sqlx.DB) BaseRepo[ranked, int, any] {
	return NewBaseRepo(db, rankedSpec(),
		func(b BaseRepo[ranked, int, any]) any { return b })
}

// selectGrammar selects the grammar driverName stands for. Both ways a
// test here opens a database start with it: open on a fakeDB, and the
// open in repo_integration_test.go that connects to a server.
func selectGrammar(t *testing.T, driverName string) {
	t.Helper()
	d, err := database.ParseDialect(driverName)
	if err != nil {
		t.Fatal(err)
	}
	database.SetDialect(d)
}

// grammars runs fn once per driver name, as a subtest named after it.
func grammars(t *testing.T, fn func(t *testing.T, driverName string)) {
	t.Helper()
	for _, name := range []string{"mysql", "sqlserver"} {
		t.Run(name, func(t *testing.T) { fn(t, name) })
	}
}

// pick returns mysql or sqlserver, whichever driverName stands for.
func pick(driverName, mysql, sqlserver string) string {
	if driverName == "mysql" {
		return mysql
	}
	return sqlserver
}

// wantStatements fails t unless f received exactly want, in order.
func wantStatements(t *testing.T, f *fakeDB, want ...sqltest.Statement) {
	t.Helper()
	got := f.Statements()
	if len(got) != len(want) {
		t.Fatalf("driver saw %d statements, want %d: %q",
			len(got), len(want), got)
	}
	for i := range want {
		if got[i].Query != want[i].Query {
			t.Errorf("statement %d:\ngot  %q\nwant %q",
				i, got[i].Query, want[i].Query)
		}
		if !reflect.DeepEqual(got[i].Args, want[i].Args) {
			t.Errorf("statement %d args = %#v, want %#v",
				i, got[i].Args, want[i].Args)
		}
	}
}

// open selects the grammar driverName stands for and returns a *sqlx.DB
// over f bound as driverName, closed when the test ends.
//
// The grammar is process-wide, so no test in this package runs in
// parallel; each one that builds a statement selects the grammar it
// asserts first.
func (f *fakeDB) open(t *testing.T, driverName string) *sqlx.DB {
	t.Helper()
	selectGrammar(t, driverName)
	return sqltest.Open(t, fakeConnector{db: f}, driverName)
}

// Connect opens a connection onto the shared fakeDB.
func (c fakeConnector) Connect(context.Context) (driver.Conn, error) {
	return fakeConn{db: c.db}, nil
}

// Begin starts a transaction that does nothing.
func (fakeConn) Begin() (driver.Tx, error) { return fakeTx{}, nil }

// QueryContext records the query and answers it.
func (c fakeConn) QueryContext(
	_ context.Context,
	query string,
	args []driver.NamedValue,
) (driver.Rows, error) {
	f := c.db
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Record(query, args)
	if f.err != nil {
		return nil, f.err
	}
	switch {
	case strings.Contains(query, "COUNT(*)"):
		return &sqltest.Rows{
			Cols:   []string{"n"},
			Values: [][]driver.Value{{f.count}},
		}, nil
	case strings.Contains(query, "@Inserted"):
		return &sqltest.Rows{
			Cols:   []string{"id"},
			Values: [][]driver.Value{{f.lastID}},
		}, nil
	}
	return &sqltest.Rows{Cols: f.cols, Values: f.rows}, nil
}

// ExecContext records the statement and reports the canned counts.
func (c fakeConn) ExecContext(
	_ context.Context,
	query string,
	args []driver.NamedValue,
) (driver.Result, error) {
	f := c.db
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Record(query, args)
	if f.err != nil {
		return nil, f.err
	}
	return fakeResult{
		affected: f.affected,
		lastID:   f.lastID,
		noCount:  f.noCount,
	}, nil
}

// Commit does nothing.
func (fakeTx) Commit() error { return nil }

// Rollback does nothing.
func (fakeTx) Rollback() error { return nil }

// LastInsertId reports the canned key.
func (r fakeResult) LastInsertId() (int64, error) { return r.lastID, nil }

// RowsAffected reports the canned count, or fails when noCount is set.
func (r fakeResult) RowsAffected() (int64, error) {
	if r.noCount {
		return 0, errors.New("fake: no count")
	}
	return r.affected, nil
}

// Read applies the option the way a repository's own Read does.
func (r *itemRepository) Read(
	ctx context.Context,
	opt itemReadOption,
) (ReadResult[item], error) {
	q, filter := r.Begin(opt.ReadOption)
	if filter {
		AndEq(q, "i.code", "code", opt.Code)
	}
	return r.Fetch(ctx, q, opt.ListOption)
}

// Create writes spec.Writable and returns the generated key: from
// LastInsertId on MySQL, and from the OUTPUT INTO result set on SQL
// Server, whose statement names the key column.
func TestCreateReturnsTheGeneratedKey(t *testing.T) {
	grammars(t, func(t *testing.T, name string) {
		f := &fakeDB{lastID: 7}
		r := newItemRepo(f.open(t, name))

		id, err := r.Create(context.Background(),
			&item{Code: new("A-1"), Title: new("first")})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if id != 7 {
			t.Errorf("id = %d, want 7", id)
		}
		wantStatements(t, f, sqltest.Statement{
			Query: pick(name,
				"INSERT INTO item (\ncode,\ntitle)\nVALUES (\n?,\n?)",
				"DECLARE @Inserted TABLE (id BIGINT);"+
					"\nINSERT INTO item (\ncode,\ntitle)"+
					"\nOUTPUT INSERTED.id INTO @Inserted (id)"+
					"\nVALUES (\n@p1,\n@p2);"+
					"\nSELECT id FROM @Inserted;"),
			Args: []any{"A-1", "first"},
		})
	})
}

// A natural key is read with KeyOf and returned as it is, from a plain
// INSERT; a model without one fails before anything is sent.
func TestCreateWithANaturalKey(t *testing.T) {
	grammars(t, func(t *testing.T, name string) {
		f := &fakeDB{affected: 1}
		r := newHolidayRepo(f.open(t, name))
		ctx := context.Background()

		key, err := r.Create(ctx,
			&holiday{Code: new("2026-08-17"), Note: new("merdeka")})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if key != "2026-08-17" {
			t.Errorf("key = %q, want 2026-08-17", key)
		}
		wantStatements(t, f, sqltest.Statement{
			Query: pick(name,
				"INSERT INTO holiday (\ncode,\nnote)\nVALUES (\n?,\n?)",
				"INSERT INTO holiday (\ncode,\nnote)"+
					"\nVALUES (\n@p1,\n@p2)"),
			Args: []any{"2026-08-17", "merdeka"},
		})

		_, err = r.Create(ctx, &holiday{Note: new("no key")})
		const want = "holiday.Create: model has no code"
		if err == nil || err.Error() != want {
			t.Errorf("err = %v, want %q", err, want)
		}
		if n := len(f.Statements()); n != 1 {
			t.Errorf("a model without its key sent a statement")
		}
	})
}

// Update writes spec.Updatable and binds a generated key from the model,
// after the columns.
func TestUpdateBindsTheGeneratedKeyFromTheModel(t *testing.T) {
	grammars(t, func(t *testing.T, name string) {
		f := &fakeDB{affected: 1}
		r := newItemRepo(f.open(t, name))

		err := r.Update(context.Background(),
			&item{ID: new(7), Code: new("ignored"), Title: new("new")})
		if err != nil {
			t.Fatalf("Update: %v", err)
		}
		wantStatements(t, f, sqltest.Statement{
			Query: pick(name,
				"UPDATE item\nSET title = ?\nWHERE id = ?",
				"UPDATE item\nSET title = @p1\nWHERE id = @p2"),
			Args: []any{"new", int64(7)},
		})
	})
}

// A natural key is bound as KeyOf returns it, through a ? numbered after
// the named placeholders, and a model without one matches no row without
// a statement being sent.
func TestUpdateBindsANaturalKeyAfterTheColumns(t *testing.T) {
	grammars(t, func(t *testing.T, name string) {
		f := &fakeDB{affected: 1}
		r := newHolidayRepo(f.open(t, name))
		ctx := context.Background()

		err := r.Update(ctx,
			&holiday{Code: new("2026-08-17"), Note: new("updated")})
		if err != nil {
			t.Fatalf("Update: %v", err)
		}
		wantStatements(t, f, sqltest.Statement{
			Query: pick(name,
				"UPDATE holiday\nSET note = ?\nWHERE code = ?",
				"UPDATE holiday\nSET note = @p1\nWHERE code = @p2"),
			Args: []any{"updated", "2026-08-17"},
		})

		err = r.Update(ctx, &holiday{Note: new("no key")})
		if err != sql.ErrNoRows {
			t.Errorf("err = %v, want sql.ErrNoRows", err)
		}
		if n := len(f.Statements()); n != 1 {
			t.Errorf("a model without its key sent a statement")
		}
	})
}

// Patch writes the named columns in sorted order with the key last, so
// the same fields build the same statement whatever order the map yields
// them in, and refuses a column outside Patchable, the key included,
// before anything is sent. An empty map is no statement at all.
func TestPatch(t *testing.T) {
	grammars(t, func(t *testing.T, name string) {
		f := &fakeDB{affected: 1}
		r := newItemRepo(f.open(t, name))
		ctx := context.Background()

		if err := r.Patch(ctx, 7, nil); err != nil {
			t.Fatalf("Patch(nil): %v", err)
		}
		const runs = 20
		fields := map[string]any{"title": "t", "code": "c"}
		want := make([]sqltest.Statement, runs)
		for i := range runs {
			if err := r.Patch(ctx, 7, fields); err != nil {
				t.Fatalf("Patch: %v", err)
			}
			want[i] = sqltest.Statement{
				Query: "UPDATE item" +
					"\nSET code = " + pick(name, "?", "@p1") +
					",\n    title = " + pick(name, "?", "@p2") +
					"\nWHERE id = " + pick(name, "?", "@p3"),
				Args: []any{"c", "t", int64(7)},
			}
		}
		wantStatements(t, f, want...)

		err := r.Patch(ctx, 7, map[string]any{"title": "x", "id": 8})
		const refused = `item.Patch: column "id" is not patchable`
		if err == nil || err.Error() != refused {
			t.Errorf("err = %v, want %q", err, refused)
		}
		if n := len(f.Statements()); n != runs {
			t.Errorf("a refused patch sent a statement")
		}
	})
}

// Delete addresses the key and reports a missing row as sql.ErrNoRows,
// unwrapped; DeleteWhere without requireOne does not.
func TestDeleteReportsAMissingRow(t *testing.T) {
	grammars(t, func(t *testing.T, name string) {
		f := &fakeDB{}
		r := newItemRepo(f.open(t, name))
		ctx := context.Background()

		if err := r.Delete(ctx, 7); err != sql.ErrNoRows {
			t.Errorf("Delete = %v, want sql.ErrNoRows", err)
		}
		err := r.DeleteWhere(ctx, "DeleteByCode", "code", "A-1", false)
		if err != nil {
			t.Errorf("DeleteWhere = %v, want nil", err)
		}
		wantStatements(t, f,
			sqltest.Statement{
				Query: pick(name,
					"DELETE FROM item\nWHERE id = ?",
					"DELETE FROM item\nWHERE id = @p1"),
				Args: []any{int64(7)},
			},
			sqltest.Statement{
				Query: pick(name,
					"DELETE FROM item\nWHERE code = ?",
					"DELETE FROM item\nWHERE code = @p1"),
				Args: []any{"A-1"},
			})
	})
}

// A table, a key and columns named after reserved words are quoted in
// every statement BaseRepo writes, in the selected grammar.
func TestReservedNamesAreQuoted(t *testing.T) {
	grammars(t, func(t *testing.T, name string) {
		f := &fakeDB{lastID: 3, affected: 1}
		r := newRankedRepo(f.open(t, name))
		ctx := context.Background()
		q := func(s string) string { return database.QuoteIdent(s) }

		if _, err := r.Create(ctx,
			&ranked{Rank: new(1), User: new("u")}); err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := r.Update(ctx, &ranked{
			Key: new(3), Rank: new(2), User: new("v"),
		}); err != nil {
			t.Fatalf("Update: %v", err)
		}
		err := r.Patch(ctx, 3, map[string]any{"rank": 4})
		if err != nil {
			t.Fatalf("Patch: %v", err)
		}
		if err := r.Delete(ctx, 3); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		lookup, _ := r.Begin(ReadOption[int]{ID: new(3)})
		if _, err := r.Fetch(ctx, lookup, ListOption{}); err != nil {
			t.Fatalf("Fetch: %v", err)
		}

		p := func(i int) string {
			return pick(name, "?", "@p"+string(rune('0'+i)))
		}
		table, key := q("order"), q("key")
		cols := "\n" + q("rank") + ",\n" + q("user")
		insert := "INSERT INTO " + table + " (" + cols + ")"
		values := "\nVALUES (\n" + p(1) + ",\n" + p(2) + ")"
		wantStatements(t, f,
			sqltest.Statement{
				Query: pick(name, insert+values,
					"DECLARE @Inserted TABLE (id BIGINT);\n"+insert+
						"\nOUTPUT INSERTED."+key+
						" INTO @Inserted (id)"+values+";"+
						"\nSELECT id FROM @Inserted;"),
				Args: []any{int64(1), "u"},
			},
			sqltest.Statement{
				Query: "UPDATE " + table +
					"\nSET " + q("rank") + " = " + p(1) +
					",\n    " + q("user") + " = " + p(2) +
					"\nWHERE " + key + " = " + p(3),
				Args: []any{int64(2), "v", int64(3)},
			},
			sqltest.Statement{
				Query: "UPDATE " + table +
					"\nSET " + q("rank") + " = " + p(1) +
					"\nWHERE " + key + " = " + p(2),
				Args: []any{int64(4), int64(3)},
			},
			sqltest.Statement{
				Query: "DELETE FROM " + table +
					"\nWHERE " + key + " = " + p(1),
				Args: []any{int64(3)},
			},
			sqltest.Statement{
				Query: pick(name,
					"SELECT o."+key+"\nFROM "+table+" AS o"+
						"\nWHERE (1=1)\n  AND (o."+key+" = ?)"+
						"\nLIMIT 1",
					"SELECT TOP (1) o."+key+"\nFROM "+table+" AS o"+
						"\nWHERE (1=1)\n  AND (o."+key+" = @p1)"),
				Args: []any{int64(3)},
			})
	})
}

// RequireOneRow is the one reading of a write's count: none matched is
// sql.ErrNoRows, any other count is success, and a driver that cannot
// count is taken at its word that the statement ran.
func TestRequireOneRow(t *testing.T) {
	for _, c := range []struct {
		res  fakeResult
		want error
	}{
		{fakeResult{affected: 0}, sql.ErrNoRows},
		{fakeResult{affected: 1}, nil},
		{fakeResult{affected: 2}, nil},
		{fakeResult{noCount: true}, nil},
	} {
		if got := RequireOneRow(c.res); got != c.want {
			t.Errorf("RequireOneRow(%+v) = %v, want %v",
				c.res, got, c.want)
		}
	}
}

// A lookup by key is capped at one row in the grammar's own way, and a
// row that is not there is an empty result rather than an error.
func TestFetchALookup(t *testing.T) {
	grammars(t, func(t *testing.T, name string) {
		f := &fakeDB{
			cols: []string{"id", "code", "title"},
			rows: [][]driver.Value{{int64(7), "A-1", "first"}},
		}
		r := newItemRepo(f.open(t, name))
		ctx := context.Background()

		res, err := r.Read(ctx,
			itemReadOption{ReadOption: ReadOption[int]{ID: new(7)}})
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if res.Total != 1 || len(res.Items) != 1 ||
			*res.Items[0].Code != "A-1" {
			t.Errorf("Read = %+v, want the one row", res)
		}
		list := sqltest.Selected("i.id", "i.code", "i.title")
		wantStatements(t, f, sqltest.Statement{
			Query: pick(name,
				"SELECT "+list+"\nFROM item AS i"+
					"\nWHERE (1=1)\n  AND (i.id = ?)\nLIMIT 1",
				"SELECT TOP (1) "+list+"\nFROM item AS i"+
					"\nWHERE (1=1)\n  AND (i.id = @p1)"),
			Args: []any{int64(7)},
		})

		f.rows = nil
		res, err = r.Read(ctx,
			itemReadOption{ReadOption: ReadOption[int]{ID: new(8)}})
		if err != nil || res.Total != 0 || res.Items != nil {
			t.Errorf("missing row: Read = %+v, %v; want empty, nil",
				res, err)
		}
	})
}

// A page counts first, over the query as it stood before its ORDER BY,
// and then reads the page; a plain list reads every row in order. The
// table's own filters apply only when neither ID nor Search is set.
func TestFetchAPageAndAList(t *testing.T) {
	grammars(t, func(t *testing.T, name string) {
		f := &fakeDB{
			count: 12,
			cols:  []string{"id", "code", "title"},
			rows: [][]driver.Value{
				{int64(3), "A-3", "third"},
				{int64(4), "A-4", "fourth"},
			},
		}
		r := newItemRepo(f.open(t, name))
		ctx := context.Background()

		res, err := r.Read(ctx, itemReadOption{
			ReadOption: ReadOption[int]{ListOption: ListOption{
				Cols:  []string{"code"},
				Sort:  []database.SortField{{Col: "i.code", Dir: "desc"}},
				Page:  2,
				Limit: 2,
			}},
			Code: new("A-3"),
		})
		if err != nil {
			t.Fatalf("Read page: %v", err)
		}
		if res.Total != 12 || len(res.Items) != 2 {
			t.Errorf("page: Total %d with %d items, want 12 with 2",
				res.Total, len(res.Items))
		}

		_, err = r.Read(ctx, itemReadOption{
			ReadOption: ReadOption[int]{Search: new("50%")},
			Code:       new("ignored"),
		})
		if err != nil {
			t.Fatalf("Read list: %v", err)
		}

		base := "SELECT " + sqltest.Selected("i.id", "i.code") +
			"\nFROM item AS i\nWHERE (1=1)\n  AND (i.code = " +
			pick(name, "?", "@p1") + ")"
		page := pick(name,
			"\nLIMIT ? OFFSET ?",
			"\nOFFSET @p2 ROWS FETCH NEXT @p3 ROWS ONLY")
		search := "SELECT " + sqltest.Selected("i.id", "i.code", "i.title") +
			"\nFROM item AS i\nWHERE (1=1)" +
			"\n  AND (i.code LIKE " + pick(name, "?", "@p1") +
			" ESCAPE '!'\n    OR i.title LIKE " + pick(name, "?", "@p2") +
			" ESCAPE '!')\nORDER BY i.id"
		pageArgs := []any{"A-3", int64(2), int64(2)}
		wantStatements(t, f,
			sqltest.Statement{
				Query: "\nSELECT COUNT(*)\nFROM (" + base + ") sub",
				Args:  []any{"A-3"},
			},
			sqltest.Statement{
				Query: base + "\nORDER BY i.code DESC" + page,
				Args:  pageArgs,
			},
			sqltest.Statement{
				Query: search,
				Args:  []any{"%50!%%", "%50!%%"},
			})
	})
}

// A prefix opens the statement, and a lookup is capped on the SELECT that
// follows it. A page of a prefixed query is refused before anything is
// sent, because its COUNT would wrap a WITH in a derived table.
func TestFetchAPrefixedQuery(t *testing.T) {
	grammars(t, func(t *testing.T, name string) {
		f := &fakeDB{cols: []string{"id"}}
		r := newItemRepo(f.open(t, name))
		ctx := context.Background()
		const cte = "WITH x AS (SELECT 1 AS id)"

		q := r.Select([]string{"id"}).Prepend(cte).One()
		if _, err := r.Fetch(ctx, q, ListOption{}); err != nil {
			t.Fatalf("lookup: %v", err)
		}
		q = r.Select([]string{"id"}).Prepend("B").Prepend("A")
		if _, err := r.Fetch(ctx, q, ListOption{}); err != nil {
			t.Fatalf("list: %v", err)
		}
		q = r.Select([]string{"id"}).Prepend(cte)
		_, err := r.Fetch(ctx, q, ListOption{Page: 1, Limit: 10})
		const want = "item.Read: cannot paginate a prefixed query"
		if err == nil || err.Error() != want {
			t.Errorf("page: err = %v, want %q", err, want)
		}

		wantStatements(t, f,
			sqltest.Statement{Query: cte + pick(name,
				"\nSELECT i.id\nFROM item AS i\nWHERE (1=1)\nLIMIT 1",
				"\nSELECT TOP (1) i.id\nFROM item AS i\nWHERE (1=1)"),
				Args: []any{},
			},
			sqltest.Statement{
				Query: "A\nB\nSELECT i.id\nFROM item AS i\nWHERE (1=1)" +
					"\nORDER BY i.id",
				Args: []any{},
			})
	})
}

// Every failure comes back as "<table>.<Method>: <cause>", with the cause
// still reachable through errors.Is, and a nil error stays nil.
func TestErrorsNameTheTableAndMethod(t *testing.T) {
	f := &fakeDB{err: errFake}
	db := f.open(t, "mysql")
	r := newItemRepo(db)
	ctx := context.Background()
	read := func(opt ReadOption[int]) error {
		_, err := r.Read(ctx, itemReadOption{ReadOption: opt})
		return err
	}

	_, createErr := r.Create(ctx, &item{Code: new("A"), Title: new("a")})
	_, naturalErr := newHolidayRepo(db).Create(ctx,
		&holiday{Code: new("2026-01-01")})
	page := ListOption{Page: 1, Limit: 5}
	for _, c := range []struct {
		err  error
		want string
	}{
		{createErr, "item.Create: " + errFake.Error()},
		{naturalErr, "holiday.Create: " + errFake.Error()},
		{read(ReadOption[int]{}), "item.Read: " + errFake.Error()},
		{read(ReadOption[int]{ID: new(1)}), "item.Read: " +
			errFake.Error()},
		{read(ReadOption[int]{ListOption: page}), "item.Read: " +
			errFake.Error()},
		{r.Update(ctx, &item{ID: new(1)}), "item.Update: " +
			errFake.Error()},
		{r.Patch(ctx, 1, map[string]any{"title": "x"}), "item.Patch: " +
			errFake.Error()},
		{r.Delete(ctx, 1), "item.Delete: " + errFake.Error()},
		{r.Wrap("Custom", errFake), "item.Custom: " + errFake.Error()},
	} {
		if c.err == nil || c.err.Error() != c.want {
			t.Errorf("err = %v, want %q", c.err, c.want)
		}
		if !errors.Is(c.err, errFake) {
			t.Errorf("%v does not wrap the driver's error", c.err)
		}
	}

	if err := r.Wrap("Custom", nil); err != nil {
		t.Errorf("Wrap(nil) = %v, want nil", err)
	}
	if _, err := r.Create(ctx, nil); err == nil ||
		err.Error() != "item.Create: nil model" {
		t.Errorf("Create(nil) = %v", err)
	}
	if err := r.Update(ctx, nil); err == nil ||
		err.Error() != "item.Update: nil model" {
		t.Errorf("Update(nil) = %v", err)
	}
}

// Each With* method returns a copy with one field changed, as the
// repository's own interface, so they chain in any order and leave the
// receiver as it was.
func TestWithReturnsScopedCopies(t *testing.T) {
	f := &fakeDB{}
	db := f.open(t, "sqlserver")
	r := newItemRepo(db)

	scoped := r.WithNamespace("app2026.dbo").WithYear(2025)
	if got := scoped.(*itemRepository).Table(); got != "app2025.dbo.item" {
		t.Errorf("namespace then year: %q", got)
	}
	scoped = r.WithYear(2025).WithNamespace("app2026.dbo")
	if got := scoped.(*itemRepository).Table(); got != "app2025.dbo.item" {
		t.Errorf("year then namespace: %q", got)
	}
	if got := r.Table(); got != "item" {
		t.Errorf("receiver changed to %q", got)
	}
	if scoped.(*itemRepository).Spec() != &itemSpec {
		t.Error("a scoped copy describes another table")
	}

	tx, err := db.Beginx()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	inTx := r.WithNamespace("app").WithTx(tx).(*itemRepository)
	if inTx.Ext() != tx {
		t.Error("WithTx: Ext is not the transaction")
	}
	if got := inTx.Table(); got != "app.item" {
		t.Errorf("WithTx dropped the namespace: %q", got)
	}
	if r.Ext() != db {
		t.Error("receiver: Ext is not the pool")
	}
}

// NewBaseRepo copies the default namespace as it stands at the call, so a
// later Configure leaves an existing repository alone.
func TestNewBaseRepoCopiesTheDefaultNamespace(t *testing.T) {
	prev := database.DefaultNamespace
	t.Cleanup(func() { database.Configure(prev) })

	f := &fakeDB{}
	db := f.open(t, "mysql")
	database.Configure("app2026")
	r := newItemRepo(db)
	database.Configure("other")

	if got := r.Table(); got != "app2026.item" {
		t.Errorf("Table = %q, want app2026.item", got)
	}
	if got := r.Qualify("unit"); got != "app2026.unit" {
		t.Errorf("Qualify = %q, want app2026.unit", got)
	}
}

// The join helpers qualify the joined table, LeftJoin drops out when
// nothing wants it, and Needs and SortsOn say when something does.
func TestJoins(t *testing.T) {
	f := &fakeDB{}
	r := newItemRepo(f.open(t, "mysql")).WithNamespace("app")
	b := r.(*itemRepository)

	if got := b.LeftJoin(false, "unit", "u", "u.id = i.unit_id"); got != "" {
		t.Errorf("unwanted LeftJoin = %q", got)
	}
	if got, want := b.LeftJoin(true, "unit", "u", "u.id = i.unit_id"),
		"\nLEFT JOIN app.unit u ON u.id = i.unit_id"; got != want {
		t.Errorf("LeftJoin = %q, want %q", got, want)
	}
	if got, want := b.InnerJoin("user", "w", "w.id = i.user_id"),
		"\nJOIN app.`user` w ON w.id = i.user_id"; got != want {
		t.Errorf("InnerJoin = %q, want %q", got, want)
	}

	if !b.Needs(nil, "title") || b.Needs([]string{"code"}, "title") {
		t.Error("Needs does not follow the resolved SELECT list")
	}
	sort := []database.SortField{{Col: "i.code"}, {Col: "u.name"}}
	if !b.SortsOn(sort, "i") || b.SortsOn(sort, "u") {
		t.Error("SortsOn does not follow the sort allowlist")
	}
}

// A spec that describes its key inconsistently stops the program at
// construction; a consistent one, generated or natural, does not.
func TestNewBaseRepoChecksTheKey(t *testing.T) {
	embed := func(b BaseRepo[holiday, string, any]) any { return b }
	embedInt := func(b BaseRepo[holiday, int, any]) any { return b }
	keyOf := func(*holiday) (string, bool) { return "", false }
	intKeyOf := func(*holiday) (int, bool) { return 0, false }

	testutil.MustPanic(t, "natural key without KeyOf", func() {
		NewBaseRepo(nil, &TableSpec[holiday, string]{
			Name: "h", Key: "code", Writable: []string{"code"},
		}, embed)
	})
	testutil.MustPanic(t, "KeyOf on a generated key", func() {
		NewBaseRepo(nil, &TableSpec[holiday, int]{
			Name: "h", Key: "id", KeyOf: intKeyOf,
		}, embedInt)
	})
	testutil.MustPanic(t, "generated key that is not an int", func() {
		NewBaseRepo(nil, &TableSpec[holiday, string]{
			Name: "h", Key: "code",
		}, embed)
	})
	testutil.MustPanic(t, "Patchable listing the key", func() {
		NewBaseRepo(nil, &TableSpec[holiday, int]{
			Name: "h", Key: "id", Patchable: []string{"id"},
		}, embedInt)
	})

	NewBaseRepo(nil, &TableSpec[holiday, string]{
		Name: "h", Key: "code", Writable: []string{"code"}, KeyOf: keyOf,
	}, embed)
	NewBaseRepo(nil, &TableSpec[holiday, int]{
		Name: "h", Key: "id", Writable: []string{"note"},
	}, embedInt)
}
