package rest

// Tests for base.go, and the fixture every test file of this package
// shares: a fakeService, a CRUD over it whose caller is the user the
// X-User header names, and an app serving that CRUD under /r with
// server.ErrorHandler installed, so every status and message a test
// asserts is what a client receives.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/mas-ony/go-toolkit/crud"
	"github.com/mas-ony/go-toolkit/internal/testutil"
	"github.com/mas-ony/go-toolkit/request"
	"github.com/mas-ony/go-toolkit/server"
	"github.com/rs/zerolog"
)

// row is a row of the fake table: an id, the user who owns it and a
// name.
type row struct {
	ID    *int    `json:"id"`
	Owner *int    `json:"owner"`
	Name  *string `json:"name"`
}

// rowOption is the read option of fakeService.
type rowOption struct {
	crud.ListOption
	Name *string
}

// rowRequest is the body of POST and PUT, and one item of POST /batch.
type rowRequest struct {
	Owner *int    `json:"owner"`
	Name  *string `json:"name"`
}

// rowBatch is the body of POST /batch.
type rowBatch struct {
	Parent *int         `json:"parent"`
	Items  []rowRequest `json:"items"`
}

// fakeService is a crud.Service over rows. Each method fails with its
// error field when that is set, and records that it ran and what it was
// given.
type fakeService struct {
	rows map[int]*row

	createErr, readErr, readByIDErr, updateErr, deleteErr error

	calls   []string
	opt     rowOption
	written *row
	batch   []row
}

