package rest

// Tests for crud.go: POST on the collection, GET /:id and PUT /:id, over
// the fixture of base_test.go.

import (
	"database/sql"
	"errors"
	"testing"
)

// POST answers 201 with the row as the service returns it, its generated
// id included.
func TestPost(t *testing.T) {
	svc, app := serveRow()

	res := send(t, app, "4", "POST", "/r", `{"name":"a","owner":4}`)
	expect(t, res, 201, "created")
	if svc.written == nil || *svc.written.ID != 100 ||
		*svc.written.Owner != 4 {
		t.Errorf("created row: %s", res.raw)
	}
}

// GET /:id answers the row, a missing one with 404, an id that is not
// one with request.ID's 400, and a service failure with a 500 that names
// what failed.
func TestGetByID(t *testing.T) {
	svc, app := serveRow()

	expect(t, send(t, app, "4", "GET", "/r/7", ""), 200, "ok")
	expect(t, send(t, app, "4", "GET", "/r/8", ""), 404, "row not found")
	expect(t, send(t, app, "4", "GET", "/r/x", ""), 400, "invalid id")

	svc.readByIDErr = errors.New("down")
	expect(t, send(t, app, "4", "GET", "/r/7", ""),
		500, "failed to fetch row")
}

// PUT writes the row under the id of its route, leaving out the owner
// decode keeps for POST, and answers a row the service did not match
// with 404.
func TestPut(t *testing.T) {
	svc, app := serveRow()

	expect(t, send(t, app, "4", "PUT", "/r/7", `{"name":"b"}`),
		200, "updated")
	if *svc.written.ID != 7 || svc.written.Owner != nil {
		t.Errorf("PUT wrote %+v", svc.written)
	}
	svc.updateErr = sql.ErrNoRows
	expect(t, send(t, app, "4", "PUT", "/r/7", `{"name":"b"}`),
		404, "row not found")
}
