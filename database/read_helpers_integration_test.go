//go:build integration

package database

// Integration tests for read_helpers.go. They share the DSN variables and
// the helpers of dialect_integration_test.go and run the same way.

import (
	"context"
	"testing"

	"github.com/jmoiron/sqlx"
	"github.com/mas-ony/go-toolkit/internal/testutil/dbtest"
)

// readRow is one row of the read fixture, with the has_child flag that
// ExistsFlag computes.
type readRow struct {
	ID       int    `db:"id"`
	Code     string `db:"code"`
	Amount   int64  `db:"amount"`
	HasChild *bool  `db:"has_child"`
}

// codeCount is one group of the GroupClause fixture.
type codeCount struct {
	Code  string `db:"code"`
	Total int    `db:"total"`
}

// The fixture in the shape the unit tests use: short names mapped to the SQL
// each one emits, here for a table aliased t.
var (
	itAllowedCols = map[string]string{
		"id":     "t.id",
		"code":   "t.code",
		"amount": "t.amount",
	}
	itDefaultCols = []string{"id", "code", "amount"}
	itSortCols    = []string{"t.code", "t.amount"}
)

// readFixture creates a parent table holding three rows and a child table
// holding one, and returns their qualified names.
func readFixture(t *testing.T, db *sqlx.DB) (parent, child string) {
	t.Helper()
	parent = scratchTable(t, db, "read", onEngine(
		"CREATE TABLE %s (id INT NOT NULL PRIMARY KEY,"+
			" code VARCHAR(20) NOT NULL, amount BIGINT NOT NULL)",
		"CREATE TABLE %s (id INT NOT NULL PRIMARY KEY,"+
			" code NVARCHAR(20) NOT NULL, amount BIGINT NOT NULL)",
	))
	child = scratchTable(t, db, "read_child",
		"CREATE TABLE %s (id INT NOT NULL PRIMARY KEY,"+
			" parent_id INT NOT NULL)")
	dbtest.Exec(t, db,
		"INSERT INTO "+parent+" (id, code, amount) VALUES"+
			" (1, 'alpha', 1234), (2, 'beta', 5678), (3, 'alpha', 91011)",
		"INSERT INTO "+child+" (id, parent_id) VALUES (1, 2)",
	)
	return parent, child
}

// hasChild is the select-list column that flags a parent row of the read
// fixture, aliased t, with whether child holds a row for it.
func hasChild(child string) string {
	return ExistsFlag("SELECT 1 FROM "+child+" c WHERE c.parent_id = t.id",
		"has_child")
}

// GetOne scans a row that exists, reports a missing one as (false, nil),
// and binds through a transaction as well as through a pool.
func TestIntegrationGetOne(t *testing.T) {
	forEachEngine(t, func(t *testing.T, db *sqlx.DB) {
		ctx := context.Background()
		parent, _ := readFixture(t, db)
		query := "SELECT id, code, amount FROM " + parent +
			"\nWHERE id = :id"

		var row readRow
		found, err := GetOne(ctx, db, query, map[string]any{"id": 2}, &row)
		if err != nil {
			t.Fatalf("%v\n%s", err, query)
		}
		if !found || row.Code != "beta" || row.Amount != 5678 {
			t.Errorf("found = %v, row = %+v", found, row)
		}

		var missing readRow
		found, err = GetOne(ctx, db, query, map[string]any{"id": 99},
			&missing)
		if err != nil || found {
			t.Errorf("missing row: found = %v, err = %v", found, err)
		}

		// ext binds as well as executes, so a transaction has to work as
		// the source of the placeholder style too.
		tx, err := db.Beginx()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback() }()

		var inTx readRow
		found, err = GetOne(ctx, tx, query, map[string]any{"id": 1}, &inTx)
		if err != nil || !found || inTx.Code != "alpha" {
			t.Errorf("in a transaction: found = %v, err = %v, row = %+v",
				found, err, inTx)
		}
	})
}

// CountQuery's derived table and SelectList's page run on both engines
// from one args map, and an empty result leaves a nil slice nil.
func TestIntegrationCountQueryAndSelectList(t *testing.T) {
	forEachEngine(t, func(t *testing.T, db *sqlx.DB) {
		ctx := context.Background()
		parent, _ := readFixture(t, db)
		base := "SELECT " +
			SelectClause(nil, itAllowedCols, itDefaultCols, "id") +
			"\nFROM " + parent + " t" +
			"\nWHERE t.code = :code"
		args := map[string]any{"code": "alpha"}

		total, err := CountQuery(ctx, db, base, args)
		if err != nil {
			t.Fatalf("%v\n%s", err, base)
		}
		if total != 2 {
			t.Errorf("total = %d, want 2", total)
		}

		// A key the count text never mentions is bound by nothing, which is
		// what lets one args map serve the count and the page.
		args["unused"] = "ignored"
		if total, err = CountQuery(ctx, db, base, args); err != nil ||
			total != 2 {
			t.Errorf("with an unused key: total = %d, err = %v", total, err)
		}

		query := base + SortClause(
			[]SortField{{Col: "t.amount", Dir: "desc"}}, itSortCols, "t.id")
		var rows []readRow
		if err := SelectList(ctx, db, query, args, &rows); err != nil {
			t.Fatalf("%v\n%s", err, query)
		}
		if len(rows) != 2 || rows[0].Amount != 91011 {
			t.Errorf("rows = %+v", rows)
		}

		// An empty result leaves the slice alone, nil included.
		var none []readRow
		err = SelectList(ctx, db, query, map[string]any{"code": "none"},
			&none)
		if err != nil {
			t.Fatal(err)
		}
		if none != nil {
			t.Errorf("empty result produced %#v, want a nil slice", none)
		}
	})
}

