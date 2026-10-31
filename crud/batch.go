package crud

import (
	"context"

	"github.com/jmoiron/sqlx"
	"github.com/mas-ony/go-toolkit/database"
)

// Batch is the part of a repository CreateAll needs: Create, and WithTx
// to put Create on the transaction CreateAll opens. R is the repository's
// own interface, which WithTx returns. A repository built on a BaseRepo
// keyed by an int satisfies it with the promoted methods, as long as its
// interface embeds Scoped[R] and declares Create.
type Batch[T, R any] interface {
	// Create inserts e and returns the key the database generated.
	Create(ctx context.Context, e *T) (int, error)
	// WithTx returns the repository scoped to tx.
	WithTx(tx *sqlx.Tx) R
}

// CreateAll inserts items in one transaction on db, through repo scoped
// to it, writes each generated id back and returns pointers into items.
// One failed insert rolls the whole batch back, and the error it returns
// is that insert's; CreateEach says what a failure leaves on items.
//
// Only db and repo have to be given; T, PT and R are inferred from items
// and repo:
//
//	created, err := crud.CreateAll(ctx, s.db, s.repo, items)
//
// It logs nothing, as BaseService's own writes log one line each: a
// service logs the batch once, with the count, where it decides what the
// batch means.
func CreateAll[T any, PT Keyed[T], R Batch[T, R]](
	ctx context.Context,
	db *sqlx.DB,
	repo R,
	items []T,
) ([]*T, error) {
	var created []*T
	err := database.WithTxContext(ctx, db, nil, func(tx *sqlx.Tx) error {
		scoped := repo.WithTx(tx)
		var err error
		created, err = CreateEach[T, PT](items, func(e *T) (int, error) {
			return scoped.Create(ctx, e)
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	return created, nil
}
