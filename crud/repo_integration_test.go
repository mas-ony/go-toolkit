//go:build integration

package crud

// Integration tests for repo.go, against the servers the DSN variables
// of the database package name:
//
//	DB_TEST_MYSQL_DSN
//		user:pass@tcp(host:3306)/scratch?parseTime=true&loc=Asia%2FJakarta
//	DB_TEST_SQLSERVER_DSN
//		sqlserver://user:pass@host?database=scratch&timezone=Asia%2FJakarta
//
//	go test -tags integration -run Integration ./crud
//
// Each test runs once per engine whose DSN is set and skips when neither
// is. The zone parameters are for the datetime suite; this one passes
// with or without them.
//
// What these tests are FOR is what repo_test.go can only assume: that
// each engine accepts the statements BaseRepo writes, with a table and
// columns that have to be quoted, and that a write reports the rows it
// matched. MySQL reports the rows it changed unless the connection sets
// ClientFoundRows, so every connection here sets it, as database.New
// does, except the one that shows what it is for.
//
// The tests create and drop their own tables. Point the DSNs at a scratch
// database, never at one that holds data.

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
	"github.com/mas-ony/go-toolkit/database"
	"github.com/mas-ony/go-toolkit/internal/dbdriver"
	"github.com/mas-ony/go-toolkit/internal/testutil/dbtest"
)

// row is a row of the scratch table most tests here create: a generated
// key, and two columns named after words an engine reserves, rank in
// MySQL and user in T-SQL.
type row struct {
	ID   *int    `db:"id"`
	Rank *int    `db:"rank"`
	User *string `db:"user"`
}

// rowOption is the read option of rowRepository.
type rowOption struct {
	ReadOption[int]

	// Rank and Ranks are exact matches on rank.
	Rank  *int
	Ranks []int
}

// rowRepo is the interface a repository over row exposes.
type rowRepo interface {
	Scoped[rowRepo]
	Create(ctx context.Context, m *row) (int, error)
	Read(ctx context.Context, opt rowOption) (ReadResult[row], error)
	Update(ctx context.Context, m *row) error
	Patch(ctx context.Context, id int, fields map[string]any) error
	Delete(ctx context.Context, id int) error
}

// rowRepository is a repository over row, written as a service would.
type rowRepository struct {
	BaseRepo[row, int, rowRepo]
}

// note is a row of a table keyed by a natural key.
type note struct {
	Code  *string `db:"code"`
	Label *string `db:"label"`
}

// errRollback is what a transaction that must roll back returns.
var errRollback = errors.New("roll back")

// embedRow is the function NewBaseRepo is handed for rowRepository.
func embedRow(b BaseRepo[row, int, rowRepo]) rowRepo {
	return &rowRepository{b}
}

// scratch creates a table from ddl, in which %s stands for the name,
// and returns the name unquoted, as a TableSpec takes it. The name is
// one only a quoted identifier can hold, so BaseRepo has to quote it.
func scratch(t *testing.T, db *sqlx.DB, base, ddl string) string {
	t.Helper()
	return dbtest.QuotedScratchTable(t, db, database.QuoteIdent,
		"crud "+base, ddl)
}

