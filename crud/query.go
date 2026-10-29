package crud

import (
	"fmt"
	"strings"

	"github.com/mas-ony/go-toolkit/database"
)

// Query accumulates a SELECT with its WHERE predicates and the
// bound-parameter map, for Fetch to run.
//
// The query uses :name placeholders that sqlx.Named expands to positional
// ? markers, which the driver's bind type then rewrites to its own
// format. Table names are part of the identifier, not placeholders, so
// they are concatenated rather than bound.
//
// The WHERE clause starts with "(1=1)" so every predicate is appended
// uniformly as "AND <expr>"; the optimiser folds the tautology away.
type Query struct {
	// prefix opens the statement ahead of the SELECT; see Prepend.
	prefix string
	// sql is the statement text built so far.
	sql string
	// args holds the values bound to its :name placeholders.
	args map[string]any
	// single marks a lookup that returns at most one row.
	single bool
}

// ListOption is the part of a read option every repository shares: which
// columns to select, how to sort, and how to paginate.
type ListOption struct {
	// Cols restricts the SELECT list to the named columns. Empty means
	// the repository's default columns. Unknown names are dropped.
	Cols []string

	// Sort holds ORDER BY fields. Columns outside the repository's sort
	// allowlist are discarded, and an empty result falls back to its
	// default sort.
	Sort []database.SortField

	// Pagination, ignored when Page < 1 or Limit < 1.
	Page  int
	Limit int
}

// ReadOption is the read option of a repository whose key has the Go
// type K. Each repository's own option embeds it and adds its
// table-specific filters, which apply only when neither ID nor Search is
// set. An alias that fixes K keeps the embedded field's name short:
//
//	type ReadOption = crud.ReadOption[int]
//
//	type UnitReadOption struct {
//		ReadOption
//		JenisID *int
//	}
type ReadOption[K comparable] struct {
	ListOption

	// ID is a single-row lookup on the key. When set, every other filter
	// is ignored.
	ID *K

	// Search matches any of the repository's search columns (LIKE
	// %value%). When set, the table-specific filters are ignored. A
	// repository with no search columns ignores it.
	Search *string
}

// ReadResult is what every repository's Read returns. Total counts all
// matching rows across pages for a paginated read, len(Items) for a plain
// list, and 0 or 1 for a single-row lookup.
type ReadResult[T any] struct {
	// Items holds the rows read; for a paginated read, one page of them.
	Items []T
	// Total reflects all matching rows across pages for paginated calls.
	Total int
}

// NewSelect starts "SELECT cols FROM from WHERE (1=1)". from is the
// qualified table, its alias, and any JOINs. Begin and Select call it for
// the repository's own table; a repository calls it directly for a
// statement whose FROM it builds itself.
func NewSelect(cols, from string) *Query {
	return &Query{
		sql: "SELECT " + cols +
			"\nFROM " + from +
			"\nWHERE (1=1)",
		args: map[string]any{},
	}
}

// AndCmp appends "col <op> :param" when v is non-nil, for range bounds
// such as ">=" and "<=". op is concatenated like col, so it must be
// written in code.
func AndCmp[V any](q *Query, col, op, param string, v *V) {
	if v == nil {
		return
	}
	q.And(col+" "+op+" :"+param).Bind(param, *v)
}

// AndEq appends "col = :param" when v is non-nil. It is AndCmp with "=".
func AndEq[V any](q *Query, col, param string, v *V) {
	AndCmp(q, col, "=", param, v)
}

// AndAnyLike appends a substring match of one value against any of cols,
// ORed together, when v is non-nil. Each match goes through
// database.LikePredicate and v through database.LikeArg, so the LIKE
// metacharacters in v match themselves.
func AndAnyLike(q *Query, param string, v *string, cols ...string) {
	if v == nil || len(cols) == 0 {
		return
	}
	preds := make([]string, len(cols))
	for i, c := range cols {
		preds[i] = database.LikePredicate(c, param)
	}
	q.And(strings.Join(preds, "\n    OR ")).
		Bind(param, database.LikeArg(*v))
}

// AndLike appends a substring match on col when v is non-nil. It is
// AndAnyLike with one column, so the LIKE metacharacters in v match
// themselves in the same way.
func AndLike(q *Query, col, param string, v *string) {
	AndAnyLike(q, param, v, col)
}

// AndIn appends "col IN (...)" when vs is non-empty. sqlx.Named does not
// expand slices, so each element is bound as param_0, param_1, ... An
// empty slice is not a filter.
//
// Keep vs bounded: each element is one bind parameter, and SQL Server
// caps a statement at about 2100 of them.
func AndIn[V any](q *Query, col, param string, vs []V) {
	if len(vs) == 0 {
		return
	}
	ph := make([]string, len(vs))
	for i, v := range vs {
		key := fmt.Sprintf("%s_%d", param, i)
		ph[i] = ":" + key
		q.args[key] = v
	}
	q.And(col + " IN (" + strings.Join(ph, ", ") + ")")
}

// Prepend puts text ahead of the SELECT, for a clause that has to open
// the statement, such as the WITH of a recursive CTE. A second call goes
// in front of the first.
//
// Fetch caps a prefixed lookup on the SELECT after the prefix, and
// refuses to paginate a prefixed query: its COUNT wraps the statement in
// a derived table, and SQL Server accepts no WITH there.
func (q *Query) Prepend(text string) *Query {
	q.prefix = text + "\n" + q.prefix
	return q
}

// Bind sets a named parameter.
func (q *Query) Bind(name string, v any) *Query {
	q.args[name] = v
	return q
}

// One marks the query as a single-row lookup: Fetch caps it at one row
// and returns at most one item.
func (q *Query) One() *Query {
	q.single = true
	return q
}

// And appends a raw predicate, whose parameters Bind sets. expr is
// concatenated into the statement, so it must be written in code.
func (q *Query) And(expr string) *Query {
	q.sql += "\n  AND (" + expr + ")"
	return q
}
