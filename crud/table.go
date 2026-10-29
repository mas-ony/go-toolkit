package crud

import (
	"slices"

	"github.com/mas-ony/go-toolkit/database"
)

// TableSpec describes the table a repository owns. T is the model a row
// maps to and K the Go type of its key.
//
// Every field is written in code, and most reach the query text as
// written, so none may come from a request. NewBaseRepo keeps a pointer
// to the spec rather than a copy, so a spec must not change once a
// repository is built on it.
type TableSpec[T any, K comparable] struct {
	// Name is the unqualified table name. BaseRepo qualifies it with the
	// active namespace through database.Qualify, which quotes it when it
	// would not parse bare.
	Name string
	// Alias is the table alias the SELECT statements use.
	Alias string

	// Key is the primary-key column Create, Update, Patch and Delete
	// address. When Writable leaves it out, the database generates it on
	// INSERT, Create returns it, and K must be int, the type
	// database.InsertReturningID returns. When Writable lists it, the key
	// is natural: the caller supplies it, KeyOf reads it from the model,
	// and Updatable usually leaves it out.
	//
	// Cols lists the key under the same name, so that every SELECT
	// carries it; see database.SelectClause.
	Key string
	// KeyOf reads a natural key from m in the form Update binds and
	// Create returns, reporting false when m carries none. It is set
	// exactly when Writable lists Key.
	KeyOf func(m *T) (K, bool)

	// Cols maps caller-facing column names to the SQL expressions
	// emitted in the SELECT list. A name that is not a key here never
	// reaches SQL.
	Cols map[string]string
	// DefaultCols is the SELECT list used when the caller names none.
	DefaultCols []string

	// Writable is the ordered column list written by Create and Update.
	// It is the single source of truth for both statements, so a column
	// cannot be added to one and forgotten in the other. Each name is
	// also the db tag its value is bound from.
	Writable []string
	// Updatable is the column list Update writes when it differs from
	// Writable, e.g. to leave a write-once foreign key alone. Nil means
	// Writable.
	Updatable []string
	// Patchable is the allowlist of columns Patch may write on their
	// own, without the rest of the row, such as a password that has an
	// endpoint of its own. Column names reach the SET clause as text,
	// not as bound values, so this list is a security boundary: Patch
	// refuses any other name before it builds SQL. It must not list Key.
	// Empty means Patch refuses every column.
	Patchable []string

	// SearchCols are the qualified columns ReadOption.Search matches,
	// ORed together. Empty means the repository has no search.
	SearchCols []string

	// SortCols is the ORDER BY allowlist.
	SortCols []string
	// DefaultSort is the ORDER BY used when the caller names none, or
	// only names columns outside SortCols.
	DefaultSort string
}

// column returns name as a statement writes it: bare when it parses bare
// on both engines, and quoted in the selected grammar otherwise, so a
// column named after a reserved word, user in T-SQL or rank in MySQL,
// needs nothing at the call site. It is database.Qualify without a
// namespace, so a column follows the rule a table name does, and a plain
// name never consults the grammar.
func column(name string) string {
	return database.Qualify("", name)
}

// columns is column applied to each of names.
func columns(names []string) []string {
	out := make([]string, len(names))
	for i, name := range names {
		out[i] = column(name)
	}
	return out
}

// check panics unless the key is described consistently: KeyOf is set
// exactly when Writable lists the key, a generated key is an int, and
// Patchable leaves the key out, since Patch binds the key under its
// column name beside the patched columns. A mismatch is a wiring mistake,
// so it stops the program when the repository is constructed rather than
// failing its first write.
func (s *TableSpec[T, K]) check() {
	natural := slices.Contains(s.Writable, s.Key)
	if natural != (s.KeyOf != nil) {
		panic("crud: " + s.Name + ": KeyOf must be set " +
			"exactly when Writable lists the key " + s.Key)
	}
	if _, isInt := any(*new(K)).(int); !natural && !isInt {
		panic("crud: " + s.Name + ": generated key " + s.Key +
			" needs K int")
	}
	if slices.Contains(s.Patchable, s.Key) {
		panic("crud: " + s.Name + ": Patchable lists the key " + s.Key)
	}
}
