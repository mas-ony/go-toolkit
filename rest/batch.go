package rest

import (
	"context"

	"github.com/gofiber/fiber/v3"
)

// BatchRows checks the envelope of a POST /batch and builds one row per
// item with row. items must not be empty, and parent, the id every row
// goes under, sent once in the envelope as the parameter name, is
// required; each check answers 400 when it fails, in that order.
//
//	rows, err := rest.BatchRows(req.Items, req.MasukID, "masuk_id",
//		func(item dispRequest) Disp {
//			return Disp{MasukID: req.MasukID, KepadaID: item.KepadaID}
//		})
func BatchRows[I, T any](
	items []I,
	parent *int,
	name string,
	row func(item I) T,
) ([]T, error) {
	if len(items) == 0 {
		return nil, BadRequest("items must not be empty")
	}
	if err := Required(parent, name); err != nil {
		return nil, err
	}
	rows := make([]T, 0, len(items))
	for _, item := range items {
		rows = append(rows, row(item))
	}
	return rows, nil
}

// CreateBatch creates rows through create, which inserts them in one
// transaction, as a service over crud.CreateAll does, and answers 201
// "created" with the rows created.
func CreateBatch[T any](
	c fiber.Ctx,
	b Base,
	create func(ctx context.Context, rows []T) ([]*T, error),
	rows []T,
) error {
	created, err := create(c.Context(), rows)
	return b.created(c, created, err)
}
