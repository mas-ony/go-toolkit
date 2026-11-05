package rest

import (
	"context"

	"github.com/gofiber/fiber/v3"
	"github.com/mas-ony/go-toolkit/crud"
	"github.com/mas-ony/go-toolkit/request"
	"github.com/mas-ony/go-toolkit/response"
)

// Table serves the two endpoints every id-keyed table has: GET on the
// collection, one page of the rows the query string selects, and DELETE
// /:id. T is the row and O the read option of its service.
type Table[T, O any] struct {
	Base

	// Guard, when set, runs before PUT and DELETE touch the row id, and
	// returns the error to answer with when the caller may not;
	// RequireOwner builds one.
	Guard func(c fiber.Ctx, id int) error

	// read and remove are the service's Read and Delete.
	read   func(ctx context.Context, opt O) ([]T, int, error)
	remove func(ctx context.Context, id int) error
	// filter builds the read option of GET; see NewTable.
	filter func(c fiber.Ctx, lo crud.ListOption) (O, error)
}

// idParam is the route parameter /:id names a row by. Delete and CRUD's
// Put read it through guarded, and CRUD's GetByID reads it directly, so
// every endpoint that addresses one row reads the same parameter.
const idParam = "id"

// NewTable returns a Table over the service methods read and remove.
// filter builds the read option of GET from the query string, around the
// page and limit already parsed into lo, and returns the error to answer
// with for a query it refuses.
func NewTable[T, O any](
	b Base,
	read func(ctx context.Context, opt O) ([]T, int, error),
	remove func(ctx context.Context, id int) error,
	filter func(c fiber.Ctx, lo crud.ListOption) (O, error),
) *Table[T, O] {
	return &Table[T, O]{Base: b, read: read, remove: remove, filter: filter}
}

// guarded reads the id of the row a write to /:id names and runs Check
// on it, and returns the id once the caller may write it, or the error to
// answer with: request.ID's 400 or the Guard's refusal. Delete and CRUD's
// Put open with it, so both hold the Guard to one order, before the row
// is touched and before a body is read.
func (h *Table[T, O]) guarded(c fiber.Ctx) (int, error) {
	id, err := request.ID(c, idParam)
	if err != nil {
		return 0, err
	}
	if err := h.Check(c, id); err != nil {
		return 0, err
	}
	return id, nil
}

// Check runs the Guard, when there is one, on the row id, and returns
// what it returns. Delete and CRUD's Put call it through guarded, and a
// write a handler serves itself calls it directly.
func (h *Table[T, O]) Check(c fiber.Ctx, id int) error {
	if h.Guard == nil {
		return nil
	}
	return h.Guard(c, id)
}

// Get handles GET on the collection: one page of the rows filter
// selects, as a response.Paginated. page and limit are read as
// request.Page reads them; the rest of the query string is filter's.
func (h *Table[T, O]) Get(c fiber.Ctx) error {
	page, limit := request.Page(c)
	lo := crud.ListOption{Page: page, Limit: limit}
	opt, err := h.filter(c, lo)
	if err != nil {
		return err
	}
	return List(c, h.Base, h.read, opt, lo)
}

// Delete handles DELETE /:id once the Guard admits the caller, and
// answers 200 "deleted".
func (h *Table[T, O]) Delete(c fiber.Ctx) error {
	id, err := h.guarded(c)
	if err != nil {
		return err
	}
	if err := h.remove(c.Context(), id); err != nil {
		return h.Fail(c, err, "delete")
	}
	return Reply(c, fiber.StatusOK, "deleted", nil)
}

// List answers a GET on a collection with the page read returns for opt,
// as a response.Paginated. lo carries the page and limit opt was built
// around.
func List[T, O any](
	c fiber.Ctx,
	b Base,
	read func(ctx context.Context, opt O) ([]T, int, error),
	opt O,
	lo crud.ListOption,
) error {
	items, total, err := read(c.Context(), opt)
	if err != nil {
		return b.Internal(c, err, "failed to fetch "+b.Entity+" list")
	}
	return OK(c, response.NewPaginated(items, total, lo.Page, lo.Limit))
}
