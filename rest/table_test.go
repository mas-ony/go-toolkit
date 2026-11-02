package rest

// Tests for table.go: GET on the collection and DELETE /:id, over the
// fixture of base_test.go.

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
)

// GET answers one page, read with the option filter builds around the
// page and limit, and a service failure with a 500 that names the list.
func TestGet(t *testing.T) {
	svc, app := serveRow()

	expect(t, send(t, app, "4", "GET", "/r?name=sev&page=2", ""),
		200, "ok")
	if svc.opt.Name == nil || *svc.opt.Name != "sev" ||
		svc.opt.Page != 2 || svc.opt.Limit != 10 {
		t.Errorf("read option: %+v", svc.opt)
	}

	svc.readErr = errors.New("down")
	expect(t, send(t, app, "4", "GET", "/r", ""),
		500, "failed to fetch row list")
}

// DELETE answers 200, a row the service did not match with 404, and a
// service failure with a 500 that names what failed and nothing more.
func TestDelete(t *testing.T) {
	svc, app := serveRow()

	expect(t, send(t, app, "4", "DELETE", "/r/7", ""), 200, "deleted")
	svc.deleteErr = errors.New("fk constraint on secret_table")
	res := send(t, app, "4", "DELETE", "/r/7", "")
	expect(t, res, 500, "failed to delete row")
	if strings.Contains(res.raw, "secret_table") {
		t.Errorf("the cause reached the client: %s", res.raw)
	}
	svc.deleteErr = sql.ErrNoRows
	expect(t, send(t, app, "4", "DELETE", "/r/7", ""),
		404, "row not found")
}
