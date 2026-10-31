package crud

import (
	"context"

	"github.com/jmoiron/sqlx"
)

// GetInt runs query, a statement that returns one integer, such as a
// COUNT or a COALESCE(MAX(...), 0), and returns that integer. Its ?
// placeholders bind args in order, rewritten into the driver's format by
// Rebind, and a failure is reported under method, as Wrap reports one.
//
// query is concatenated as written, so it must be written in code, with
// tables named through Table or Qualify. A statement that returns no row
// is a failure, sql.ErrNoRows wrapped: an aggregate always returns one,
// so a query that can return none should not come here.
func (b BaseRepo[T, K, R]) GetInt(
	ctx context.Context,
	method, query string,
	args ...any,
) (int, error) {
	var n int
	err := sqlx.GetContext(ctx, b.Ext(), &n, b.Rebind(query), args...)
	if err != nil {
		return 0, b.Wrap(method, err)
	}
	return n, nil
}

// Count returns the number of rows in the repository's own table.
func (b BaseRepo[T, K, R]) Count(ctx context.Context) (int, error) {
	return b.GetInt(ctx, "Count", "SELECT COUNT(*) FROM "+b.Table())
}

// Max returns the largest value of col in the repository's own table, or
// 0 when the table has no rows, such as the highest number issued so far
// for a form to offer the next one. col is written in code and quoted
// like the columns of a TableSpec.
func (b BaseRepo[T, K, R]) Max(ctx context.Context, col string) (int, error) {
	return b.GetInt(ctx, "Max",
		"SELECT COALESCE(MAX("+column(col)+"), 0) FROM "+b.Table())
}
