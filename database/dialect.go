package database

// SQL grammar that differs between MySQL and SQL Server.
//
// Query builders that must run on both engines call the helpers in this
// file for the clauses below instead of branching on the engine. Anything
// else they write has to be spelled identically on both, and the lists
// after the table cover the common cases. The helpers deal in statement
// syntax only; qualifying a table name with a database or schema prefix is
// left to the caller.
//
//	                   MySQL                    SQL Server
//	-----------------  -----------------------  ---------------------------
//	row cap            SELECT ... LIMIT n       SELECT TOP (n) ...
//	page               LIMIT :limit             OFFSET :offset ROWS
//	                   OFFSET :offset           FETCH NEXT :limit ROWS ONLY
//	server clock       NOW()                    SYSDATETIME()
//	recursive CTE      WITH RECURSIVE x AS (    WITH x AS (
//	quoted identifier  `name`                   [name]
//	generated key      LastInsertId()           OUTPUT INSERTED.pk INTO @t
//	LIKE escape        ESCAPE '!'               ESCAPE '!'
//
// MySQL here includes MariaDB, and SQL Server includes Azure SQL Database.
//
// # Every helper has a package-level form and a method form
//
// The package-level functions read the grammar SetDialect selected, which
// suits a process that talks to one engine and is the shape most callers
// want. Each is a one-line wrapper over the same method on Dialect, and the
// method takes the grammar as its receiver instead:
//
//	QuoteIdent(name)     the selected grammar
//	d.QuoteIdent(name)   the grammar in d
//
// The method form is what makes this package usable where one process
// spells SQL for both engines — a migration or a sync job between two
// systems — and what lets a test cover both grammars without touching
// process-wide state. Nothing else separates the two forms.
//
// One of those methods is NOT in this file. Qualify is declared in
// namespace.go, beside the package-level Qualify it mirrors, because the
// two share needsQuoting and splitting them would leave neither readable on
// its own. It is listed here so that a reader checking the claim above
// against this file does not conclude it is missing.
//
// # Row caps are applied to a finished query
//
// TOP follows SELECT while LIMIT ends the statement, so no fragment that a
// builder concatenates can land in the right place on both engines.
// Builders therefore produce a complete, uncapped SELECT and pass it to
// ApplyLimit, which inserts the cap where the selected dialect needs it.
//
// # Portable without a helper
//
//   - NULL coalescing: COALESCE. ISNULL takes two arguments on SQL Server
//     but is a one-argument null test on MySQL.
//   - Boolean columns: CASE WHEN ... THEN 1 ELSE 0 END. A bare comparison
//     such as COUNT(1) > 0 is not a valid select-list expression in T-SQL.
//   - Concatenation: CONCAT(a, b). On MySQL, + adds numbers, and || means
//     OR unless PIPES_AS_CONCAT is set.
//
// # Not made portable here
//
//   - Identifier case. MySQL table names follow lower_case_table_names and
//     are case-sensitive on Linux by default; SQL Server identifiers follow
//     the database collation. Spell every name in the schema exactly as the
//     query text spells it.
//   - Collation. Whether = and LIKE ignore case and accents depends on the
//     column collation, and the two engines ship different defaults.
//   - Types. DATETIME2(n) is DATETIME(n) on MySQL. A flag is BIT on SQL
//     Server and TINYINT(1) on MySQL; both scan into a bool. REAL is a
//     4-byte float on SQL Server but DOUBLE on MySQL unless REAL_AS_FLOAT
//     is set.
//   - Triggers. A trigger that maintains a column exists only where it was
//     migrated. A statement that sets the column itself, through NowExpr
//     for example, does not depend on it.
//   - GROUP BY. MySQL accepts select-list columns that depend functionally
//     on the grouping key, and any column when ONLY_FULL_GROUP_BY is off.
//     SQL Server requires every non-aggregated column in the GROUP BY.
//   - Errors. A duplicate key is error 1062 on MySQL and 2627 or 2601 on
//     SQL Server. Classify driver errors by number, never by message text.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/jmoiron/sqlx"
)

// selectedDialect holds the Dialect passed to SetDialect, or zero before
// the first call.
var selectedDialect atomic.Int32

// likeEscape is the LIKE escape character. LikeArg writes it and
// LikePredicate declares it, so the two cannot disagree.
//
// ESCAPE takes a string literal, and a backslash cannot be written the same
// way on both engines. Under MySQL's default sql_mode a backslash escapes
// the next character of a literal, so '\' leaves the literal unterminated
// and '\\' is required; SQL Server reads '\\' as two characters, which
// ESCAPE rejects.
// "!" is an ordinary character in literals on both engines and a wildcard
// on neither.
const likeEscape = "!"

