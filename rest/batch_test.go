package rest

// Tests for batch.go: BatchRows and CreateBatch behind POST /r/batch,
// over the fixture of base_test.go.

import (
	"errors"
	"testing"

	"github.com/rs/zerolog"
)

// BatchRows refuses an empty batch and one without its parent, in that
// order, and builds one row per item under the parent; CreateBatch
// answers 201 with the rows, or 500 when the service fails.
func TestBatch(t *testing.T) {
	svc := &fakeService{}
	app := newApp(newHandler(zerolog.Nop(), svc), svc)

	expect(t, send(t, app, "4", "POST", "/r/batch", `{"parent":9}`),
		400, "items must not be empty")
	expect(t, send(t, app, "4", "POST", "/r/batch", `{"items":[{}]}`),
		400, "parent is required")
	if svc.called("CreateBatch") {
		t.Fatal("a refused batch reached the service")
	}

	res := send(t, app, "4", "POST", "/r/batch",
		`{"parent":9,"items":[{"name":"a"},{"name":"b","owner":3}]}`)
	expect(t, res, 201, "created")
	if len(svc.batch) != 2 || *svc.batch[1].Owner != 9 ||
		*svc.batch[1].Name != "b" {
		t.Errorf("batch rows: %+v", svc.batch)
	}

	svc.createErr = errors.New("down")
	expect(t, send(t, app, "4", "POST", "/r/batch",
		`{"parent":9,"items":[{"name":"a"}]}`), 500, "failed to create row")
}
