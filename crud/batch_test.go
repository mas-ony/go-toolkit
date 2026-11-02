package crud

// Tests for batch.go, against the fake driver of repo_test.go, over
// connections whose transactions count their commits and rollbacks.

import (
	"context"
	"database/sql/driver"
	"errors"
	"testing"

	"github.com/jmoiron/sqlx"
	"github.com/mas-ony/go-toolkit/internal/testutil/sqltest"
)

// entry is a row of a table with a generated key, which takes the key
// back through SetID.
type entry struct {
	ID   *int    `db:"id"`
	Code *string `db:"code"`
}

// entryRepo is the interface a repository over entry exposes.
type entryRepo interface {
	Scoped[entryRepo]
	Create(ctx context.Context, e *entry) (int, error)
}

// entryRepository is a BaseRepo over entrySpec, embedded by value.
type entryRepository struct {
	BaseRepo[entry, int, entryRepo]
}

// txCounts counts what the transactions of a countingConn end in.
type txCounts struct{ commits, rollbacks int }

// countingTx counts its Commit and Rollback into n.
type countingTx struct{ n *txCounts }

// countingConn is a fakeConn whose Begin starts a countingTx.
type countingConn struct {
	fakeConn
	n *txCounts
}

var entrySpec = TableSpec[entry, int]{
	Name:        "entry",
	Alias:       "e",
	Key:         "id",
	Cols:        map[string]string{"id": "e.id", "code": "e.code"},
	DefaultCols: []string{"id", "code"},
	Writable:    []string{"code"},
	DefaultSort: "e.id",
}

// The interface check CreateAll relies on: a repository interface that
// embeds Scoped and declares Create is a Batch.
var _ Batch[entry, entryRepo] = entryRepo(nil)

// openCounting selects the grammar driverName stands for and returns a
// *sqlx.DB over f whose transactions count into n.
func openCounting(
	t *testing.T,
	f *fakeDB,
	n *txCounts,
	driverName string,
) *sqlx.DB {
	t.Helper()
	selectGrammar(t, driverName)
	return sqltest.Open(t, func() driver.Conn {
		return countingConn{fakeConn{db: f}, n}
	}, driverName)
}

// newEntryRepo returns an entryRepository over db.
func newEntryRepo(db *sqlx.DB) entryRepo {
	return &entryRepository{NewBaseRepo(db, &entrySpec,
		func(b BaseRepo[entry, int, entryRepo]) entryRepo {
			return &entryRepository{b}
		})}
}

// SetID stores the key CreateAll writes back.
func (e *entry) SetID(id int) { e.ID = &id }

// Commit counts a commit.
func (tx countingTx) Commit() error {
	tx.n.commits++
	return nil
}

// Rollback counts a rollback.
func (tx countingTx) Rollback() error {
	tx.n.rollbacks++
	return nil
}

// Begin starts a transaction that counts how it ends.
func (c countingConn) Begin() (driver.Tx, error) {
	return countingTx{c.n}, nil
}

// Every item is inserted inside one transaction, which commits, and gets
// its generated key back.
func TestCreateAllCommitsOneTransaction(t *testing.T) {
	grammars(t, func(t *testing.T, name string) {
		f := &fakeDB{lastID: 7}
		var n txCounts
		db := openCounting(t, f, &n, name)

		items := []entry{{Code: new("a")}, {Code: new("b")}}
		created, err := CreateAll(context.Background(), db,
			newEntryRepo(db), items)
		if err != nil {
			t.Fatalf("CreateAll: %v", err)
		}
		if len(created) != 2 || created[1] != &items[1] {
			t.Fatalf("created = %v, want pointers into items", created)
		}
		for i, e := range items {
			if e.ID == nil || *e.ID != 7 {
				t.Errorf("items[%d].ID = %v, want 7", i, e.ID)
			}
		}
		if n.commits != 1 || n.rollbacks != 0 {
			t.Errorf("commits, rollbacks = %d, %d; want 1, 0",
				n.commits, n.rollbacks)
		}
		insert := pick(name,
			"INSERT INTO entry (\ncode)\nVALUES (\n?)",
			"DECLARE @Inserted TABLE (id BIGINT);"+
				"\nINSERT INTO entry (\ncode)"+
				"\nOUTPUT INSERTED.id INTO @Inserted (id)"+
				"\nVALUES (\n@p1);"+
				"\nSELECT id FROM @Inserted;")
		wantStatements(t, f,
			sqltest.Statement{Query: insert, Args: []any{"a"}},
			sqltest.Statement{Query: insert, Args: []any{"b"}})
	})
}

// A failed insert rolls the batch back and returns that insert's error,
// in the "<table>.<Method>" form, with no rows.
func TestCreateAllRollsBackOnError(t *testing.T) {
	grammars(t, func(t *testing.T, name string) {
		f := &fakeDB{err: errFake}
		var n txCounts
		db := openCounting(t, f, &n, name)

		created, err := CreateAll(context.Background(), db,
			newEntryRepo(db), []entry{{Code: new("a")}})
		if !errors.Is(err, errFake) ||
			err.Error() != "entry.Create: "+errFake.Error() {
			t.Fatalf("err = %v, want entry.Create wrapping errFake", err)
		}
		if created != nil {
			t.Errorf("created = %v, want nil", created)
		}
		if n.commits != 0 || n.rollbacks != 1 {
			t.Errorf("commits, rollbacks = %d, %d; want 0, 1",
				n.commits, n.rollbacks)
		}
	})
}

// An empty batch still opens and commits its transaction, and returns no
// rows rather than an error.
func TestCreateAllEmpty(t *testing.T) {
	f := &fakeDB{}
	var n txCounts
	db := openCounting(t, f, &n, "mysql")

	created, err := CreateAll(context.Background(), db, newEntryRepo(db),
		[]entry(nil))
	if err != nil || len(created) != 0 {
		t.Fatalf("CreateAll(nil) = %v, %v; want no rows, nil",
			created, err)
	}
	if n.commits != 1 {
		t.Errorf("commits = %d, want 1", n.commits)
	}
	wantStatements(t, f)
}