// likeEscaper escapes the escape character itself, % and _, which are
// wildcards on both engines, and [, which opens a character class in T-SQL.
// Escaping [ is harmless on MySQL, where an escaped ordinary character
// matches itself, and ], ^ and - are special only inside a class that an
// escaped [ can no longer open. The replacer makes one pass and never
// rescans its own output, so the rules cannot interfere with each other.
var likeEscaper = strings.NewReplacer(
	likeEscape, likeEscape+likeEscape,
	"%", likeEscape+"%",
	"_", likeEscape+"_",
	"[", likeEscape+"[",
)

// Dialect is the statement grammar the helpers in this file emit.
//
// The zero value selects no grammar. No engine is a safe default for code
// shared between deployments, so until SetDialect has run, every helper
// that depends on the grammar panics instead of emitting SQL for an engine
// nobody chose.
type Dialect int

const (
	// DialectMySQL emits MySQL and MariaDB grammar.
	DialectMySQL Dialect = iota + 1

	// DialectSQLServer emits T-SQL for SQL Server and Azure SQL Database.
	DialectSQLServer
)

// ErrNoIdentityValue is returned by InsertReturningID when the INSERT
// succeeded but yielded no generated key.
var ErrNoIdentityValue = errors.New("insert returned no identity value")

// activeDialect returns the selected Dialect. It panics if SetDialect has
// not run, because a missing call is a wiring mistake that must fail loudly
// rather than produce SQL for the wrong engine.
func activeDialect() Dialect {
	d := Dialect(selectedDialect.Load())
	if d == 0 {
		panic("dialect not selected: call SetDialect at startup")
	}
	return d
}

// isMySQL reports whether d spells MySQL grammar, and panics on a value that
// spells no grammar at all.
//
// Every method below branches through it rather than on `d == DialectMySQL`,
// so that a zero or out-of-range Dialect fails loudly instead of falling
// into the T-SQL side of an if/else. That is the same bargain SetDialect and
// activeDialect strike for the package-level forms: an engine nobody chose
// is a wiring mistake, and emitting the wrong grammar for it would surface
// as a syntax error far from the cause.
func (d Dialect) isMySQL() bool {
	switch d {
	case DialectMySQL:
		return true
	case DialectSQLServer:
		return false
	}
	panic("database: " + d.String() + " is not a grammar")
}

// cutKeyword reports whether s, after any leading white space, opens with
// keyword in any letter case followed by white space. If so, it returns
// the text after that white space.
func cutKeyword(s, keyword string) (rest string, ok bool) {
	const space = " \t\r\n"
	t := strings.TrimLeft(s, space)
	if len(t) <= len(keyword) ||
		!strings.EqualFold(t[:len(keyword)], keyword) {
		return s, false
	}
	rest = strings.TrimLeft(t[len(keyword):], space)
	if len(rest) == len(t)-len(keyword) {
		return s, false
	}
	return rest, true
}

// insertMySQL is the MySQL half of InsertReturningID.
func insertMySQL(
	ctx context.Context,
	ext sqlx.ExtContext,
	table, cols, vals string,
	arg any,
) (int64, error) {
	query := "INSERT INTO " + table + " (" + cols + ")" +
		"\nVALUES (" + vals + ")"
	res, err := sqlx.NamedExecContext(ctx, ext, query, arg)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if id == 0 {
		return 0, ErrNoIdentityValue
	}
	return id, nil
}

// insertSQLServer is the SQL Server half of InsertReturningID.
func insertSQLServer(
	ctx context.Context,
	ext sqlx.ExtContext,
	table, cols, vals, pk string,
	arg any,
) (int64, error) {
	query := "DECLARE @Inserted TABLE (id BIGINT);" +
		"\nINSERT INTO " + table + " (" + cols + ")" +
		"\nOUTPUT INSERTED." + pk + " INTO @Inserted (id)" +
		"\nVALUES (" + vals + ");" +
		"\nSELECT id FROM @Inserted;"
	rows, err := sqlx.NamedQueryContext(ctx, ext, query, arg)
	if err != nil {
		return 0, err
	}
	defer func() { _ = rows.Close() }()

	if !rows.Next() {
		// An iteration error carries the real cause; without one, the
		// sentinel keeps a missing key from reading as id 0.
		if err := rows.Err(); err != nil {
			return 0, err
		}
		return 0, ErrNoIdentityValue
	}
	var id int64
	if err := rows.Scan(&id); err != nil {
		return 0, err
	}
	return id, rows.Err()
}