// ExistsFlag scans into a bool on both engines, and a LIKE over AsText
// searches a numeric column instead of failing a conversion.
func TestIntegrationExistsFlagAndAsText(t *testing.T) {
	forEachEngine(t, func(t *testing.T, db *sqlx.DB) {
		ctx := context.Background()
		parent, child := readFixture(t, db)
		flag := hasChild(child)
		query := "SELECT t.id, t.code, t.amount, " + flag +
			"\nFROM " + parent + " t" +
			"\nWHERE " + LikePredicate(AsText("t.amount"), "search") +
			"\nORDER BY t.id"
		args := map[string]any{"search": LikeArg("5678")}

		var rows []readRow
		if err := SelectList(ctx, db, query, args, &rows); err != nil {
			t.Fatalf("%v\n%s", err, query)
		}
		if len(rows) != 1 || rows[0].ID != 2 {
			t.Fatalf("rows = %+v, want the row with amount 5678", rows)
		}
		if rows[0].HasChild == nil || !*rows[0].HasChild {
			t.Errorf("has_child = %v, want true", rows[0].HasChild)
		}
	})
}

// GroupClause groups, and SortClause orders the groups, on both engines.
func TestIntegrationGroupClause(t *testing.T) {
	forEachEngine(t, func(t *testing.T, db *sqlx.DB) {
		ctx := context.Background()
		parent, _ := readFixture(t, db)
		query := "SELECT t.code, COUNT(*) AS total" +
			"\nFROM " + parent + " t" +
			GroupClause("t.code", []string{"t.code"}) +
			SortClause([]SortField{{Col: "t.code", Dir: "asc"}},
				itSortCols, "t.code")

		var got []codeCount
		if err := SelectList(ctx, db, query, map[string]any{},
			&got); err != nil {
			t.Fatalf("%v\n%s", err, query)
		}
		want := []codeCount{{"alpha", 2}, {"beta", 1}}
		if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
			t.Errorf("groups = %+v, want %+v", got, want)
		}
	})
}

// An extra column rides along the SELECT list SelectClause builds, and
// through the derived table CountQuery wraps it in, on both engines.
func TestIntegrationSelectClauseExtra(t *testing.T) {
	forEachEngine(t, func(t *testing.T, db *sqlx.DB) {
		ctx := context.Background()
		parent, child := readFixture(t, db)
		flag := hasChild(child)
		query := "SELECT " + SelectClause([]string{"code"},
			itAllowedCols, itDefaultCols, "id", flag) +
			"\nFROM " + parent + " t"

		total, err := CountQuery(ctx, db, query, map[string]any{})
		if err != nil {
			t.Fatalf("%v\n%s", err, query)
		}
		if total != 3 {
			t.Errorf("total = %d, want 3", total)
		}

		var rows []readRow
		err = SelectList(ctx, db, query+"\nORDER BY t.id",
			map[string]any{}, &rows)
		if err != nil {
			t.Fatalf("%v\n%s", err, query)
		}
		if len(rows) != 3 || rows[0].HasChild == nil ||
			*rows[0].HasChild || rows[1].HasChild == nil ||
			!*rows[1].HasChild {
			t.Errorf("rows = %+v, want row 2 alone flagged", rows)
		}
	})
}

// InList binds a list on both engines, and the (NULL) of an empty list
// parses and matches no row, under IN and NOT IN alike.
func TestIntegrationInList(t *testing.T) {
	forEachEngine(t, func(t *testing.T, db *sqlx.DB) {
		ctx := context.Background()
		parent, _ := readFixture(t, db)
		count := func(op string, ids []int) int {
			t.Helper()
			args := map[string]any{}
			query := "SELECT t.id FROM " + parent + " t" +
				"\nWHERE t.id " + op + " " + InList(args, "id", ids)
			n, err := CountQuery(ctx, db, query, args)
			if err != nil {
				t.Fatalf("%v\n%s", err, query)
			}
			return n
		}

		if n := count("IN", []int{1, 3}); n != 2 {
			t.Errorf("IN (1, 3) counted %d rows, want 2", n)
		}
		if n := count("NOT IN", []int{1, 3}); n != 1 {
			t.Errorf("NOT IN (1, 3) counted %d rows, want 1", n)
		}
		if n := count("IN", nil); n != 0 {
			t.Errorf("IN of no values counted %d rows, want 0", n)
		}
		if n := count("NOT IN", nil); n != 0 {
			t.Errorf("NOT IN of no values counted %d rows, want 0", n)
		}
	})
}
