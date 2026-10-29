package crud

// Tests for service.go: BaseService over a fake repository, which shows
// what it hands on and what it logs, and once over a real BaseRepo, which
// shows the two layers fit; then Unpack, FirstOf and CreateEach.

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

// fakeRepo is a Repository over item that records the read options it is
// handed and answers from canned values.
type fakeRepo struct {
	id    int
	read  ReadResult[item]
	err   error
	opts  []itemReadOption
	calls []string
}

// itemByID is the read option of a lookup by id.
func itemByID(id int) itemReadOption {
	return itemReadOption{ReadOption: ReadOption[int]{ID: &id}}
}

// newService returns a BaseService over repo that logs into buf.
func newService(
	repo Repository[item, itemReadOption],
	buf *bytes.Buffer,
) Service[item, itemReadOption] {
	return NewBaseService[item]("item", zerolog.New(buf), repo, itemByID)
}

// SetID sets the generated key, which makes *item Keyed.
func (i *item) SetID(id int) { i.ID = &id }

// Create records the call and returns the canned key.
func (r *fakeRepo) Create(_ context.Context, _ *item) (int, error) {
	r.calls = append(r.calls, "Create")
	return r.id, r.err
}

// Read records the option and returns the canned read.
func (r *fakeRepo) Read(
	_ context.Context,
	opt itemReadOption,
) (ReadResult[item], error) {
	r.calls = append(r.calls, "Read")
	r.opts = append(r.opts, opt)
	return r.read, r.err
}

// Update records the call.
func (r *fakeRepo) Update(context.Context, *item) error {
	r.calls = append(r.calls, "Update")
	return r.err
}

// Delete records the call.
func (r *fakeRepo) Delete(context.Context, int) error {
	r.calls = append(r.calls, "Delete")
	return r.err
}

// A write that succeeds sets the generated id and logs one Info line
// under the entity key; one that fails logs nothing and hands the error
// back as it came.
func TestBaseServiceWrites(t *testing.T) {
	ctx := context.Background()
	var buf bytes.Buffer
	repo := &fakeRepo{id: 7}
	svc := newService(repo, &buf)

	e := &item{Code: new("A-1")}
	got, err := svc.Create(ctx, e)
	if err != nil || got != e || e.ID == nil || *e.ID != 7 {
		t.Fatalf("Create = %+v, %v; want e with id 7", got, err)
	}
	if err := svc.Update(ctx, e); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if err := svc.Delete(ctx, 7); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	want := `{"level":"info","entity":"item","id":7,"message":"created"}
{"level":"info","entity":"item","message":"updated"}
{"level":"info","entity":"item","id":7,"message":"deleted"}
`
	if buf.String() != want {
		t.Errorf("log:\n%s\nwant:\n%s", buf.String(), want)
	}

	buf.Reset()
	repo.err = sql.ErrNoRows
	if _, err := svc.Create(ctx, &item{}); err != sql.ErrNoRows {
		t.Errorf("Create = %v, want the repository's error", err)
	}
	if err := svc.Update(ctx, e); err != sql.ErrNoRows {
		t.Errorf("Update = %v, want the repository's error", err)
	}
	if err := svc.Delete(ctx, 7); err != sql.ErrNoRows {
		t.Errorf("Delete = %v, want the repository's error", err)
	}
	if buf.Len() != 0 {
		t.Errorf("a failed write logged:\n%s", buf.String())
	}
}

// Read hands the option on and spreads the result; ReadByID reads with
// the option byID builds and returns the first row, or nil.
func TestBaseServiceReads(t *testing.T) {
	ctx := context.Background()
	var buf bytes.Buffer
	repo := &fakeRepo{read: ReadResult[item]{
		Items: []item{{ID: new(3)}, {ID: new(4)}},
		Total: 9,
	}}
	svc := newService(repo, &buf)

	opt := itemReadOption{Code: new("A")}
	items, total, err := svc.Read(ctx, opt)
	if err != nil || len(items) != 2 || total != 9 {
		t.Errorf("Read = %d items, total %d, %v", len(items), total, err)
	}
	row, err := svc.ReadByID(ctx, 3)
	if err != nil || row == nil || *row.ID != 3 {
		t.Errorf("ReadByID = %+v, %v; want the first row", row, err)
	}
	if len(repo.opts) != 2 || *repo.opts[0].Code != "A" ||
		repo.opts[1].ID == nil || *repo.opts[1].ID != 3 {
		t.Errorf("options handed on: %+v", repo.opts)
	}

	repo.read = ReadResult[item]{}
	if row, err := svc.ReadByID(ctx, 5); row != nil || err != nil {
		t.Errorf("missing row: ReadByID = %+v, %v; want nil, nil",
			row, err)
	}
	if buf.Len() != 0 {
		t.Errorf("a read logged:\n%s", buf.String())
	}
}

// The two layers fit as a service wires them: a BaseService over a
// repository built on BaseRepo, its Create returning the key the driver
// reported.
func TestBaseServiceOverBaseRepo(t *testing.T) {
	f := &fakeDB{lastID: 11}
	var buf bytes.Buffer
	svc := newService(newItemRepo(f.open(t, "mysql")), &buf)

	e, err := svc.Create(context.Background(),
		&item{Code: new("B-1"), Title: new("b")})
	if err != nil || *e.ID != 11 {
		t.Fatalf("Create = %+v, %v; want id 11", e, err)
	}
	if !strings.Contains(buf.String(), `"id":11`) {
		t.Errorf("log does not carry the key: %s", buf.String())
	}
}

// Unpack and FirstOf hand an error on with nothing beside it, and FirstOf
// reads an empty result as no row.
func TestUnpackAndFirstOf(t *testing.T) {
	res := ReadResult[item]{Items: []item{{ID: new(1)}}, Total: 4}

	items, total, err := Unpack(res, nil)
	if len(items) != 1 || total != 4 || err != nil {
		t.Errorf("Unpack = %v, %d, %v", items, total, err)
	}
	items, total, err = Unpack(res, errFake)
	if items != nil || total != 0 || err != errFake {
		t.Errorf("Unpack with error = %v, %d, %v", items, total, err)
	}

	if row, err := FirstOf(res, nil); row != &res.Items[0] || err != nil {
		t.Errorf("FirstOf = %v, %v", row, err)
	}
	if row, err := FirstOf(res, errFake); row != nil || err != errFake {
		t.Errorf("FirstOf with error = %v, %v", row, err)
	}
	if row, err := FirstOf(ReadResult[item]{}, nil); row != nil ||
		err != nil {
		t.Errorf("FirstOf of nothing = %v, %v", row, err)
	}
}

// CreateEach writes each id onto the caller's slice and points into it,
// and stops at the first failure, leaving the ids written before it.
func TestCreateEach(t *testing.T) {
	items := []item{{}, {}, {}}
	next := 0
	create := func(*item) (int, error) {
		next++
		if next == 3 {
			return 0, errFake
		}
		return next * 10, nil
	}

	created, err := CreateEach(items[:2], create)
	if err != nil || len(created) != 2 || created[1] != &items[1] ||
		*items[0].ID != 10 || *items[1].ID != 20 {
		t.Fatalf("CreateEach = %v, %v", created, err)
	}

	next = 1
	created, err = CreateEach(items, create)
	if !errors.Is(err, errFake) || created != nil {
		t.Fatalf("failing CreateEach = %v, %v", created, err)
	}
	if *items[0].ID != 20 || items[2].ID != nil {
		t.Errorf("ids after the failure: %v, %v", items[0].ID, items[2].ID)
	}
}