// ParseDialect maps a database/sql driver name onto a Dialect, ignoring
// case and surrounding space:
//
//	mysql                       DialectMySQL
//	sqlserver, mssql, azuresql  DialectSQLServer
//
// Any other name returns an error wrapping ErrUnsupportedDriver. A guess
// would hide the mistake until the first query failed.
//
// sqlx v1.4.0 has no bind type for "azuresql" and falls back to ?
// placeholders, which that driver does not rewrite. Register it with
// sqlx.BindDriver("azuresql", sqlx.AT) before using that name with sqlx.
func ParseDialect(driver string) (Dialect, error) {
	switch strings.ToLower(strings.TrimSpace(driver)) {
	case "mysql":
		return DialectMySQL, nil
	case "sqlserver", "mssql", "azuresql":
		return DialectSQLServer, nil
	}
	return 0, fmt.Errorf("%w: %q", ErrUnsupportedDriver, driver)
}

// SetDialect selects the grammar for the whole package. Call it once at
// startup, before the first query is built:
//
//	d, err := database.ParseDialect(driverName)
//	if err != nil {
//		return err
//	}
//	database.SetDialect(d)
//
// The helpers read the selection every time they run. It is stored
// atomically, so a later call is not a data race, but it is still a bug: a
// query built while the selection changes can mix both grammars.
//
// One selection per process is therefore a limit as well as a default, and
// the way around it is not to call SetDialect twice. A process that spells
// SQL for both engines at once calls the method on Dialect for every helper
// instead, and need never call SetDialect at all.
//
// SetDialect panics if d is neither DialectMySQL nor DialectSQLServer.
func SetDialect(d Dialect) {
	if d != DialectMySQL && d != DialectSQLServer {
		panic("SetDialect: invalid " + d.String())
	}
	selectedDialect.Store(int32(d))
}

// QuoteIdent quotes name as an identifier for the selected dialect, so a
// word that is reserved on only one engine parses on both; user, for
// example, is reserved in T-SQL but not in MySQL.
//
// MySQL gets backticks, which quote an identifier under every sql_mode,
// while double quotes do so only under ANSI_QUOTES. SQL Server gets
// brackets, which QUOTED_IDENTIFIER does not affect. A closing quote
// character inside name is doubled, as QUOTENAME does, so the result is
// always exactly one identifier.
//
// Both engines accept a quoted identifier wherever a bare one is legal.
// Quoting only the names that need it keeps logged SQL readable.
func QuoteIdent(name string) string {
	return activeDialect().QuoteIdent(name)
}

// QuoteIdent is QuoteIdent in the grammar d rather than the selected one.
func (d Dialect) QuoteIdent(name string) string {
	if d.isMySQL() {
		return "`" + strings.ReplaceAll(name, "`", "``") + "`"
	}
	return "[" + strings.ReplaceAll(name, "]", "]]") + "]"
}

// NowExpr returns SQL for the current date and time on the database server,
// for timestamp columns that a statement sets from the server clock instead
// of a bound value. MySQL reports it in the session time zone and SQL Server
// in the host's local time, and neither carries an offset.
//
//	MySQL       NOW()          whole seconds
//	SQL Server  SYSDATETIME()  DATETIME2(7), 100-nanosecond steps
//
// Both engines round a value into a column with fewer fractional digits
// (MySQL unless TIME_TRUNCATE_FRACTIONAL is set). NOW() leaves MySQL
// nothing to round, so a DATETIME(0) column stores the current second. SQL
// Server rounds SYSDATETIME() into a DATETIME2(0) column, which can then
// hold a time up to half a second ahead of the clock. Where that matters,
// truncate explicitly: DATETRUNC(second, SYSDATETIME()) does so on SQL
// Server 2022 and later.
func NowExpr() string {
	return activeDialect().NowExpr()
}

// NowExpr is NowExpr in the grammar d rather than the selected one.
func (d Dialect) NowExpr() string {
	if d.isMySQL() {
		return "NOW()"
	}
	return "SYSDATETIME()"
}