// open connects to the engine e names and selects its grammar. On MySQL
// it sets ClientFoundRows to found, whatever the DSN says.
func open(t *testing.T, e dbtest.Engine, dsn string, found bool) *sqlx.DB {
	t.Helper()
	selectGrammar(t, e.Driver)

	if e.Driver == dbdriver.MySQL {
		cfg, err := mysql.ParseDSN(dsn)
		if err != nil {
			t.Fatalf("parsing %s: %v", e.DSNEnv, err)
		}
		cfg.ClientFoundRows = found
		dsn = cfg.FormatDSN()
	}
	db, err := sqlx.Connect(e.Driver, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// forEachEngine runs fn once per engine whose DSN is set, with a
// repository over a fresh row table on a connection that reports rows
// matched.
func forEachEngine(
	t *testing.T,
	fn func(t *testing.T, r *rowRepository, db *sqlx.DB),
) {
	t.Helper()
	dbtest.Each(t, func(t *testing.T, e dbtest.Engine, dsn string) {
		db := open(t, e, dsn, true)
		fn(t, newRowRepo(t, e, db), db)
	})
}

// mustCreate inserts a row and returns its key.
func mustCreate(t *testing.T, r *rowRepository, rank int, user string) int {
	t.Helper()
	id, err := r.Create(context.Background(),
		&row{Rank: &rank, User: &user})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return id
}

// mustRead reads the row whose key is id, or nil.
func mustRead(t *testing.T, r *rowRepository, id int) *row {
	t.Helper()
	got, err := FirstOf(r.Read(context.Background(),
		rowOption{ReadOption: ReadOption[int]{ID: &id}}))
	if err != nil {
		t.Fatalf("Read %d: %v", id, err)
	}
	return got
}

// newRowRepo creates the row table and returns a repository over it.
// The spec quotes rank and user where it is SQL of its own, the SELECT
// list and the allowlists; BaseRepo quotes them in what it writes.
func newRowRepo(t *testing.T, e dbtest.Engine, db *sqlx.DB) *rowRepository {
	t.Helper()
	name := scratch(t, db, "row", pick(e.Driver,
		"CREATE TABLE %s (id INT AUTO_INCREMENT PRIMARY KEY,"+
			" `rank` INT NOT NULL, `user` VARCHAR(50) NOT NULL)",
		"CREATE TABLE %s (id INT IDENTITY(1,1) PRIMARY KEY,"+
			" [rank] INT NOT NULL, [user] NVARCHAR(50) NOT NULL)",
	))
	rank := "t." + database.QuoteIdent("rank")
	user := "t." + database.QuoteIdent("user")
	spec := &TableSpec[row, int]{
		Name:  name,
		Alias: "t",
		Key:   "id",
		Cols: map[string]string{
			"id":   "t.id",
			"rank": rank,
			"user": user,
		},
		DefaultCols: []string{"id", "rank", "user"},
		Writable:    []string{"rank", "user"},
		Patchable:   []string{"rank"},
		SearchCols:  []string{user},
		SortCols:    []string{rank, "t.id"},
		DefaultSort: "t.id",
	}
	return &rowRepository{NewBaseRepo(db, spec, embedRow)}
}

// Read applies the option the way a repository's own Read does. The
// rank column is quoted here, since a filter is the repository's own
// SQL.
func (r *rowRepository) Read(
	ctx context.Context,
	opt rowOption,
) (ReadResult[row], error) {
	q, filter := r.Begin(opt.ReadOption)
	if filter {
		rank := "t." + database.QuoteIdent("rank")
		AndEq(q, rank, "rank", opt.Rank)
		AndIn(q, rank, "ranks", opt.Ranks)
	}
	return r.Fetch(ctx, q, opt.ListOption)
}

// Every write BaseRepo derives from a TableSpec runs as written on each
// engine, over a table and columns that have to be quoted, and a write
// to a row that is not there reports sql.ErrNoRows.
func TestIntegrationWritesRoundTrip(t *testing.T) {
	forEachEngine(t, func(t *testing.T, r *rowRepository, _ *sqlx.DB) {
		ctx := context.Background()
		first := mustCreate(t, r, 1, "alpha")
		second := mustCreate(t, r, 2, "beta")
		if second == first {
			t.Fatalf("two rows were given the key %d", first)
		}

		got := mustRead(t, r, first)
		if got == nil || *got.Rank != 1 || *got.User != "alpha" {
			t.Fatalf("read back %+v", got)
		}

		err := r.Update(ctx, &row{ID: &first, Rank: new(3),
			User: new("gamma")})
		if err != nil {
			t.Fatalf("Update: %v", err)
		}
		err = r.Patch(ctx, first, map[string]any{"rank": 4})
		if err != nil {
			t.Fatalf("Patch: %v", err)
		}
		got = mustRead(t, r, first)
		if got == nil || *got.Rank != 4 || *got.User != "gamma" {
			t.Errorf("after Update and Patch: %+v", got)
		}

		if err := r.Delete(ctx, first); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if got := mustRead(t, r, first); got != nil {
			t.Errorf("deleted row read back: %+v", got)
		}
		if got := mustRead(t, r, second); got == nil {
			t.Error("Delete removed another row")
		}

		missing := &row{ID: &first, Rank: new(5), User: new("delta")}
		if err := r.Update(ctx, missing); err != sql.ErrNoRows {
			t.Errorf("Update of a missing row = %v", err)
		}
		err = r.Patch(ctx, first, map[string]any{"rank": 5})
		if err != sql.ErrNoRows {
			t.Errorf("Patch of a missing row = %v", err)
		}
		if err := r.Delete(ctx, first); err != sql.ErrNoRows {
			t.Errorf("Delete of a missing row = %v", err)
		}
	})
}

// Re-saving a row unchanged matches it on both engines under
// ClientFoundRows. Without the setting, MySQL counts the row as
// unchanged, and the same save reads as a missing row: the reason
// database.New sets it, and a pool opened another way must too.
func TestIntegrationUnchangedUpdateMatchesItsRow(t *testing.T) {
	dbtest.Each(t, func(t *testing.T, e dbtest.Engine, dsn string) {
		ctx := context.Background()
		r := newRowRepo(t, e, open(t, e, dsn, true))
		id := mustCreate(t, r, 1, "same")
		same := &row{ID: &id, Rank: new(1), User: new("same")}

		if err := r.Update(ctx, same); err != nil {
			t.Errorf("with ClientFoundRows: Update = %v, want nil", err)
		}
		if e.Driver != dbdriver.MySQL {
			return
		}
		changed := &rowRepository{
			NewBaseRepo(open(t, e, dsn, false), r.Spec(), embedRow),
		}
		if err := changed.Update(ctx, same); err != sql.ErrNoRows {
			t.Errorf("without ClientFoundRows: Update = %v, "+
				"want sql.ErrNoRows", err)
		}
	})
}

// A page counts every match and reads the page in the order asked for;
// an IN list and a search filter on each engine, the search matching a
// LIKE metacharacter as itself.
func TestIntegrationFetchModes(t *testing.T) {
	forEachEngine(t, func(t *testing.T, r *rowRepository, _ *sqlx.DB) {
		ctx := context.Background()
		for i := 1; i <= 5; i++ {
			mustCreate(t, r, i, "user-"+string(rune('0'+i)))
		}
		mustCreate(t, r, 6, "50% off")
		rank := "t." + database.QuoteIdent("rank")

		page, err := r.Read(ctx, rowOption{
			ReadOption: ReadOption[int]{ListOption: ListOption{
				Sort:  []database.SortField{{Col: rank, Dir: "desc"}},
				Page:  2,
				Limit: 2,
			}},
		})
		if err != nil {
			t.Fatalf("page: %v", err)
		}
		if page.Total != 6 || len(page.Items) != 2 ||
			*page.Items[0].Rank != 4 || *page.Items[1].Rank != 3 {
			t.Errorf("page: Total %d, items %+v; want 6, ranks 4 and 3",
				page.Total, page.Items)
		}

		in, err := r.Read(ctx, rowOption{Ranks: []int{1, 3, 9}})
		if err != nil {
			t.Fatalf("IN: %v", err)
		}
		if in.Total != 2 || *in.Items[0].Rank != 1 ||
			*in.Items[1].Rank != 3 {
			t.Errorf("IN: %+v, want ranks 1 and 3", in.Items)
		}

		found, err := r.Read(ctx,
			rowOption{ReadOption: ReadOption[int]{Search: new("0%")}})
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		if found.Total != 1 || *found.Items[0].User != "50% off" {
			t.Errorf("search: %+v, want the one row holding 0%%",
				found.Items)
		}
	})
}

// A prefixed statement runs on each engine: a lookup capped on the SELECT
// after a recursive CTE, and a plain list after it.
func TestIntegrationPrefixedQuery(t *testing.T) {
	forEachEngine(t, func(t *testing.T, r *rowRepository, _ *sqlx.DB) {
		ctx := context.Background()
		for i := 1; i <= 5; i++ {
			mustCreate(t, r, i, "u")
		}
		cte := database.RecursiveCTE("n") +
			"SELECT 1 AS k UNION ALL SELECT k + 1 FROM n WHERE k < 3)"
		filter := "t." + database.QuoteIdent("rank") +
			" IN (SELECT k FROM n)"

		q := r.Select(nil).Prepend(cte).And(filter).One()
		one, err := r.Fetch(ctx, q, ListOption{})
		if err != nil {
			t.Fatalf("lookup: %v", err)
		}
		if one.Total != 1 || *one.Items[0].Rank > 3 {
			t.Errorf("lookup: %+v, want one row ranked 3 or less",
				one.Items)
		}

		q = r.Select(nil).Prepend(cte).And(filter)
		list, err := r.Fetch(ctx, q, ListOption{})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if list.Total != 3 {
			t.Errorf("list: %d rows, want 3", list.Total)
		}
	})
}

// A write inside a transaction goes through it: rolled back, it leaves
// nothing behind.
func TestIntegrationWithTx(t *testing.T) {
	forEachEngine(t, func(t *testing.T, r *rowRepository, db *sqlx.DB) {
		ctx := context.Background()
		err := database.WithTxContext(ctx, db, nil,
			func(tx *sqlx.Tx) error {
				_, err := r.WithTx(tx).Create(ctx,
					&row{Rank: new(1), User: new("gone")})
				if err != nil {
					return err
				}
				return errRollback
			})
		if !errors.Is(err, errRollback) {
			t.Fatalf("WithTxContext = %v, want the rollback", err)
		}
		all, err := r.Read(ctx, rowOption{})
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if all.Total != 0 {
			t.Errorf("rolled-back row read back: %+v", all.Items)
		}
	})
}

// A natural key is written as given, addressed by Update and Delete, and
// a duplicate is the driver's own refusal under "<table>.Create", which
// database.Rejected still reads through the wrapping.
func TestIntegrationNaturalKey(t *testing.T) {
	dbtest.Each(t, func(t *testing.T, e dbtest.Engine, dsn string) {
		ctx := context.Background()
		db := open(t, e, dsn, true)
		name := scratch(t, db, "note", pick(e.Driver,
			"CREATE TABLE %s (code VARCHAR(10) NOT NULL PRIMARY KEY,"+
				" label VARCHAR(50) NULL)",
			"CREATE TABLE %s (code NVARCHAR(10) NOT NULL PRIMARY KEY,"+
				" label NVARCHAR(50) NULL)",
		))
		r := NewBaseRepo(db, &TableSpec[note, string]{
			Name:  name,
			Alias: "n",
			Key:   "code",
			KeyOf: func(m *note) (string, bool) {
				if m.Code == nil {
					return "", false
				}
				return *m.Code, true
			},
			Cols: map[string]string{
				"code":  "n.code",
				"label": "n.label",
			},
			DefaultCols: []string{"code", "label"},
			Writable:    []string{"code", "label"},
			Updatable:   []string{"label"},
			DefaultSort: "n.code",
		}, func(b BaseRepo[note, string, any]) any { return b })

		code := "2026-08-17"
		key, err := r.Create(ctx, &note{Code: &code, Label: new("a")})
		if err != nil || key != code {
			t.Fatalf("Create = %q, %v", key, err)
		}
		_, err = r.Create(ctx, &note{Code: &code, Label: new("b")})
		if !errors.Is(database.Rejected(err), database.ErrRejected) {
			t.Errorf("duplicate: %v, want a refused row", err)
		}
		if err == nil || !strings.HasPrefix(err.Error(), name+".Create: ") {
			t.Errorf("duplicate: %v, want it named after the table", err)
		}

		if err := r.Update(ctx,
			&note{Code: &code, Label: new("c")}); err != nil {
			t.Fatalf("Update: %v", err)
		}
		q, _ := r.Begin(ReadOption[string]{ID: &code})
		got, err := FirstOf(r.Fetch(ctx, q, ListOption{}))
		if err != nil || got == nil || *got.Label != "c" {
			t.Errorf("read back %+v, %v; want label c", got, err)
		}

		if err := r.Delete(ctx, code); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if err := r.Delete(ctx, code); err != sql.ErrNoRows {
			t.Errorf("second Delete = %v, want sql.ErrNoRows", err)
		}
	})
}
