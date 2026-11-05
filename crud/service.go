package crud

import (
	"context"

	"github.com/mas-ony/go-toolkit/internal/logfield"
	"github.com/rs/zerolog"
)

// Keyed is satisfied by a pointer to a model that can take the int key
// the database generated for it, so generic code can set the key an
// INSERT returned on the struct it inserted.
type Keyed[T any] interface {
	*T
	SetID(id int)
}

// RepositoryOf is the part of a repository BaseServiceOf needs, for a
// table whose key has the Go type K. A repository built on a BaseRepo
// satisfies it with the promoted Create, Update and Delete and a Read of
// its own, whose option type is O.
type RepositoryOf[T any, K comparable, O any] interface {
	// Create inserts e and returns its key: the one the database
	// generated, or the natural key e carries.
	Create(ctx context.Context, e *T) (K, error)
	// Read fetches the rows opt selects.
	Read(ctx context.Context, opt O) (ReadResult[T], error)
	// Update writes the row e describes and returns sql.ErrNoRows when
	// no row matched.
	Update(ctx context.Context, e *T) error
	// Delete removes the row whose key is key and returns sql.ErrNoRows
	// when no row matched.
	Delete(ctx context.Context, key K) error
}

// Repository is the RepositoryOf a table keyed by an int the database
// generates, the part of a repository BaseService needs. A repository
// built on a BaseRepo keyed by an int satisfies it.
type Repository[T, O any] = RepositoryOf[T, int, O]

// ServiceOf is the service contract of a table whose key has the Go type
// K. A service with nothing extra declares itself as an alias of it; one
// with extra methods embeds it in its own interface.
type ServiceOf[T any, K comparable, O any] interface {
	// Create inserts a new row and returns it, with its key set when the
	// database generated one.
	Create(ctx context.Context, e *T) (*T, error)
	// Read fetches rows according to opt and returns them with the
	// total: across all pages when paginating, len(items) otherwise.
	Read(ctx context.Context, opt O) ([]T, int, error)
	// ReadByID returns the row whose key is key, or nil if there is
	// none.
	ReadByID(ctx context.Context, key K) (*T, error)
	// Update persists changes to all mutable fields of the row and
	// returns sql.ErrNoRows when no row matched.
	Update(ctx context.Context, e *T) error
	// Delete removes the row whose key is key and returns sql.ErrNoRows
	// when no row matched.
	Delete(ctx context.Context, key K) error
}

// Service is the ServiceOf a table keyed by an int the database
// generates, which BaseService implements.
type Service[T, O any] = ServiceOf[T, int, O]

// BaseServiceOf implements ServiceOf over a RepositoryOf. A service with
// extra behaviour embeds *BaseServiceOf and adds methods, or declares one
// of the same name, which shadows the promoted one; the fields are
// exported for those methods to use.
//
// T is the model, K the Go type of its key and O the repository's read
// option.
type BaseServiceOf[T any, K comparable, O any] struct {
	// Entity names the table's rows in the log, under the "entity" key.
	Entity string
	// Log receives one Info line per row written.
	Log zerolog.Logger
	// Repo is the persistence layer for the table.
	Repo RepositoryOf[T, K, O]
	// byKey builds the read option of a single-row lookup.
	byKey func(key K) O
	// setKey sets the key Create returned on the row it inserted, and is
	// nil when the row carried its key already.
	setKey func(e *T, key K)
}

// BaseService implements Service over a Repository: it is the
// BaseServiceOf of a table keyed by an int the database generates, whose
// Create sets that key on the row it inserted. A service with extra
// behaviour embeds *BaseService and adds methods, or declares one of the
// same name, which shadows the promoted one; Entity, Log and Repo are
// promoted for those methods to use.
//
// T is the model, PT its pointer type, which takes the generated id, and
// O the repository's read option.
type BaseService[T any, PT Keyed[T], O any] struct {
	*BaseServiceOf[T, int, O]
}

// NewBaseServiceOf returns a BaseServiceOf over repo whose rows the log
// calls entity, for a table whose rows carry their key before they are
// inserted, such as a natural key. byKey builds the read option of a
// lookup by key, which ReadByID runs. Only T needs to be given
// explicitly; K and O are inferred from byKey.
//
// A key the database generates goes through NewBaseService instead,
// which sets the key Create returns on the row.
func NewBaseServiceOf[T any, K comparable, O any](
	entity string,
	log zerolog.Logger,
	repo RepositoryOf[T, K, O],
	byKey func(key K) O,
) *BaseServiceOf[T, K, O] {
	return &BaseServiceOf[T, K, O]{
		Entity: entity,
		Log:    log,
		Repo:   repo,
		byKey:  byKey,
	}
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
	s := NewBaseServiceOf(entity, log, repo, byID)
	s.setKey = func(e *T, id int) { PT(e).SetID(id) }
	return &BaseService[T, PT, O]{s}
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

// logged writes msg at info level for one write to the table: the rows'
// entity under the key logfield gives it, then key, the key of the row
// written, under "id" when the write has one to name. Create, Update and
// Delete all log through it, so the three lines differ only in msg and
// in whether they name the row.
func (s *BaseServiceOf[T, K, O]) logged(msg string, key *K) {
	e := logfield.Entity(s.Log.Info(), s.Entity)
	if key != nil {
		e = e.Interface("id", *key)
	}
	e.Msg(msg)
}

// Create inserts a new row and returns it, with the key the database
// generated set on it when there is one.
func (s *BaseServiceOf[T, K, O]) Create(
	ctx context.Context,
	e *T,
) (*T, error) {
	key, err := s.Repo.Create(ctx, e)
	if err != nil {
		return nil, err
	}
	if s.setKey != nil {
		s.setKey(e, key)
	}
	s.logged("created", &key)
	return e, nil
}

// Read fetches rows according to opt.
func (s *BaseServiceOf[T, K, O]) Read(
	ctx context.Context,
	opt O,
) ([]T, int, error) {
	return Unpack(s.Repo.Read(ctx, opt))
}

// ReadByID returns the row whose key is key, or nil if there is none.
func (s *BaseServiceOf[T, K, O]) ReadByID(
	ctx context.Context,
	key K,
) (*T, error) {
	return FirstOf(s.Repo.Read(ctx, s.byKey(key)))
}

// Update persists changes to all mutable fields of the row.
func (s *BaseServiceOf[T, K, O]) Update(ctx context.Context, e *T) error {
	if err := s.Repo.Update(ctx, e); err != nil {
		return err
	}
	s.logged("updated", nil)
	return nil
}

// Delete removes the row whose key is key.
func (s *BaseServiceOf[T, K, O]) Delete(ctx context.Context, key K) error {
	if err := s.Repo.Delete(ctx, key); err != nil {
		return err
	}
	s.logged("deleted", &key)
	return nil
}
