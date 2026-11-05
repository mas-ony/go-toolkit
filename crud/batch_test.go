package crud

// Tests for batch.go, against the fake driver of repo_test.go, which
// counts how the transactions begun on it end.

import (
	"context"
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

// newEntryRepo returns an entryRepository over db.
func newEntryRepo(db *sqlx.DB) entryRepo {
	return &entryRepository{NewBaseRepo(db, &entrySpec,
		func(b BaseRepo[entry, int, entryRepo]) entryRepo {
			return &entryRepository{b}
		})}
}

// SetID stores the key CreateAll writes back.
func (e *entry) SetID(id int) { e.ID = &id }

// Every item is inserted inside one transaction, which commits, and gets
// its generated key back.
func TestCreateAllCommitsOneTransaction(t *testing.T) {
	grammars(t, func(t *testing.T, name string) {
		f := &fakeDB{lastID: 7}
		db := f.open(t, name)

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
		if commits, rollbacks := f.ended(); commits != 1 || rollbacks != 0 {
			t.Errorf("commits, rollbacks = %d, %d; want 1, 0",
				commits, rollbacks)
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
		db := f.open(t, name)

		created, err := CreateAll(context.Background(), db,
			newEntryRepo(db), []entry{{Code: new("a")}})
		if !errors.Is(err, errFake) ||
			err.Error() != "entry.Create: "+errFake.Error() {
			t.Fatalf("err = %v, want entry.Create wrapping errFake", err)
		}
		if created != nil {
			t.Errorf("created = %v, want nil", created)
		}
		if commits, rollbacks := f.ended(); commits != 0 || rollbacks != 1 {
			t.Errorf("commits, rollbacks = %d, %d; want 0, 1",
				commits, rollbacks)
		}
	})
}

// An empty batch still opens and commits its transaction, and returns no
// rows rather than an error.
func TestCreateAllEmpty(t *testing.T) {
	f := &fakeDB{}
	db := f.open(t, "mysql")

	created, err := CreateAll(context.Background(), db, newEntryRepo(db),
		[]entry(nil))
	if err != nil || len(created) != 0 {
		t.Fatalf("CreateAll(nil) = %v, %v; want no rows, nil",
			created, err)
	}
	if commits, _ := f.ended(); commits != 1 {
		t.Errorf("commits = %d, want 1", commits)
	}
	wantStatements(t, f)
}
