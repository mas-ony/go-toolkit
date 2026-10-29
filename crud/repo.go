package crud

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/jmoiron/sqlx"
	"github.com/mas-ony/go-toolkit/database"
)

// BaseRepo holds everything the repositories of a service have in common:
// the connection and its scoping (transaction, namespace, fiscal year)
// and the statements that follow mechanically from a TableSpec.
//
// T is the model the table maps to, K the Go type of its key and R the
// repository's own interface. A repository embeds BaseRepo by value, so
// WithTx, WithNamespace, WithYear, Create, Update, Patch and Delete are
// promoted onto it and satisfy its interface as long as the signatures
// match. The statements cover a single-column key, generated or natural;
// see TableSpec.Key. A repository whose statements a TableSpec cannot
// describe, such as a composite key or a column set to
// COALESCE(:x, NOW()), declares its own method of the same name, which
// shadows the promoted one.
//
// The query builders are promoted as well, for the repository's own
// Read: Begin or Select starts the statement, the And helpers filter it,
// and Fetch runs it.
//
// BaseRepo logs nothing; see the package documentation for where its
// errors go instead.
type BaseRepo[T any, K comparable, R any] struct {
	// db is the connection pool. It runs the queries when tx is nil, and
	// its driver decides the placeholder style either way.
	db *sqlx.DB
	// tx is nil when not in a transaction.
	tx *sqlx.Tx
	// namespace is the base prefix table names are qualified with.
	namespace string
	// year is the fiscal year ns applies to it.
	year int

	// spec describes the table and its key; Spec returns it.
	spec *TableSpec[T, K]
	// embed turns a scoped copy of the base back into the repository, so
	// the With* methods can return R.
	embed func(BaseRepo[T, K, R]) R
}

// Scoped is the set of scoping methods every repository interface embeds.
// R is the repository's own interface, so each method hands back a value
// the caller can keep using without a type assertion.
type Scoped[R any] interface {
	// WithTx returns a repository scoped to tx for multi-table
	// transactions.
	WithTx(tx *sqlx.Tx) R
	// WithNamespace returns a repository whose queries are qualified with
	// namespace instead of the configured default.
	WithNamespace(namespace string) R
	// WithYear returns a repository whose queries target the database for
	// the given fiscal year. A year of zero or less leaves the namespace
	// as it is.
	WithYear(year int) R
}

// errNilModel is the cause Create and Update report for a nil model.
var errNilModel = errors.New("nil model")

