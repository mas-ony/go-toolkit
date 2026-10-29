// Package crud is the repository and service layer a service would
// otherwise write again for every table: a repository base that derives a
// table's SELECT, INSERT, UPDATE and DELETE from a description of the
// table, the query builder a repository's own Read filters with, and a
// generic service over the repository for a table that needs nothing
// more.
//
// It builds on database, which assembles clauses and names no table, and
// it names no table either. A service declares one TableSpec per table,
// embeds BaseRepo in its repository, and writes by hand only what a
// TableSpec cannot describe, with the same builders.
//
// # Four files, two layers
//
// table.go describes a table. repo.go and query.go are the repository
// layer: BaseRepo with the statements it derives from a TableSpec, and the
// Query a repository's own Read assembles and Fetch runs. service.go is
// the service layer: BaseService, which implements Service over any
// Repository, and Unpack, FirstOf and CreateEach, the three steps a
// hand-written service method repeats.
//
// # The repository shape
//
// A repository embeds BaseRepo by value and hands NewBaseRepo a function
// that wraps a BaseRepo back into the repository:
//
//	type unitRepository struct {
//		crud.BaseRepo[Unit, int, UnitRepository]
//	}
//
//	func wrapUnit(
//		b crud.BaseRepo[Unit, int, UnitRepository],
//	) UnitRepository {
//		return &unitRepository{b}
//	}
//
//	func NewUnitRepository(db *sqlx.DB) UnitRepository {
//		return &unitRepository{crud.NewBaseRepo(db, &unitSpec, wrapUnit)}
//	}
//
// WithTx, WithNamespace and WithYear return a copy with one field changed,
// handed back through that function as the repository's own interface, so
// they chain in any order and none of them mutates the receiver. That is
// the shape the database package documents under "Scoped copies", and
// Scoped is its interface. Create, Update, Patch and Delete are promoted
// from the base as well; a repository whose statement differs declares a
// method of the same name, which shadows the promoted one.
//
// # Errors are returned, never logged
//
// BaseRepo logs nothing. Every error it returns names the table and the
// method, as "<table>.<Method>: <cause>", and wraps the cause, so
// errors.Is and errors.As still reach the driver's error and the database
// package's sentinels. Wrap gives a hand-written repository method the
// same form.
//
// The one exception is sql.ErrNoRows, which Update, Patch and Delete
// return as database/sql spells it when no row matched, so a caller may
// compare against it as well as match it with errors.Is.
//
// Logging belongs to whoever decides what a failure means. A repository
// that logged and returned would log every failure twice, once without
// the request it belonged to, and its line could not say whether the
// failure was a 404, a 500 or no failure at all. The HTTP layer logs a
// failure once, where it answers 500. A caller that swallows an error,
// such as an access check that reads a failed lookup as "no", has to log
// it there, because nothing below it will.
//
// BaseService logs one Info line per row it writes, under the "entity"
// key: "created" and "deleted" with the id, "updated" without. It logs no
// failure, for the same reason.
//
// # Missing rows
//
// A lookup by key that finds nothing is an empty result, not an error,
// which is the Read contract of the database package; FirstOf turns it
// into a nil row. A write that matches nothing returns sql.ErrNoRows,
// which RequireOneRow reads from the statement's own count rather than
// from a second query.
//
// That count needs one setting on MySQL, which by default reports the
// rows an UPDATE CHANGED rather than the rows it MATCHED: ClientFoundRows.
// database.New sets it, so re-saving a row unchanged still counts the
// row, as SQL Server always does. A pool opened any other way must set it
// as well, or an unedited save answers sql.ErrNoRows on MySQL alone.
//
// # Where text reaches the SQL
//
// Values are always bound. Identifiers are written in code, and each
// reaches a statement by one route:
//
//	table name         TableSpec.Name, through database.Qualify
//	written columns    TableSpec.Key, Writable, Updatable and Patchable
//	SELECT list        TableSpec.Cols, through database.SelectClause
//	ORDER BY           TableSpec.SortCols, through database.SortClause
//	search columns     TableSpec.SearchCols, as database.LikePredicate
//	joins and filters  the text a repository hands the join and And
//	                   helpers
//
// A column the statements write, the key included, is quoted when it
// would not parse bare on both engines, by the rule database.Qualify
// applies to a table name, so a column named user or rank is written
// correctly. Each such column is also the :name its value binds to, so it
// has to be a plain identifier: an ASCII letter or underscore, then ASCII
// letters, digits and underscores. A reserved word is one; a name with a
// space is not.
//
// The expressions in Cols, SearchCols, SortCols and DefaultSort are the
// repository's own SQL and reach the statement as written. A reserved
// name in one of them is quoted there, with database.QuoteIdent, which
// needs the dialect selected first, so such a spec is built after
// database.SetDialect rather than as a package-level value.
//
// # Process-wide state
//
// None of its own. NewBaseRepo copies database.DefaultNamespace, so call
// database.Configure before constructing a repository. Statements read
// the selected dialect as they are built, so database.SetDialect has to
// run before the first query, though not before the first constructor.
//
// # The service layer
//
// Service is the contract of a table keyed by an int the database
// generates, and BaseService implements it over any Repository. A table
// with nothing extra declares its service as an alias of Service and
// returns a BaseService:
//
//	type UnitService = crud.Service[Unit, UnitReadOption]
//
//	func NewUnitService(
//		log zerolog.Logger,
//		repo UnitRepository,
//	) UnitService {
//		return crud.NewBaseService[Unit]("unit", log, repo, unitByID)
//	}
//
// A service with more embeds *BaseService and adds methods, or shadows
// the ones that differ. A table with a natural key writes its service by
// hand, while BaseRepo serves its repository all the same.
//
// # What the tests hold in place
//
// The unit suite runs every statement against a fake driver that records
// what it receives, under both grammars and both placeholder styles, so
// the text of each statement is asserted without a server.
//
// The integration suite runs the same statements against real servers.
// It sits behind the integration build tag and reads the DSN variables
// the database package documents:
//
//	go test -tags integration -run Integration ./crud
//
// What only it can show is that each engine accepts the statements as
// written, a reserved column name included, and that RequireOneRow counts
// the rows an UPDATE matched under ClientFoundRows and the rows it changed
// without it. Its tables are created and dropped by the tests, so the
// DSNs must point at a scratch database.
package crud
