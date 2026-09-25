// Package database opens the application's connection pool, brackets a unit
// of work in a transaction, and builds the statements that run inside it on
// either of two engines. It defines no repository and no interface, and it
// is the only package here that knows a driver name.
//
// Five files, three jobs. db.go builds a driver-specific DSN, opens the
// pool and tunes it. tx.go runs a function inside a transaction and decides
// when that transaction commits, rolls back, or re-raises a panic.
// dialect.go, read_helpers.go and namespace.go assemble query text:
// respectively the grammar MySQL and SQL Server spell differently, the
// clause assembly they spell the same, and namespace-qualified table names.
//
// Nothing here names a real table or column. A service writes the
// repositories, one per table or view, and calls these helpers to build
// their statements.
//
// # This package holds process-wide state
//
// Two pieces: the selected dialect and the default namespace prefix. Both
// are set once at startup and read by every helper afterwards.
//
//	dbCfg := config.NewDatabaseConfig(v)
//	appCfg := config.NewAppConfig(v)
//
//	db, err := database.New(dbCfg, appCfg)
//	if err != nil {
//		return err
//	}
//	d, err := database.ParseDialect(dbCfg.Driver)
//	if err != nil {
//		return err
//	}
//	database.SetDialect(d)
//	database.Configure(dbCfg.Namespace())
//
// Configure takes the whole PREFIX a table name is qualified with —
// catalog.schema on SQL Server, the catalog alone on MySQL — which is what
// DatabaseConfig.Namespace builds. Passing the schema on its own is the
// easy mistake: SQL Server would then resolve every table against the
// login's default database rather than the configured one, and MySQL, where
// the config refuses a schema, would qualify nothing at all.
//
// SetDialect panics on an unknown dialect, and every helper that spells
// grammar panics until it has run, because a missing call is a wiring
// mistake that must fail at startup rather than emit SQL for the wrong
// engine. Configure has no such guard: an empty namespace means bare table
// names, which is a valid deployment.
//
// Neither is synchronised against the readers, so calling either while
// requests are in flight is a data race. One dialect and one prefix per
// process is a default rather than a limit: a repository reading another
// namespace receives it through its own WithNamespace method.
//
// # Neither piece of state is mandatory
//
// Every helper whose output depends on the engine has a second form, a
// method on Dialect, which takes the grammar as its receiver and reads no
// process-wide state:
//
//	database.QuoteIdent("user")   the selected grammar
//	d.QuoteIdent("user")          the grammar in d
//
// The two are otherwise identical — the package-level function is a
// one-line wrapper over the method — and the method form is what makes this
// package usable in a process that spells SQL for BOTH engines at once, a
// migration or a sync job between two systems, where a single selection
// cannot describe the work. A process of that shape never calls SetDialect
// and never trips the panic that guards it.
//
// The namespace prefix is already an argument to Qualify rather than state
// it reads, so Configure is a convenience from the start; DefaultNamespace
// is only where a constructor reaches when it is given nothing.
//
// Qualify resolves the grammar lazily, and only for a name that cannot
// appear bare, so a repository built from plain names qualifies correctly
// before SetDialect has run.
//
// # The exported surface
//
// New and NewContext for the pool, WithTx and WithTxContext for its
// transactions — each pair a shortcut and an explicit-context form of one
// function. ParseDialect, SetDialect and Configure for the wiring above.
// SortField, Dialect and the clause helpers named under the two headings
// below, each of the engine-dependent ones also a method on Dialect. Five
// sentinels — ErrUnsupportedDriver, ErrUnsupportedEncryptMode,
// ErrInvalidCredential, ErrMalformedDSN and ErrNoIdentityValue — each
// documented where it is declared, and each meant to be matched with
// errors.Is rather than by message text.
//
// # Configuration arrives as two structs and nothing else
//
// From config.DatabaseConfig everything, Schema included: New spends the
// rest on the DSN and the pool, and Configure spends Schema on the prefix
// Qualify joins to every table name. From config.AppConfig exactly Name and
// Location — Name to label the session on SQL Server, Location to decide
// how offsetless time columns are read. Env, Host and the rest of AppConfig
// are not consulted here.
//
// New hands back a *sqlx.DB rather than a wrapper of its own, so the pool
// belongs to the caller from the moment it is returned: nothing here closes
// it afterwards, and the process entry point is what defers db.Close. The
// single exception runs the other way — a New that fails its ping closes
// the pool it opened before returning the error, so no caller is left
// holding a reference to a pool that never connected.
//
// # The Read contract the query helpers are built for
//
//   - Single-row lookup (an id or another unique key is set): sql.ErrNoRows
//     is normalised to an empty result, so callers check the number of items
//     instead of inspecting the error.
//   - Paginated list (a page and a limit are set): count first, then fetch
//     the page.
//   - Plain list: every matching row, in a deterministic order.
//
// # Scoped copies
//
// The repository shape these helpers assume holds a *sqlx.DB, an optional
// *sqlx.Tx, a namespace prefix and a fiscal year. WithTx, WithNamespace and
// WithYear each return a shallow copy with one field changed, so they chain
// in any order and none of them mutates the receiver. Every copy keeps db,
// and two unexported methods complete the pattern: ns() combines the prefix
// with the year through NamespaceForYear, and ext() returns the transaction
// when one is set and db otherwise. Query methods call ext(), which is what
// lets a repository take part in a transaction without knowing that it is
// in one.
//
// # Where the engine differences live
//
// Nothing outside dialect.go branches on the configured engine. Every
// clause the two spell differently has a helper there — ApplyLimit,
// PageClause, NowExpr, RecursiveCTE, QuoteIdent, InsertReturningID,
// LikeArg and LikePredicate. A new difference belongs there
// rather than in a conditional at the call site, which is also why the
// selected dialect is not exported: a repository that can read it will
// eventually branch on it.
//
// The rest is the portable half: clause assembly spelled the same on both
// engines, plus the allowlists that decide which identifiers may reach the
// query text at all.
//
// Three ordering rules the type system cannot express:
//
//   - CountQuery runs before the ORDER BY and the page tail are appended,
//     because its derived table accepts neither.
//   - PageClause is appended after an ORDER BY. SortClause never returns an
//     empty string, and a builder that sorts on a fixed key appends a
//     literal ORDER BY.
//   - ApplyLimit and PageClause never meet in one query, and a capped query
//     is never counted.
//
// # Where user input reaches the SQL text
//
// Values are always bound. Only identifiers are ever concatenated, and each
// has a named boundary that limits it to something chosen in code:
//
//	column names in SELECT    SelectClause, through its allowed map
//	column names in ORDER BY  SortClause, through its allowlist
//	column names in GROUP BY  GroupClause, through its allowlist
//	namespace and table names Qualify, from literals in each repository
//	row caps                  ApplyLimit, from an int the repository passes
//	subqueries and aliases    ExistsFlag and AsText, from literals
//
// Everything else — every filter value, every LIKE pattern, every page
// offset — is a bound parameter. A helper that concatenates needs the same
// shape: an allowlist, or text written in code.
//
// The allowlists are the service's, not this package's. A helper given an
// empty or wrong allowlist drops every column and falls back, so a typo in
// one costs a wider result set rather than an injection.
//
// # Contexts
//
// Every helper that reaches the driver takes a context and hands it on, so
// pass the request's context down through the service layer. A read that
// starts from context.Background() leaves its statement running after the
// client that asked for it has gone.
//
// NewContext is the exception that proves the rule: its context governs the
// startup ping alone and does not outlive the call, because the pool it
// returns is bounded by whatever context each later query carries. Handing
// it a request context would be a category mistake rather than a tighter
// bound.
//
// # The DSN carries credentials
//
// buildDSN embeds the username and the password in the string it returns,
// and that string must never be logged, returned to a client, or
// interpolated into an error. config.DatabaseConfig keeps the same two
// values out of its String method; what is unique here is that the DSN
// joins them into one string that gives away both at once.
//
// New's two error paths treat their cause in opposite ways, and the
// asymmetry is deliberate. A failed sqlx.Open DROPS the driver's error and
// returns ErrMalformedDSN alone, because a driver reporting a parse failure
// may quote the string it was handed. A failed ping WRAPS its cause,
// because by then the DSN has been parsed into a config struct and what is
// left to report is an address, a TLS failure or a rejected login —
// undiagnosable without the cause, and carrying no password in either
// driver.
//
// # Both drivers are linked in; the config picks one
//
// go-mssqldb and go-sql-driver/mysql are compiled into every build, one as
// a blank import and one named because buildDSN calls mysql.NewConfig.
// Either spelling gets the init() that registers the driver, so the driver
// SET is fixed at build time and database.driver decides only which
// registered name is used.
//
// # The driver name is read twice
//
// buildDSN's first return value goes to sqlx.Open and becomes
// db.DriverName(), which sqlx.BindType reads to choose a placeholder style.
// That is why the first branch returns "sqlserver" for a configured
// "mssql" as well: go-mssqldb registers both names, they are separate
// registrations rather than aliases, and only "sqlserver" maps to a bind
// type. Under "mssql" Rebind silently becomes a no-op and every :name query
// reaches SQL Server with ? still in it, failing at query time with nothing
// in the message pointing back at the configured driver name. Check sqlx's
// bind table before choosing what a new branch returns; an unrecognised
// name degrades to UNKNOWN rather than erroring.
//
// ParseDialect reads the SAME configured value to pick a grammar, and
// accepts the same three spellings for SQL Server. Both report an
// unknown name with ErrUnsupportedDriver, so the two switches cannot
// disagree about whether a name is known — only about what it maps to.
//
// # The property any driver added here must have
//
// A qualified table reference LEADS with the catalog, and a query may name
// a catalog other than the one the session opened. Per-catalog scoping
// in namespace.go rests entirely on that, so a driver without the property
// cannot be added without also changing what qualification means there.
// How many parts a qualified name has is not part of the deal — SQL Server
// takes three and MySQL two — only which part comes first.
//
// # What the two engines are made to agree on
//
// Most of db.go is spent making one configuration produce the same
// BEHAVIOUR on both engines rather than the same parameters:
//
//   - TLS. encryptModes translates the four values of database.encrypt into
//     each driver's own parameter, so the key means something on MySQL
//     instead of quietly selecting that driver's plaintext default.
//   - Row counts. ClientFoundRows makes MySQL report rows MATCHED, the
//     convention SQL Server's @@ROWCOUNT already follows, so a repository
//     reading RowsAffected() == 0 as "no such id" cannot turn an unedited
//     re-save into a 404 on one engine only.
//   - Time. The sqlserver DSN carries timezone= and the mysql Config gets
//     ParseTime with Loc, so offsetless columns are labelled with
//     app.Location rather than UTC on either side.
//   - Dial deadlines. mysql.Config.Timeout defaults to no timeout at all,
//     so dialTimeout sets it to the value go-mssqldb already applies.
//
// Each is argued in full where it is set. What matters at this level is
// that none of them is a tuning knob: changing one changes what identical
// code MEANS on one of the two engines.
//
// # Where the agreement stops
//
// At transactions, and only there. With nil options a transaction runs at
// the engine's own default level — READ COMMITTED on SQL Server,
// REPEATABLE READ on InnoDB — and nothing here equalises that. The two
// sql.TxOptions extremes are each usable on exactly the engine the other is
// not: LevelSnapshot is sqlserver-only, ReadOnly: true is mysql-only, and
// each fails at BEGIN on the other rather than downgrading silently.
// Portable code passes nil or a level both accept.
//
// # What a successful New proves, and what it does not
//
// It proves the network path, the credentials, and that the login can reach
// its default catalog. It proves nothing about a catalog a query qualifies
// itself with, which the ping never exercises — that surfaces at the first
// query instead. It proves nothing about later connections either: the
// deadline covers STARTUP, and every query afterwards is bounded by
// whatever context its caller supplies, which for context.Background() is
// nothing at all.
//
// # Timeouts are constants here; pool sizes come from config
//
// pingTimeout (5s) and dialTimeout (15s) are constants rather than config
// keys, and the ordering between them is load-bearing: the ping's deadline
// has to fire first so a host that DROPS packets is reported as unreachable
// rather than as a driver i/o timeout. The four pool numbers are config's,
// and applyPoolSettings calls all four setters unconditionally — which is
// what makes an absent max_idle_conns mean "retain no idle connections"
// rather than database/sql's documented default of two.
//
// # Transactions do not nest
//
// database/sql has no savepoints. Calling either entry point from inside fn
// checks out a SECOND connection, which then waits on the row locks the
// first is still holding: a self-deadlock lasting until the lock timeout,
// not an error. Pass the *sqlx.Tx down through each repository's own WithTx
// method instead, which is why fn receives a concrete *sqlx.Tx rather than
// an interface.
//
// Errors keep their shape on the way out. A begin failure is wrapped, fn's
// error is returned untouched so callers can errors.Is their own sentinels,
// commit's error is returned as-is, and a panic is rolled back and
// re-raised rather than converted.
//
// # What the tests hold in place
//
// Two suites, and the split is by what each can prove without a server.
//
// The UNIT suite needs none and runs on every `go test`. The DSN tests
// parse each string back with the DRIVER's own parser, sweeping every byte
// from 0x01 to 0x7e through the username and the password separately; the
// one value that does not survive is a ":" in a MySQL username, which
// buildDSN refuses rather than pretends to escape. The transaction tests
// run against a fake driver, at the seam where begin, commit and rollback
// are observable at all. The clause tests assert query TEXT, which is all a
// string-building helper can be held to offline.
//
// The INTEGRATION suite needs a scratch server for at least one engine and
// is behind a build tag, so it is opt-in twice over:
//
//	DB_TEST_MYSQL_DSN
//		user:pass@tcp(host:3306)/scratch?parseTime=true&loc=Asia%2FJakarta
//	DB_TEST_SQLSERVER_DSN
//		sqlserver://user:pass@host?database=scratch&timezone=Asia%2FJakarta
//
// This suite alone passes with or without the zone parameters, because
// every test where time matters opens its pool through New, which adds
// them itself. They are shown because the datetime package reads the same
// two variables and cannot pass without them — and this suite passes with
// them — so one pair of values serves both.
//
//	go test -tags integration -run Integration ./database
//
// Every test runs once per engine whose DSN is set and skips when neither
// is. They create and drop their own tables, so the DSNs must point at a
// scratch database and never at one holding data.
//
// What it is FOR is the set of claims above that a string comparison
// cannot reach: that the clause helpers produce SQL each engine actually
// accepts, and that the four agreements db.go engineers — TLS, row counts,
// time zones, dial deadlines — hold against a running server rather than
// only in the DSN. ClientFoundRows is the clearest case: nothing offline
// can show that an UPDATE changing no column still reports one row matched.
//
// Both suites are therefore the driver-bump alarm as well: a change to
// either driver's escaping rules, to how database/sql sequences its
// cleanup, or to what either engine accepts, breaks a test here rather than
// a deployment.
//
// One limit worth keeping in view. A driver a given deployment never opens
// is exercised only through that driver's own parser, so any claim in this
// package about its RUNTIME behaviour was read out of its source rather
// than observed against a server — unless that engine's DSN was set when
// the integration suite last ran.
package database