// RequireOneRow converts "statement succeeded but matched nothing" into
// sql.ErrNoRows, so the service layer can map a missing key to a 404
// without a second round trip to check existence first. It returns
// sql.ErrNoRows as database/sql spells it, unwrapped.
//
// It reads RowsAffected, which MySQL reports as the rows a statement
// CHANGED unless the connection sets ClientFoundRows. database.New sets
// it, so an UPDATE that writes the values a row already holds still
// counts the row it matched, as SQL Server always does. A pool opened any
// other way must set it too, or re-saving an unedited row answers
// sql.ErrNoRows on MySQL alone.
//
// A driver that cannot report the count is not read as a miss, since the
// statement itself succeeded: RequireOneRow returns nil. Both drivers the
// database package opens report it.
func RequireOneRow(res sql.Result) error {
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// NewBaseRepo returns a BaseRepo over the table spec describes, which
// runs its statements on db and qualifies its tables with
// database.DefaultNamespace as it stands at the call, so call it after
// database.Configure.
//
// embed puts a BaseRepo into the repository that embeds it, and is how
// WithTx, WithNamespace and WithYear hand back R; the package
// documentation shows one. NewBaseRepo panics when spec describes its key
// inconsistently: KeyOf must be set exactly when Writable lists the key,
// a generated key needs K int, and Patchable must leave the key out.
func NewBaseRepo[T any, K comparable, R any](
	db *sqlx.DB,
	spec *TableSpec[T, K],
	embed func(BaseRepo[T, K, R]) R,
) BaseRepo[T, K, R] {
	spec.check()
	return BaseRepo[T, K, R]{
		db:        db,
		namespace: database.DefaultNamespace,
		spec:      spec,
		embed:     embed,
	}
}

// ns returns the namespace tables are qualified with: the base namespace,
// re-pointed at the fiscal year when one is set.
func (b BaseRepo[T, K, R]) ns() string {
	return database.NamespaceForYear(b.namespace, b.year)
}

// joinAs renders one join clause of the given kind.
func (b BaseRepo[T, K, R]) joinAs(kind, table, alias, on string) string {
	return "\n" + kind + " " + b.Qualify(table) + " " + alias + " ON " + on
}

// updateQuery is the UPDATE that Update and Patch send: each of cols set
// from the named parameter of its own name, on the row whose key equals
// where, which is a named parameter or a ? the caller binds.
func (b BaseRepo[T, K, R]) updateQuery(cols []string, where string) string {
	sets := make([]string, len(cols))
	for i, c := range cols {
		sets[i] = column(c) + " = :" + c
	}
	return "UPDATE " + b.Table() +
		"\nSET " + strings.Join(sets, ",\n    ") +
		"\nWHERE " + column(b.spec.Key) + " = " + where
}

// WithTx returns a copy of the repository that executes on tx instead of
// the connection pool. Namespace and year are carried over, so the With*
// methods chain in any order.
func (b BaseRepo[T, K, R]) WithTx(tx *sqlx.Tx) R {
	b.tx = tx
	return b.embed(b)
}

// WithNamespace returns a copy of the repository whose queries are
// qualified with namespace instead of the default.
func (b BaseRepo[T, K, R]) WithNamespace(namespace string) R {
	b.namespace = namespace
	return b.embed(b)
}

// WithYear returns a copy of the repository whose queries target the
// database for the given fiscal year, in place of the year its namespace
// carries. A year of zero or less leaves the namespace as it is.
func (b BaseRepo[T, K, R]) WithYear(year int) R {
	b.year = year
	return b.embed(b)
}

// Wrap returns err with the table and the method it failed in, as
// "<table>.<method>: <err>", or nil when err is nil, so a repository
// method can end in "return r.Wrap(...)". errors.Is and errors.As see
// through it to the cause.
//
// Every error BaseRepo returns has this form, sql.ErrNoRows from a write
// aside; a method a repository writes itself calls Wrap to match.
func (b BaseRepo[T, K, R]) Wrap(method string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s.%s: %w", b.spec.Name, method, err)
}

// Ext returns the transaction when one is set, the pool otherwise.
// Every statement BaseRepo runs goes through it, and a repository's own
// statements should too, which is what lets a repository take part in a
// transaction without knowing that it is in one.
func (b BaseRepo[T, K, R]) Ext() sqlx.ExtContext {
	if b.tx != nil {
		return b.tx
	}
	return b.db
}

// Qualify returns name qualified with the active namespace. Use it for
// tables other than the repository's own, e.g. in a JOIN.
func (b BaseRepo[T, K, R]) Qualify(name string) string {
	return database.Qualify(b.ns(), name)
}

// Spec returns the description of the repository's own table, for the
// statements a repository writes itself.
func (b BaseRepo[T, K, R]) Spec() *TableSpec[T, K] {
	return b.spec
}

// Table returns the repository's own table, qualified.
func (b BaseRepo[T, K, R]) Table() string {
	return b.Qualify(b.spec.Name)
}

// Rebind rewrites ? placeholders into the driver's format.
func (b BaseRepo[T, K, R]) Rebind(query string) string {
	return b.db.Rebind(query)
}

// Select starts a SELECT over the repository's own table with the
// caller's column subset resolved through spec.Cols.
func (b BaseRepo[T, K, R]) Select(cols []string) *Query {
	return b.SelectJoined(cols, "")
}

// SelectJoined is Select with joins appended after the FROM table. Build
// joins with LeftJoin, emitting each only when Needs or SortsOn says
// something references its alias, and with InnerJoin for the ones that
// must always apply.
func (b BaseRepo[T, K, R]) SelectJoined(
	cols []string,
	joins string,
) *Query {
	s := b.spec
	list := database.SelectClause(cols, s.Cols, s.DefaultCols, s.Key)
	return NewSelect(list, b.Table()+" AS "+s.Alias+joins)
}

// Needs reports whether the resolved SELECT list includes any of names,
// so a JOIN that only feeds those columns can be left out otherwise.
func (b BaseRepo[T, K, R]) Needs(cols []string, names ...string) bool {
	s := b.spec
	return database.NeedsJoin(cols, s.Cols, s.DefaultCols, names)
}

// BeginJoined is Begin with joins appended after the FROM table.
func (b BaseRepo[T, K, R]) BeginJoined(
	opt ReadOption[K],
	joins string,
) (*Query, bool) {
	s := b.spec
	q := b.SelectJoined(opt.Cols, joins)

	switch {
	case opt.ID != nil:
		AndEq(q, s.Alias+"."+column(s.Key), s.Key, opt.ID)
		q.One()
		return q, false
	case opt.Search != nil && len(s.SearchCols) > 0:
		AndAnyLike(q, "search", opt.Search, s.SearchCols...)
		return q, false
	}
	return q, true
}

// Begin starts the SELECT for opt and applies the filters ReadOption
// carries: ID as a single-row lookup on the key, otherwise Search across
// spec.SearchCols. It reports whether the caller should go on to apply
// its table-specific filters, which is only when neither took effect.
func (b BaseRepo[T, K, R]) Begin(opt ReadOption[K]) (*Query, bool) {
	return b.BeginJoined(opt, "")
}

// LeftJoin returns "LEFT JOIN table alias ON on" with table qualified by
// the active namespace, or "" when want is false. A LEFT JOIN never
// removes rows from the result, so it is safe to leave out whenever
// nothing reads its alias.
func (b BaseRepo[T, K, R]) LeftJoin(
	want bool,
	table, alias, on string,
) string {
	if !want {
		return ""
	}
	return b.joinAs("LEFT JOIN", table, alias, on)
}

// InnerJoin returns "JOIN table alias ON on" with table qualified by the
// active namespace. It takes no want flag because an inner join drops
// rows with no match: leaving it out would change which rows come back
// and what Total counts, not just which columns are filled. Emit it on
// every read path, the COUNT included; if it is only wanted for some
// reads, decide that explicitly in the repository.
func (b BaseRepo[T, K, R]) InnerJoin(table, alias, on string) string {
	return b.joinAs("JOIN", table, alias, on)
}

// SortsOn reports whether sort names an allowlisted column under alias.
// SortClause emits such a column whatever the SELECT list asked for, so
// a JOIN must be kept for it or the alias is missing from the query.
func (b BaseRepo[T, K, R]) SortsOn(
	sort []database.SortField,
	alias string,
) bool {
	for _, f := range sort {
		if slices.Contains(b.spec.SortCols, f.Col) &&
			strings.HasPrefix(f.Col, alias+".") {
			return true
		}
	}
	return false
}

// Fetch runs a built query in one of three modes:
//
//  1. Single-row lookup (q.One was called): returns at most one item. A
//     missing row is an empty result, not an error; callers check
//     len(Items).
//  2. Paginated list (Page and Limit positive): COUNT, then fetch the
//     page. ORDER BY is appended after the count, since it is invalid
//     inside the COUNT wrapper.
//  3. Plain list: every matching row.
//
// A query with a prefix (see Prepend) runs in mode 1 or 3 only. Asked
// for a page of one, Fetch returns an error before sending anything.
func (b BaseRepo[T, K, R]) Fetch(
	ctx context.Context,
	q *Query,
	opt ListOption,
) (ReadResult[T], error) {
	query, args := q.sql, q.args

	if q.single {
		var item T
		capped := q.prefix + database.ApplyLimit(query, 1)
		found, err := database.GetOne(ctx, b.Ext(), capped, args, &item)
		if err != nil {
			return ReadResult[T]{}, b.Wrap("Read", err)
		}
		if !found {
			return ReadResult[T]{}, nil
		}
		return ReadResult[T]{Items: []T{item}, Total: 1}, nil
	}

	order := database.SortClause(
		opt.Sort, b.spec.SortCols, b.spec.DefaultSort)

	if opt.Page > 0 && opt.Limit > 0 {
		if q.prefix != "" {
			return ReadResult[T]{}, b.Wrap("Read",
				errors.New("cannot paginate a prefixed query"))
		}
		total, err := database.CountQuery(ctx, b.Ext(), query, args)
		if err != nil {
			return ReadResult[T]{}, b.Wrap("Read", err)
		}

		var items []T
		paged := query + order +
			database.PageClause(args, opt.Page, opt.Limit)
		err = database.SelectList(ctx, b.Ext(), paged, args, &items)
		if err != nil {
			return ReadResult[T]{}, b.Wrap("Read", err)
		}
		return ReadResult[T]{Items: items, Total: total}, nil
	}

	var items []T
	err := database.SelectList(ctx, b.Ext(), q.prefix+query+order, args,
		&items)
	if err != nil {
		return ReadResult[T]{}, b.Wrap("Read", err)
	}
	return ReadResult[T]{Items: items, Total: len(items)}, nil
}

// Create inserts m and returns the new row's key. The columns written are
// spec.Writable, bound as :name placeholders from the model's db tags.
//
// A generated key comes back through InsertReturningID. A natural key is
// read with KeyOf before anything is sent, so a model that carries none
// fails without a round trip, and the row is written with database.Insert,
// a plain INSERT, because there is nothing to fetch back.
func (b BaseRepo[T, K, R]) Create(ctx context.Context, m *T) (K, error) {
	var zero K
	if m == nil {
		return zero, b.Wrap("Create", errNilModel)
	}

	w := b.spec.Writable
	cols := "\n" + strings.Join(columns(w), ",\n")
	vals := "\n:" + strings.Join(w, ",\n:")

	if b.spec.KeyOf == nil {
		id, err := database.InsertReturningID(
			ctx, b.Ext(), b.Table(), cols, vals, column(b.spec.Key), m)
		if err != nil {
			return zero, b.Wrap("Create", err)
		}
		return any(id).(K), nil
	}

	key, ok := b.spec.KeyOf(m)
	if !ok {
		return zero, b.Wrap("Create",
			errors.New("model has no "+b.spec.Key))
	}

	_, err := database.Insert(ctx, b.Ext(), b.Table(), cols, vals, m)
	if err != nil {
		return zero, b.Wrap("Create", err)
	}
	return key, nil
}

// Update writes every column in spec.Updatable (or spec.Writable when
// that is nil) for the row whose key matches m's, and returns
// sql.ErrNoRows when no row matched.
//
// A generated key is bound from the model like the columns. A natural key
// is bound as KeyOf returns it, through a ? that sqlx.Named passes over
// and Rebind numbers after the named placeholders. A model KeyOf finds no
// key in matches no row.
func (b BaseRepo[T, K, R]) Update(ctx context.Context, m *T) error {
	if m == nil {
		return b.Wrap("Update", errNilModel)
	}

	w := b.spec.Updatable
	if w == nil {
		w = b.spec.Writable
	}

	where := ":" + b.spec.Key
	var keyArg []any
	if b.spec.KeyOf != nil {
		key, ok := b.spec.KeyOf(m)
		if !ok {
			return sql.ErrNoRows
		}
		where, keyArg = "?", []any{key}
	}

	bound, args, err := sqlx.Named(b.updateQuery(w, where), m)
	if err != nil {
		return b.Wrap("Update", err)
	}
	args = append(args, keyArg...)

	res, err := b.Ext().ExecContext(ctx, b.Rebind(bound), args...)
	if err != nil {
		return b.Wrap("Update", err)
	}
	return RequireOneRow(res)
}

// Patch writes only the columns named in fields, for the row whose key is
// key, and returns sql.ErrNoRows when no row matched. An empty map writes
// nothing. key takes the form Delete takes.
//
// Each name is checked against spec.Patchable before any SQL is built,
// and one outside it is an error; the values and the key are bound as
// usual. The columns are written in sorted order, so the same fields
// always build the same statement.
func (b BaseRepo[T, K, R]) Patch(
	ctx context.Context,
	key K,
	fields map[string]any,
) error {
	if len(fields) == 0 {
		return nil
	}

	cols := slices.Sorted(maps.Keys(fields))
	args := make(map[string]any, len(cols)+1)
	for _, c := range cols {
		if !slices.Contains(b.spec.Patchable, c) {
			return b.Wrap("Patch",
				fmt.Errorf("column %q is not patchable", c))
		}
		args[c] = fields[c]
	}
	args[b.spec.Key] = key

	query := b.updateQuery(cols, ":"+b.spec.Key)
	res, err := sqlx.NamedExecContext(ctx, b.Ext(), query, args)
	if err != nil {
		return b.Wrap("Patch", err)
	}
	return RequireOneRow(res)
}

// DeleteWhere removes every row where col = val, and reports a failure
// under method. With requireOne set it returns sql.ErrNoRows when nothing
// matched; leave it unset for a bulk delete such as removing a parent's
// children, where none is fine.
//
// col is a column of the repository's own table, written in code and
// quoted like the columns of a TableSpec.
func (b BaseRepo[T, K, R]) DeleteWhere(
	ctx context.Context,
	method string,
	col string,
	val any,
	requireOne bool,
) error {
	query :=
		"DELETE FROM " + b.Table() +
			"\nWHERE " + column(col) + " = ?"

	res, err := b.Ext().ExecContext(ctx, b.Rebind(query), val)
	if err != nil {
		return b.Wrap(method, err)
	}
	if !requireOne {
		return nil
	}
	return RequireOneRow(res)
}

// Delete removes the row whose key is key and returns sql.ErrNoRows when
// nothing matched.
func (b BaseRepo[T, K, R]) Delete(ctx context.Context, key K) error {
	return b.DeleteWhere(ctx, "Delete", b.spec.Key, key, true)
}
