package rest

import (
	"context"
	"database/sql"
	"errors"

	"github.com/gofiber/fiber/v3"
	"github.com/mas-ony/go-toolkit/internal/logfield"
	"github.com/mas-ony/go-toolkit/response"
	"github.com/rs/zerolog"
)

// Base is embedded by every handler: its logger, the noun its messages
// are built from, and how to tell who the caller is.
type Base struct {
	// Log carries the handler's name under the "handler" key.
	Log zerolog.Logger
	// Entity names a row in messages: "failed to create <Entity>".
	Entity string
	// Missing is the 404 message, "<Entity> not found" unless set to
	// another.
	Missing string
	// Caller returns the id of the user the request is for, 0 when there
	// is none. Deny logs it, and RequireOwner compares it with the id of
	// a row's owner. Nil logs no caller.
	Caller func(c fiber.Ctx) int
}

// logAt writes msg on e with what every line Base logs carries beside the
// handler, the method and the path of the request, so Internal, Deny and
// Bind place a line the same way. The keys are logfield's, the ones
// server's lines carry them under too.
func logAt(e *zerolog.Event, c fiber.Ctx, msg string) {
	logfield.Request(e, c.Method(), c.Path()).Msg(msg)
}

// NewBase returns the Base of the handler logged as name, whose rows are
// called entity in its messages.
func NewBase(log zerolog.Logger, name, entity string) Base {
	return Base{
		Log:     logfield.Handler(log, name),
		Entity:  entity,
		Missing: entity + " not found",
	}
}

// Fetch reads the row key names through read, for an endpoint that
// serves or checks one row. A failure is answered through Fail, as
// "failed to fetch <Entity>", and a row that does not exist with
// NotFound, so GetByID, the guard RequireOwner builds and an endpoint of
// a handler's own refuse a key in the same words. key is whatever the
// table is keyed by: an id from request.ID, or a natural key such as a
// date.
func Fetch[K, T any](
	c fiber.Ctx,
	b Base,
	key K,
	read func(ctx context.Context, key K) (*T, error),
) (*T, error) {
	row, err := read(c.Context(), key)
	if err != nil {
		return nil, b.Fail(c, err, "fetch")
	}
	if row == nil {
		return nil, b.NotFound()
	}
	return row, nil
}

// BadRequest answers 400 with msg.
func BadRequest(msg string) error {
	return fiber.NewError(fiber.StatusBadRequest, msg)
}

// Required answers 400 "<name> is required" when v is nil or holds the
// zero value of its type: an empty string, or an id of 0. cmp.Or answers
// a body that fails several checks with the first in its list:
//
//	err := cmp.Or(rest.Required(req.Kode, "kode"),
//		rest.Required(req.Nama, "nama"))
func Required[V comparable](v *V, name string) error {
	var zero V
	if v == nil || *v == zero {
		return BadRequest(name + " is required")
	}
	return nil
}

// Reply answers status with the success envelope.
func Reply(c fiber.Ctx, status int, msg string, data any) error {
	return c.Status(status).JSON(response.OK(msg, data))
}

// OK answers 200 "ok" with data.
func OK(c fiber.Ctx, data any) error {
	return Reply(c, fiber.StatusOK, "ok", data)
}

// created answers a create: its failure through Fail, or 201 "created"
// with data, what it created. Post and CreateBatch both answer through
// it, so a row and a batch are reported alike.
func (b Base) created(c fiber.Ctx, data any, err error) error {
	if err != nil {
		return b.Fail(c, err, "create")
	}
	return Reply(c, fiber.StatusCreated, "created", data)
}

// Fail answers an error from the service: 404 when no row matched, which
// a service reports as sql.ErrNoRows, and otherwise 500 with the message
// "failed to <action> <Entity>".
func (b Base) Fail(c fiber.Ctx, err error, action string) error {
	return b.FailWith(c, err, "failed to "+action+" "+b.Entity)
}

// FailWith is Fail with the 500 message spelled out.
func (b Base) FailWith(c fiber.Ctx, err error, msg string) error {
	if errors.Is(err, sql.ErrNoRows) {
		return b.NotFound()
	}
	return b.Internal(c, err, msg)
}

// Internal logs err and answers 500 with msg, which is all the client
// learns of it.
func (b Base) Internal(c fiber.Ctx, err error, msg string) error {
	logAt(b.Log.Error().Err(err), c, msg)
	return fiber.NewError(fiber.StatusInternalServerError, msg)
}

// NotFound answers 404 with the Missing message.
func (b Base) NotFound() error {
	return fiber.NewError(fiber.StatusNotFound, b.Missing)
}

// Deny logs why the caller was refused, at warn level, and answers
// status with msg: 403 for a refusal the caller may know about, or 404
// with Missing for one that must not reveal that the row exists.
func (b Base) Deny(c fiber.Ctx, status int, msg, why string) error {
	e := b.Log.Warn()
	if b.Caller != nil {
		e = e.Int("caller", b.Caller(c))
	}
	logAt(e, c, why)
	return fiber.NewError(status, msg)
}

// Bind decodes the JSON body of the request into req, and answers 400
// "invalid request body" when it does not decode.
func (b Base) Bind(c fiber.Ctx, req any) error {
	if err := c.Bind().JSON(req); err != nil {
		logAt(b.Log.Warn().Err(err), c, "failed to bind request body")
		return BadRequest("invalid request body")
	}
	return nil
}