// result is a decoded response.
type result struct {
	status  int
	raw     string
	Success bool            `json:"success"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

var _ crud.Service[row, rowOption] = (*fakeService)(nil)

// newHandler returns a CRUD over svc whose caller is the user the X-User
// header names. Its filter reads ?name=, and its decode requires a name
// and takes the owner on POST only.
func newHandler(
	log zerolog.Logger,
	svc *fakeService,
) *CRUD[row, *row, rowOption] {
	b := NewBase(log, "row", "row")
	b.Caller = func(c fiber.Ctx) int {
		n, _ := strconv.Atoi(c.Get("X-User"))
		return n
	}
	filter := func(c fiber.Ctx, lo crud.ListOption) (rowOption, error) {
		return rowOption{ListOption: lo,
			Name: request.StringPtr(c, "name")}, nil
	}
	decode := func(c fiber.Ctx, create bool) (*row, error) {
		var req rowRequest
		if err := b.Bind(c, &req); err != nil {
			return nil, err
		}
		if err := Required(req.Name, "name"); err != nil {
			return nil, err
		}
		r := &row{Name: req.Name}
		if create {
			r.Owner = req.Owner
		}
		return r, nil
	}
	return NewCRUD(b, svc, filter, decode)
}

// newApp returns an app that renders errors with server.ErrorHandler and
// serves h under /r, with POST /r/batch over svc.
func newApp(h *CRUD[row, *row, rowOption], svc *fakeService) *fiber.App {
	app := fiber.New(fiber.Config{
		ErrorHandler: server.ErrorHandler(zerolog.Nop(), "request_id"),
	})
	app.Post("/r/batch", func(c fiber.Ctx) error {
		var req rowBatch
		if err := h.Bind(c, &req); err != nil {
			return err
		}
		rows, err := BatchRows(req.Items, req.Parent, "parent",
			func(item rowRequest) row {
				return row{Owner: req.Parent, Name: item.Name}
			})
		if err != nil {
			return err
		}
		return CreateBatch(c, h.Base, svc.CreateBatch, rows)
	})
	app.Post("/r", h.Post)
	app.Get("/r", h.Get)
	app.Get("/r/:id", h.GetByID)
	app.Put("/r/:id", h.Put)
	app.Delete("/r/:id", h.Delete)
	return app
}

// send sends one request to app as the user named, "" for none, with
// body as written, and decodes the envelope it gets back.
func send(
	t *testing.T,
	app *fiber.App,
	user, method, path, body string,
) result {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if user != "" {
		req.Header.Set("X-User", user)
	}
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	res := result{status: resp.StatusCode, raw: string(raw)}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("%s %s: %v: %s", method, path, err, raw)
	}
	return res
}

// expect fails t unless res carries status and msg.
func expect(t *testing.T, res result, status int, msg string) {
	t.Helper()
	if res.status != status || res.Message != msg {
		t.Errorf("got %d %q, want %d %q (%s)",
			res.status, res.Message, status, msg, res.raw)
	}
}

// serveRow returns a fakeService holding row 7, which user 4 owns, and an
// app serving a CRUD over it that logs nowhere.
func serveRow() (*fakeService, *fiber.App) {
	svc := &fakeService{rows: map[int]*row{
		7: {ID: new(7), Owner: new(4), Name: new("seven")},
	}}
	return svc, newApp(newHandler(zerolog.Nop(), svc), svc)
}

// called reports whether the method name ran.
func (f *fakeService) called(name string) bool {
	return slices.Contains(f.calls, name)
}

func (f *fakeService) Create(_ context.Context, r *row) (*row, error) {
	f.calls = append(f.calls, "Create")
	if f.createErr != nil {
		return nil, f.createErr
	}
	r.SetID(100)
	f.written = r
	return r, nil
}

func (f *fakeService) CreateBatch(
	_ context.Context,
	rows []row,
) ([]*row, error) {
	f.calls = append(f.calls, "CreateBatch")
	if f.createErr != nil {
		return nil, f.createErr
	}
	f.batch = rows
	out := make([]*row, len(rows))
	for i := range rows {
		out[i] = &rows[i]
	}
	return out, nil
}

func (f *fakeService) Read(
	_ context.Context,
	opt rowOption,
) ([]row, int, error) {
	f.calls = append(f.calls, "Read")
	f.opt = opt
	if f.readErr != nil {
		return nil, 0, f.readErr
	}
	var items []row
	for _, r := range f.rows {
		items = append(items, *r)
	}
	return items, len(items), nil
}

func (f *fakeService) ReadByID(_ context.Context, id int) (*row, error) {
	f.calls = append(f.calls, "ReadByID")
	if f.readByIDErr != nil {
		return nil, f.readByIDErr
	}
	return f.rows[id], nil
}

func (f *fakeService) Update(_ context.Context, r *row) error {
	f.calls = append(f.calls, "Update")
	f.written = r
	return f.updateErr
}

func (f *fakeService) Delete(_ context.Context, _ int) error {
	f.calls = append(f.calls, "Delete")
	return f.deleteErr
}

// SetID takes the id a route or an insert gives the row.
func (r *row) SetID(id int) { r.ID = &id }

// A body that does not decode is answered with Bind's 400, and one that
// leaves out a field the handler needs with Required's.
func TestBindAndRequired(t *testing.T) {
	_, app := serveRow()

	expect(t, send(t, app, "4", "POST", "/r", `{`),
		400, "invalid request body")
	expect(t, send(t, app, "4", "POST", "/r", `{"owner":4}`),
		400, "name is required")
}

// A 500 is logged once at error level with its cause, and a refusal at
// warn level with the caller.
func TestLogging(t *testing.T) {
	var buf bytes.Buffer
	svc := &fakeService{
		rows:      map[int]*row{7: {ID: new(7), Owner: new(4)}},
		deleteErr: errors.New("disk on fire"),
	}
	h := newHandler(zerolog.New(&buf), svc)
	h.Guard = RequireOwner(h.Base, svc.ReadByID,
		func(r *row) *int { return r.Owner }, nil)
	app := newApp(h, svc)

	send(t, app, "4", "DELETE", "/r/7", "")
	send(t, app, "5", "DELETE", "/r/7", "")
	lines := testutil.LogLines(t, buf.Bytes())
	if len(lines) != 2 {
		t.Fatalf("%d log lines, want 2: %s", len(lines), buf.Bytes())
	}
	failed, denied := lines[0], lines[1]
	if failed["level"] != "error" || failed["error"] != "disk on fire" ||
		failed["message"] != "failed to delete row" ||
		failed["handler"] != "row" || failed["method"] != "DELETE" ||
		failed["path"] != "/r/7" {
		t.Errorf("500 log: %v", failed)
	}
	if denied["level"] != "warn" || denied["caller"] != float64(5) ||
		denied["message"] != "row denied: not owned by the caller" {
		t.Errorf("403 log: %v", denied)
	}
}
