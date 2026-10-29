package crud

import (
	"context"

	"github.com/rs/zerolog"
)

// Keyed is satisfied by a pointer to a model that can take the int key
// the database generated for it, so generic code can set the key an
// INSERT returned on the struct it inserted.
type Keyed[T any] interface {
	*T
	SetID(id int)
}

// Repository is the part of a repository BaseService needs. A repository
// built on a BaseRepo keyed by an int satisfies it with the promoted
// Create, Update and Delete and a Read of its own, whose option type is
// O.
type Repository[T, O any] interface {
	// Create inserts e and returns the key the database generated.
	Create(ctx context.Context, e *T) (int, error)
	// Read fetches the rows opt selects.
	Read(ctx context.Context, opt O) (ReadResult[T], error)
	// Update writes the row e describes and returns sql.ErrNoRows when
	// no row matched.
	Update(ctx context.Context, e *T) error
	// Delete removes the row whose key is id and returns sql.ErrNoRows
	// when no row matched.
	Delete(ctx context.Context, id int) error
}

// Service is the service contract of a table keyed by an int the database
// generates. A service with nothing extra declares itself as an alias of
// it; one with extra methods embeds it in its own interface.
type Service[T, O any] interface {
	// Create inserts a new row and returns it with the generated id.
	Create(ctx context.Context, e *T) (*T, error)
	// Read fetches rows according to opt and returns them with the
	// total: across all pages when paginating, len(items) otherwise.
	Read(ctx context.Context, opt O) ([]T, int, error)
	// ReadByID returns a single row or nil if not found.
	ReadByID(ctx context.Context, id int) (*T, error)
	// Update persists changes to all mutable fields of the row and
	// returns sql.ErrNoRows when no row matched.
	Update(ctx context.Context, e *T) error
	// Delete removes the row identified by id and returns sql.ErrNoRows
	// when no row matched.
	Delete(ctx context.Context, id int) error
}

// BaseService implements Service over a Repository. A service with extra
// behaviour embeds *BaseService and adds methods, or declares one of the
// same name, which shadows the promoted one; the fields are exported for
// those methods to use.
//
// T is the model, PT its pointer type, which takes the generated id, and
// O the repository's read option.
type BaseService[T any, PT Keyed[T], O any] struct {
	// Entity names the table's rows in the log, under the "entity" key.
	Entity string
	// Log receives one Info line per row written.
	Log zerolog.Logger
	// Repo is the persistence layer for the table.
	Repo Repository[T, O]
	// byID builds the read option of a single-row lookup.
	byID func(id int) O
}

// NewBaseService returns a BaseService over repo whose rows the log calls
// entity. byID builds the read option of a lookup by id, which ReadByID
// runs. Only T needs to be given explicitly; PT and O are inferred from
// it and from byID.
func NewBaseService[T any, PT Keyed[T], O any](
	entity string,
	log zerolog.Logger,
	repo Repository[T, O],
	byID func(id int) O,
) *BaseService[T, PT, O] {
	return &BaseService[T, PT, O]{
		Entity: entity,
		Log:    log,
		Repo:   repo,
		byID:   byID,
	}
}

// Unpack spreads a repository read into the (items, total, err) triple
// every service Read returns, so a custom Read is a one-liner:
//
//	return crud.Unpack(s.repo.Read(ctx, opt))
func Unpack[T any](r ReadResult[T], err error) ([]T, int, error) {
	if err != nil {
		return nil, 0, err
	}
	return r.Items, r.Total, nil
}

// FirstOf returns the first row of a repository read, or nil when the
// read matched nothing. Callers must check for nil before dereferencing.
func FirstOf[T any](r ReadResult[T], err error) (*T, error) {
	if err != nil {
		return nil, err
	}
	if len(r.Items) == 0 {
		return nil, nil
	}
	return &r.Items[0], nil
}

// CreateEach inserts items one by one through create, writes each
// generated id back, and returns pointers into items. Run it inside a
// transaction and pass a create bound to the tx-scoped repository.
//
// items is iterated by index so the id lands on the caller's backing
// array. On error the ids written before the failure stay on items even
// though the transaction rolled them back, so items must not be reused.
func CreateEach[T any, PT Keyed[T]](
	items []T,
	create func(e *T) (int, error),
) ([]*T, error) {
	created := make([]*T, 0, len(items))
	for i := range items {
		id, err := create(&items[i])
		if err != nil {
			return nil, err
		}
		PT(&items[i]).SetID(id)
		created = append(created, &items[i])
	}
	return created, nil
}

// Create inserts a new row and returns it with the generated id.
func (s *BaseService[T, PT, O]) Create(
	ctx context.Context,
	e *T,
) (*T, error) {
	id, err := s.Repo.Create(ctx, e)
	if err != nil {
		return nil, err
	}
	PT(e).SetID(id)
	s.Log.Info().Str("entity", s.Entity).Int("id", id).Msg("created")
	return e, nil
}

// Read fetches rows according to opt.
func (s *BaseService[T, PT, O]) Read(
	ctx context.Context,
	opt O,
) ([]T, int, error) {
	return Unpack(s.Repo.Read(ctx, opt))
}

// ReadByID returns a single row or nil if not found.
func (s *BaseService[T, PT, O]) ReadByID(
	ctx context.Context,
	id int,
) (*T, error) {
	return FirstOf(s.Repo.Read(ctx, s.byID(id)))
}

// Update persists changes to all mutable fields of the row.
func (s *BaseService[T, PT, O]) Update(ctx context.Context, e *T) error {
	if err := s.Repo.Update(ctx, e); err != nil {
		return err
	}
	s.Log.Info().Str("entity", s.Entity).Msg("updated")
	return nil
}

// Delete removes the row identified by id.
func (s *BaseService[T, PT, O]) Delete(ctx context.Context, id int) error {
	if err := s.Repo.Delete(ctx, id); err != nil {
		return err
	}
	s.Log.Info().Str("entity", s.Entity).Int("id", id).Msg("deleted")
	return nil
}
