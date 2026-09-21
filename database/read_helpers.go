package database

// The portable half of clause assembly: the parts spelled the same on both
// engines, plus the allowlists that decide which identifiers may reach the
// query text. The grammar the two engines disagree about is in dialect.go,
// and table naming is in namespace.go.
//
// Nothing here reads the selected dialect, so none of it needs SetDialect
// and none of it has a method form on Dialect. That is the dividing line
// between this file and dialect.go rather than a coincidence: a clause that
// turns out to need the engine belongs there.
//
// The package documentation in doc.go carries the Read contract these
// helpers are built for, the ordering rules between them, and the full list
// of places an identifier can reach the SQL text.

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"

	"github.com/jmoiron/sqlx"
)

// SortField is one column and direction for an ORDER BY clause, in the shape
// a query-string parser produces from something like "t.created_at:desc".
// This module's request.Sort is one such parser; nothing here depends on it,
// and a service with its own sort syntax fills the struct directly.
//
// No parser is expected to validate column names, so Col carries the
// client's text unchanged. Safety comes from SortClause, which emits a
// column only when it appears in the repository's allowlist, so unvalidated
// input never reaches the query.
type SortField struct {
	// Col is the column expression as received from the client, for example
	// "t.created_at".
	Col string

	// Dir is the sort direction. SortClause normalises it: anything that is
	// not "desc", in any letter case, sorts ascending.
	Dir string
}

// activeCols resolves which column names a request selects: the known names
// in cols, or defaults when cols is empty or names nothing known.
//
// It is the one definition of "the requested columns", so SelectClause and
// needsJoin cannot disagree. They would: a request naming only unknown
// columns falls back to defaults, and a JOIN decision taken from cols alone
// would drop the JOIN that those defaults reference.
func activeCols(cols, defaults []string, allowed map[string]string) []string {
	known := make([]string, 0, len(cols))
	for _, name := range cols {
		if _, ok := allowed[name]; !ok || slices.Contains(known, name) {
			continue
		}
		known = append(known, name)
	}
	if len(known) == 0 {
		return defaults
	}
	return known
}

// NeedsJoin reports whether the requested columns include any that a JOIN
// supplies, so a builder can leave that JOIN out when nothing reads it.
// joinCols lists the names which depend on it.
//
// The requested columns are resolved exactly as SelectClause resolves them,
// which is what keeps the JOIN and the select list in step.
//
// One combination stays the caller's to avoid: a column that only a JOIN
// supplies may also appear in the ORDER BY or GROUP BY allowlist, and a
// request narrow enough to drop that JOIN while still sorting or grouping on
// its column emits SQL that names an absent alias.
func NeedsJoin(
	cols []string,
	allowed map[string]string,
	defaults []string,
	joinCols []string,
) bool {
	for _, name := range activeCols(cols, defaults, allowed) {
		if slices.Contains(joinCols, name) {
			return true
		}
	}
	return false
}

// ExistsFlag renders a select-list column that reports as 1 or 0 whether
// subquery matches any row, under the name alias:
//
//	CASE WHEN EXISTS (
//	  SELECT 1 FROM ... WHERE ...
//	) THEN 1 ELSE 0 END AS has_children
//
// subquery must be a complete SELECT and alias an identifier, both written by
// the calling repository, so neither carries client input.
//
// Two reasons for a helper rather than "COUNT(1) > 0" inline:
//
//   - Portability. A bare comparison is a legal select-list expression on
//     MySQL, which renders it as 1 or 0, but T-SQL has no boolean value
//     outside a predicate and rejects it.
//   - Cost. EXISTS stops at the first matching row, while COUNT reads every
//     match before comparing the total to zero. These columns are correlated
//     subqueries, evaluated once per output row, so the difference is per
//     row.
//
// The 1 or 0 scans into a bool field: database/sql converts an integer 0 or 1
// to bool.
func ExistsFlag(subquery, alias string) string {
	return "CASE WHEN EXISTS (" + subquery + ") THEN 1 ELSE 0 END AS " + alias
}

