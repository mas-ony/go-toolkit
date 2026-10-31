package crud

// Tests for scalar.go, against the fake driver of repo_test.go.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"testing"

	"github.com/mas-ony/go-toolkit/internal/testutil/sqltest"
)

// Count counts the repository's own table, answered here by the fake's
// canned count.
func TestCount(t *testing.T) {
	grammars(t, func(t *testing.T, name string) {
		f := &fakeDB{count: 9}
		n, err := newItemRepo(f.open(t, name)).Count(context.Background())
		if err != nil || n != 9 {
			t.Fatalf("Count = %d, %v; want 9, nil", n, err)
		}
		wantStatements(t, f, sqltest.Statement{
			Query: "SELECT COUNT(*) FROM item",
			Args:  []any{},
		})
	})
}

// Max reads the largest value of a column, 0 standing in for an empty
// table, and quotes a reserved column name.
func TestMax(t *testing.T) {
	grammars(t, func(t *testing.T, name string) {
		f := &fakeDB{
			cols: []string{"n"},
			rows: [][]driver.Value{{int64(41)}},
		}
		db := f.open(t, name)
		ctx := context.Background()

		n, err := newItemRepo(db).Max(ctx, "code")
		if err != nil || n != 41 {
			t.Fatalf("Max = %d, %v; want 41, nil", n, err)
		}
		_, err = newRankedRepo(db).Max(ctx, "rank")
		if err != nil {
			t.Fatalf("Max(rank): %v", err)
		}
		wantStatements(t, f,
			sqltest.Statement{
				Query: "SELECT COALESCE(MAX(code), 0) FROM item",
				Args:  []any{},
			},
			sqltest.Statement{
				Query: pick(name,
					"SELECT COALESCE(MAX(`rank`), 0) FROM `order`",
					"SELECT COALESCE(MAX([rank]), 0) FROM [order]"),
				Args: []any{},
			})
	})
}

// GetInt binds its arguments through ? placeholders in the driver's
// format, and reports a failure under the method it is given.
func TestGetInt(t *testing.T) {
	grammars(t, func(t *testing.T, name string) {
		f := &fakeDB{
			cols: []string{"n"},
			rows: [][]driver.Value{{int64(3)}},
		}
		r := newItemRepo(f.open(t, name))
		ctx := context.Background()

		n, err := r.GetInt(ctx, "CountByCode",
			"SELECT COUNT(DISTINCT title) FROM "+r.Table()+
				" WHERE code = ?", "A-1")
		if err != nil || n != 3 {
			t.Fatalf("GetInt = %d, %v; want 3, nil", n, err)
		}
		wantStatements(t, f, sqltest.Statement{
			Query: "SELECT COUNT(DISTINCT title) FROM item WHERE code = " +
				pick(name, "?", "@p1"),
			Args: []any{"A-1"},
		})

		f.err = errFake
		_, err = r.GetInt(ctx, "CountByCode", "SELECT 1")
		if !errors.Is(err, errFake) ||
			err.Error() != "item.CountByCode: "+errFake.Error() {
			t.Errorf("err = %v, want item.CountByCode wrapping errFake",
				err)
		}
	})
}

// A statement that returns no row fails with sql.ErrNoRows, wrapped.
func TestGetIntNoRow(t *testing.T) {
	f := &fakeDB{cols: []string{"n"}}
	r := newItemRepo(f.open(t, "mysql"))
	_, err := r.GetInt(context.Background(), "Max", "SELECT 1 WHERE 1=0")
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("err = %v, want sql.ErrNoRows", err)
	}
}
