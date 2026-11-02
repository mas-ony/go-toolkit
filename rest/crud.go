package rest

import (
	"github.com/gofiber/fiber/v3"
	"github.com/mas-ony/go-toolkit/crud"
	"github.com/mas-ony/go-toolkit/request"
)

// CRUD serves the five endpoints of a table whose service is a
// crud.Service: GET and DELETE /:id from Table, and POST, GET /:id and
// PUT /:id. T is the row, PT its pointer type, which takes the id from
// the route, and O the service's read option.
type CRUD[T any, PT crud.Keyed[T], O any] struct {
	*Table[T, O]

	// svc is the table's service.
	svc crud.Service[T, O]
	// decode reads the body of POST and PUT; see NewCRUD.
	decode func(c fiber.Ctx, create bool) (*T, error)
}

// NewCRUD returns a CRUD over svc, with filter as Table's. decode binds
// the body of POST (create true) or PUT, usually with Base.Bind, checks
// it, and returns the row it describes or the error to answer with. On
// PUT the id comes from the route, so decode leaves it out.
//
// PT and O are inferred, and T as well when decode or svc names it.
func NewCRUD[T any, PT crud.Keyed[T], O any](
	b Base,
	svc crud.Service[T, O],
	filter func(c fiber.Ctx, lo crud.ListOption) (O, error),
	decode func(c fiber.Ctx, create bool) (*T, error),
) *CRUD[T, PT, O] {
	return &CRUD[T, PT, O]{
		Table:  NewTable(b, svc.Read, svc.Delete, filter),
		svc:    svc,
		decode: decode,
	}
}

// Post handles POST on the collection and answers 201 "created" with the
// row as the service returns it, its generated id included.
func (h *CRUD[T, PT, O]) Post(c fiber.Ctx) error {
	row, err := h.decode(c, true)
	if err != nil {
		return err
	}
	saved, err := h.svc.Create(c.Context(), row)
	return h.created(c, saved, err)
}

// GetByID handles GET /:id: the row, or 404 with the Missing message.
func (h *CRUD[T, PT, O]) GetByID(c fiber.Ctx) error {
	id, err := request.ID(c, "id")
	if err != nil {
		return err
	}
	row, err := fetch(c, h.Base, id, h.svc.ReadByID)
	if err != nil {
		return err
	}
	return OK(c, row)
}

// Put handles PUT /:id once the Guard admits the caller, and answers 200
// "updated" with the row as written. The service writes every updatable
// column, so a field the body leaves out is stored as NULL.
func (h *CRUD[T, PT, O]) Put(c fiber.Ctx) error {
	id, err := h.guarded(c)
	if err != nil {
		return err
	}
	row, err := h.decode(c, false)
	if err != nil {
		return err
	}
	PT(row).SetID(id)
	if err := h.svc.Update(c.Context(), row); err != nil {
		return h.Fail(c, err, "update")
	}
	return Reply(c, fiber.StatusOK, "updated", row)
}