// InsertReturningID inserts one row and returns the integer key the
// database generated for it.
//
// table is the target, already qualified; cols is the comma-separated
// column list and vals the matching VALUES list, whose :name placeholders
// bind from arg as in sqlx.NamedExecContext; pk is the identity column.
// All four are concatenated into the statement, so they must come from
// code, never from a request. vals may contain expressions, for example
// "COALESCE(:created_at, " + NowExpr() + ")".
//
// MySQL reports the key through LastInsertId. go-mssqldb does not implement
// LastInsertId, so on SQL Server the statement returns the key as a result
// set instead:
//
//	DECLARE @Inserted TABLE (id BIGINT);
//	INSERT INTO table (cols)
//	OUTPUT INSERTED.pk INTO @Inserted (id)
//	VALUES (vals);
//	SELECT id FROM @Inserted;
//
// The table variable is required. SQL Server rejects an OUTPUT clause
// without INTO (Msg 334) on a target table with an enabled trigger for the
// statement's action, and nothing here can see the schema.
//
// ErrNoIdentityValue means no key came back: LastInsertId reported 0 on
// MySQL, which is what a table without an AUTO_INCREMENT column produces,
// or the result set was empty on SQL Server. A key of 0 from SQL Server is
// returned as is, since an identity can be seeded at zero. A key that does
// not fit in an int, possible only where int has 32 bits, is an error.
func InsertReturningID(
	ctx context.Context,
	ext sqlx.ExtContext,
	table, cols, vals, pk string,
	arg any,
) (int, error) {
	return activeDialect().InsertReturningID(
		ctx, ext, table, cols, vals, pk, arg)
}

// InsertReturningID is InsertReturningID in the grammar d rather than the
// selected one. ext still has to be bound to a connection of the engine d
// spells, which nothing here can check.
func (d Dialect) InsertReturningID(
	ctx context.Context,
	ext sqlx.ExtContext,
	table, cols, vals, pk string,
	arg any,
) (int, error) {
	var (
		id  int64
		err error
	)
	if d.isMySQL() {
		id, err = insertMySQL(ctx, ext, table, cols, vals, arg)
	} else {
		id, err = insertSQLServer(ctx, ext, table, cols, vals, pk, arg)
	}
	if err != nil {
		return 0, err
	}
	n := int(id)
	if int64(n) != id {
		return 0, fmt.Errorf("identity value %d overflows int", id)
	}
	return n, nil
}

// ApplyLimit caps a finished SELECT at n rows. On MySQL it appends LIMIT n;
// on SQL Server it inserts TOP (n) after SELECT, or after SELECT DISTINCT
// or SELECT ALL, where T-SQL requires it.
//
// The query is returned unchanged when n <= 0, so a cap that may be absent
// needs no conditional at the call site. It is also returned unchanged, on
// both engines, when it does not open with the SELECT keyword. That is a
// bug in the caller, but an uncapped query still runs, while a cap spliced
// into an unrecognised shape would be a syntax error.
//
// What the text cannot show remains the caller's responsibility:
//
//   - Nothing follows the final clause. LIMIT comes after ORDER BY but
//     before FOR UPDATE or a semicolon, so a query ending in either cannot
//     be capped by appending.
//   - The query is one SELECT, not a UNION, INTERSECT or EXCEPT. LIMIT
//     caps the whole compound, but TOP caps only its first branch.
//   - The query is neither paginated nor counted. TOP cannot share a query
//     scope with OFFSET/FETCH, and a COUNT over a capped query counts at
//     most n rows.
//
// Without an ORDER BY, both engines return an arbitrary subset.
//
// n is formatted into the text rather than bound, which keeps an args map
// out of the signature; an int cannot inject anything.
//
// "Opens with the SELECT keyword" means the word followed by white space,
// after any leading white space. Three shapes therefore go by uncapped on
// both engines: a query opening with a CTE, as RecursiveCTE builds; one
// opening with a comment; and the legal but unspaced "SELECT*FROM t". None
// is a reason to loosen the match, because every alternative splices the cap
// into text this function has not actually recognised.
func ApplyLimit(query string, n int) string {
	return activeDialect().ApplyLimit(query, n)
}

// ApplyLimit is ApplyLimit in the grammar d rather than the selected one.
func (d Dialect) ApplyLimit(query string, n int) string {
	mysql := d.isMySQL()
	if n <= 0 {
		return query
	}
	rest, ok := cutKeyword(query, "SELECT")
	if !ok {
		return query
	}
	if mysql {
		return query + "\nLIMIT " + strconv.Itoa(n)
	}
	if r, ok := cutKeyword(rest, "DISTINCT"); ok {
		rest = r
	} else if r, ok := cutKeyword(rest, "ALL"); ok {
		rest = r
	}
	head := query[:len(query)-len(rest)]
	return head + "TOP (" + strconv.Itoa(n) + ") " + rest
}