// AsText renders col as a character string, for a LIKE filter that searches
// a numeric column beside text ones.
//
// Without it the engines disagree: MySQL converts the column to a string,
// while SQL Server's type precedence converts the pattern to a number
// instead, so "%123%" fails the whole statement with a conversion error
// rather than matching nothing. CAST(col AS CHAR(20)) is spelled the same on
// both.
//
// Twenty characters hold any 64-bit integer, sign included. A value that does
// not fit is truncated on MySQL and, on SQL Server, rendered as "*" for an
// integer or rejected as a conversion error for a decimal or float, so cast
// wider columns with an expression of their own.
//
// The cast defeats an index on col, as does the leading "%" of the pattern it
// is written for.
func AsText(col string) string {
	return "CAST(" + col + " AS CHAR(20))"
}

// GetOne runs a named single-row query and scans the row into dest. A
// missing row is reported as (false, nil) rather than as sql.ErrNoRows, so
// callers test the bool instead of the error.
//
// args binds by name, as in sqlx.Named. ext both binds and executes: it is
// the repository's transaction when one is set and its *sqlx.DB otherwise,
// and either knows its own placeholder style, so nothing else needs passing.
//
// A single-row query usually carries a row cap and no ORDER BY, which leaves
// the row undefined when the predicate matches more than one. Filter on a
// key that can match at most once.
func GetOne(
	ctx context.Context,
	ext sqlx.ExtContext,
	query string,
	args map[string]any,
	dest any,
) (found bool, err error) {
	bound, boundArgs, err := ext.BindNamed(query, args)
	if err != nil {
		return false, err
	}
	err = sqlx.GetContext(ctx, ext, dest, bound, boundArgs...)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// CountQuery reports how many rows query matches, by wrapping it in a
// COUNT(*) derived table. A paginated Read calls it before fetching the page
// so the response can carry a total. The alias sub is arbitrary but
// mandatory: both engines reject an unaliased derived table.
//
// Four preconditions on query, all met by a builder that returns a bare
// SELECT ... FROM ... WHERE:
//
//   - No ORDER BY and no pagination tail. SQL Server rejects an ORDER BY in
//     a derived table unless TOP, OFFSET or FOR XML comes with it, while
//     MySQL accepts and ignores one, so SQL Server is the engine that
//     catches the mistake. Append SortClause and PageClause afterwards.
//   - No row cap. COUNT(*) over a query capped at one row returns 1, so the
//     total would silently cap instead of failing. Cap only the branches
//     that return without counting.
//   - No leading WITH. SQL Server rejects a CTE inside a derived table,
//     while MySQL allows one, so a builder that opens with RecursiveCTE
//     counts with a statement of its own.
//   - Named, distinct columns in the select list. A repeated name is an
//     error on both engines, and an unnamed expression is one on SQL Server,
//     even though nothing here reads the columns.
//
// args is not modified here, and keys the text never mentions do no harm:
// sqlx binds only the names that appear in the query. The order of this call
// and PageClause therefore matters for the query text, not the arguments.
func CountQuery(
	ctx context.Context,
	ext sqlx.ExtContext,
	query string,
	args map[string]any,
) (int, error) {
	bound, boundArgs, err := ext.BindNamed(`
SELECT COUNT(*)
FROM (`+query+`) sub`,
		args,
	)
	if err != nil {
		return 0, err
	}
	var total int
	err = sqlx.GetContext(ctx, ext, &total, bound, boundArgs...)
	if err != nil {
		return 0, err
	}
	return total, nil
}

// SelectList runs a named query and scans every row into dest, which must be
// a pointer to a slice.
//
// An empty result is not an error: sqlx appends nothing and returns nil. It
// also leaves dest untouched, so a nil slice stays nil. Whatever has to
// serialise as an empty list rather than as null must normalise that itself,
// usually the response helper that builds the paginated body.
func SelectList(
	ctx context.Context,
	ext sqlx.ExtContext,
	query string,
	args map[string]any,
	dest any,
) error {
	bound, boundArgs, err := ext.BindNamed(query, args)
	if err != nil {
		return err
	}
	return sqlx.SelectContext(ctx, ext, dest, bound, boundArgs...)
}

// SelectClause builds the comma-separated column list of a SELECT.
//
// allowed maps the names a client may ask for onto the SQL each one emits,
// which lets a request use the names of the JSON contract without knowing
// table aliases or physical column names:
//
//	SelectClause([]string{"unit_name"}, allowed, defaults, "id")
//	// "t.id,\n       u.name AS unit_name"
//
// cols holds the requested names, empty for all of defaults; see activeCols
// for how unknown names are handled. The expression registered under pk is
// prepended when it is missing, so a scan can always find the key. Repeated
// names, and two names that map to one expression, emit that column once,
// because a derived table rejects a duplicate column name and CountQuery
// builds one.
//
// The separator indents continuation lines under the "SELECT " that the
// caller writes in front of the result.
//
// Every expression comes from allowed and none from cols, so nothing a
// client sends reaches the query text. SelectClause panics when cols,
// defaults and pk between them resolve to no column at all, which takes a
// disagreement between three arguments that are all written in code.
func SelectClause(
	cols []string,
	allowed map[string]string,
	defaults []string,
	pk string,
) string {
	var exprs []string
	for _, name := range activeCols(cols, defaults, allowed) {
		expr, ok := allowed[name]
		if !ok || slices.Contains(exprs, expr) {
			continue
		}
		exprs = append(exprs, expr)
	}
	if pkExpr, ok := allowed[pk]; ok && !slices.Contains(exprs, pkExpr) {
		exprs = append([]string{pkExpr}, exprs...)
	}
	if len(exprs) == 0 {
		panic("SelectClause: cols, defaults and pk resolve to no column")
	}
	return strings.Join(exprs, ",\n       ")
}

// SortClause builds an "\nORDER BY col1 DIR1, col2 DIR2" fragment from
// fields, keeping only those whose Col is in allowed and normalising each
// direction to ASC or DESC. When nothing is left it orders by fallback
// instead, so the result is never empty.
//
// That matters beyond tidiness. PageClause emits OFFSET/FETCH on SQL Server,
// which is a syntactic extension of ORDER BY and cannot appear without one,
// and on MySQL a page without a total order can overlap or skip rows between
// two identical requests.
//
// A column repeated in fields is emitted once, with the first direction
// given for it. SQL Server requires the columns of an ORDER BY to be unique
// and MySQL does not, so a client naming one column twice would otherwise
// work on one engine only.
//
// fallback is appended after "ORDER BY " with no validation, so it must be a
// column or comma-separated list written in code, never client input.
func SortClause(fields []SortField, allowed []string, fallback string) string {
	var sb strings.Builder
	var written []string
	for _, f := range fields {
		if !slices.Contains(allowed, f.Col) ||
			slices.Contains(written, f.Col) {
			continue
		}
		if len(written) > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString(f.Col)
		if strings.EqualFold(f.Dir, "desc") {
			sb.WriteString(" DESC")
		} else {
			sb.WriteString(" ASC")
		}
		written = append(written, f.Col)
	}

	if len(written) == 0 {
		return "\nORDER BY " + fallback
	}
	return "\nORDER BY " + sb.String()
}

// GroupClause returns an "\nGROUP BY <col>" fragment, or an empty string when
// col is empty or not in allowed. col must be spelled as the allowlist spells
// it, qualified the same way, since the check is membership and nothing else.
//
// An unknown col yields no grouping rather than an error, as an unknown sort
// column does. A request that names one therefore reads ungrouped rows.
//
// Two things the caller owns: col must also appear in the select list, and
// every other selected column must be grouped or aggregated. Neither is
// visible from here, and both engines reject a select list that breaks the
// second — MySQL under ONLY_FULL_GROUP_BY, SQL Server always.
//
//	GroupClause("t.group_id", []string{"t.group_id", "t.type_id"})
//	// "\nGROUP BY t.group_id"
func GroupClause(col string, allowed []string) string {
	if col != "" && slices.Contains(allowed, col) {
		return "\nGROUP BY " + col
	}
	return ""
}
