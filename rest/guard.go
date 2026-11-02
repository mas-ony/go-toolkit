package rest

import (
	"context"

	"github.com/gofiber/fiber/v3"
)

// RequireOwner returns a Table.Guard that admits whoever admit lets
// through, and anyone else only to the rows they own: owner reads the id
// of a row's owner, and b.Caller gives the caller's. A row that does not
// exist answers 404 with the Missing message. A row the caller does not
// own answers 403 "forbidden", logged through Deny, and so does every row
// for a caller of 0, which is no one. A nil admit lets no one through on
// position alone.
//
// It is for a route that checks the caller's position at most, never the
// row, where any caller the route admits could otherwise edit or delete
// another's row by guessing its id:
//
//	h.Guard = rest.RequireOwner(h.Base, svc.ReadByID,
//		func(d *Disp) *int { return d.DariID },
//		middleware.IsAdminAndOperator)
//
// RequireOwner panics when b.Caller is nil, which is a wiring mistake.
func RequireOwner[T any](
	b Base,
	read func(ctx context.Context, id int) (*T, error),
	owner func(row *T) *int,
	admit func(c fiber.Ctx) bool,
) func(c fiber.Ctx, id int) error {
	if b.Caller == nil {
		panic("rest: RequireOwner needs Base.Caller")
	}
	return func(c fiber.Ctx, id int) error {
		if admit != nil && admit(c) {
			return nil
		}
		row, err := fetch(c, b, id, read)
		if err != nil {
			return err
		}
		caller := b.Caller(c)
		if o := owner(row); caller == 0 || o == nil || *o != caller {
			return b.Deny(c, fiber.StatusForbidden, "forbidden",
				b.Entity+" denied: not owned by the caller")
		}
		return nil
	}
}