// PageClause returns the pagination tail for the selected dialect and
// stores the offset and limit it binds in args. page is 1-based.
//
// Append it last, to a query that ends in an ORDER BY on a unique key. SQL
// Server rejects OFFSET/FETCH without ORDER BY; MySQL accepts LIMIT without
// one, but then two requests for adjacent pages can overlap or skip rows.
//
// The engines disagree at the edges: SQL Server rejects a negative offset
// and a FETCH of zero rows, while MySQL returns an empty page for LIMIT 0.
// PageClause therefore raises page and limit to at least 1 and saturates an
// offset that would overflow, so both engines receive the same valid tail.
// A caller that gives zero a meaning of its own, such as "no pagination",
// has to act on it before calling.
//
// The two tails name their parameters in opposite order. sqlx.Named emits
// positional arguments in the order the names occur in the text, so one
// args map serves both; a query bound with a positional slice would have to
// follow the order of its tail.
//
// To count the matching rows, wrap the query as it was before its ORDER BY
// and this tail were added. SQL Server rejects an ORDER BY in a derived
// table that has no TOP or OFFSET, and a COUNT over a page counts only the
// page. The offset and limit keys left in args do no harm there, because
// sqlx.Named ignores keys the text does not reference.
func PageClause(args map[string]any, page, limit int) string {
	return activeDialect().PageClause(args, page, limit)
}

// PageClause is PageClause in the grammar d rather than the selected one.
func (d Dialect) PageClause(args map[string]any, page, limit int) string {
	mysql := d.isMySQL()
	if page < 1 {
		page = 1
	}
	if limit < 1 {
		limit = 1
	}
	offset := math.MaxInt
	if page-1 <= math.MaxInt/limit {
		offset = (page - 1) * limit
	}
	args["offset"] = offset
	args["limit"] = limit

	if mysql {
		return "\nLIMIT :limit OFFSET :offset"
	}
	return "\nOFFSET :offset ROWS FETCH NEXT :limit ROWS ONLY"
}

// LikeArg returns s as a contains-pattern for LikePredicate: s with its LIKE
// metacharacters escaped, between % wildcards. A search for "50%" then
// matches only values that contain "50%", not every value containing "50".
func LikeArg(s string) string {
	return "%" + likeEscaper.Replace(s) + "%"
}

// LikePredicate returns "col LIKE :param ESCAPE '!'", the predicate that
// matches a pattern from LikeArg. col must be a column expression chosen in
// code, and param is the bind name without its leading colon.
func LikePredicate(col, param string) string {
	return col + " LIKE :" + param + " ESCAPE '" + likeEscape + "'"
}

// RecursiveCTE opens a recursive common table expression named name, up to
// and including the opening parenthesis:
//
//	MySQL       WITH RECURSIVE name AS (
//	SQL Server  WITH name AS (
//
// MySQL and MariaDB require RECURSIVE for a CTE that references itself, and
// T-SQL has no such keyword, so the word is mandatory on one engine and a
// syntax error on the other.
//
// name is concatenated into the text and must be an identifier chosen in
// code. The body is the caller's, and three rules keep it portable:
//
//   - Give every anchor column a distinct name, such as "0 AS depth". SQL
//     Server rejects a CTE column without one.
//   - Keep each recursive column the type of its anchor column. SQL Server
//     rejects a mismatch; MySQL types the column from the anchor alone and
//     rejects or truncates wider values from the recursive member.
//   - Bound the recursion in the query itself, with a depth column. SQL
//     Server stops at MAXRECURSION (100 by default) and MySQL at
//     cte_max_recursion_depth (1000), both with an error, but MariaDB stops
//     at max_recursive_iterations with only a warning and returns the rows
//     gathered so far.
func RecursiveCTE(name string) string {
	return activeDialect().RecursiveCTE(name)
}

// RecursiveCTE is RecursiveCTE in the grammar d rather than the selected
// one.
func (d Dialect) RecursiveCTE(name string) string {
	if d.isMySQL() {
		return "WITH RECURSIVE " + name + " AS ("
	}
	return "WITH " + name + " AS ("
}

// String returns the canonical driver name of d, or Dialect(n) for a value
// that is not a grammar.
func (d Dialect) String() string {
	switch d {
	case DialectMySQL:
		return "mysql"
	case DialectSQLServer:
		return "sqlserver"
	}
	return "Dialect(" + strconv.Itoa(int(d)) + ")"
}
