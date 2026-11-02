package rest

// Tests for guard.go: RequireOwner, over the fixture of base_test.go.

import (
	"errors"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/mas-ony/go-toolkit/internal/testutil"
	"github.com/rs/zerolog"
)

// RequireOwner lets admit's callers and a row's owner write, refuses
// everyone else with 403 before the service is reached, answers a
// missing row with 404, and runs before PUT reads its body.
func TestRequireOwner(t *testing.T) {
	newSvc := func() *fakeService {
		return &fakeService{rows: map[int]*row{
			7: {ID: new(7), Owner: new(4)},
			9: {ID: new(9), Owner: new(0)},
		}}
	}
	routes := func(svc *fakeService) *fiber.App {
		h := newHandler(zerolog.Nop(), svc)
		h.Guard = RequireOwner(h.Base, svc.ReadByID,
			func(r *row) *int { return r.Owner },
			func(c fiber.Ctx) bool { return c.Get("X-User") == "1" })
		return newApp(h, svc)
	}

	svc := newSvc()
	app := routes(svc)
	body := `{"name":"b"}`
	expect(t, send(t, app, "5", "PUT", "/r/7", body), 403, "forbidden")
	expect(t, send(t, app, "5", "DELETE", "/r/7", ""), 403, "forbidden")
	expect(t, send(t, app, "5", "PUT", "/r/7", `{`), 403, "forbidden")
	expect(t, send(t, app, "", "PUT", "/r/9", body), 403, "forbidden")
	if svc.called("Update") || svc.called("Delete") {
		t.Fatalf("a refused write reached the service: %v", svc.calls)
	}
	expect(t, send(t, app, "5", "PUT", "/r/8", body), 404, "row not found")

	expect(t, send(t, app, "4", "PUT", "/r/7", body), 200, "updated")
	expect(t, send(t, app, "4", "DELETE", "/r/7", ""), 200, "deleted")

	svc = newSvc()
	app = routes(svc)
	expect(t, send(t, app, "1", "DELETE", "/r/7", ""), 200, "deleted")
	if svc.called("ReadByID") {
		t.Error("a caller admit lets through had the row read")
	}

	svc.readByIDErr = errors.New("down")
	expect(t, send(t, app, "5", "PUT", "/r/7", body),
		500, "failed to fetch row")
}

// RequireOwner refuses to build without a way to tell the caller.
func TestRequireOwnerNeedsCaller(t *testing.T) {
	b := NewBase(zerolog.Nop(), "row", "row")
	testutil.MustPanic(t, "RequireOwner without Caller", func() {
		RequireOwner(b, (&fakeService{}).ReadByID,
			func(r *row) *int { return r.Owner }, nil)
	})
}
