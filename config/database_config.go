package config

// The database.* section of config.yaml.
//
// Keys in this section, with their environment-variable spellings:
//
//	database.driver
//		DATABASE_DRIVER
//	database.host
//		DATABASE_HOST
//	database.port
//		DATABASE_PORT
//	database.catalog
//		DATABASE_CATALOG
//	database.schema
//		DATABASE_SCHEMA
//	database.username
//		DATABASE_USERNAME
//	database.password
//		DATABASE_PASSWORD
//	database.encrypt
//		DATABASE_ENCRYPT
//	database.conn_max_idle_time
//		DATABASE_CONN_MAX_IDLE_TIME
//	database.conn_max_lifetime
//		DATABASE_CONN_MAX_LIFETIME
//	database.max_idle_conns
//		DATABASE_MAX_IDLE_CONNS
//	database.max_open_conns
//		DATABASE_MAX_OPEN_CONNS
//
// Twelve keys, and that is the whole section — NewDatabaseConfig below reads
// exactly these. The environment spelling holds only for a Viper built by
// NewViper; see the package documentation.
//
// Two of those twelve name things, and what the second one is worth depends on
// the driver:
//
//	key                sqlserver / mssql      mysql
//	database.catalog   the CATALOG opened     the DATABASE
//	database.schema    required ("dbo")       must be ""
//
// database.catalog is the database the connection OPENS, on both. It is the
// one value every branch of buildDSN reads, and it is the same object under
// two names — a catalog on SQL Server, a database on MySQL.
//
// database.schema is the level between that database and the table. SQL Server
// has one and requires it here; MySQL has none, so a value there is a syntax
// error waiting to happen and Validate rejects it.
//
// The namespace every query is qualified with is rendered from the two by
// Namespace() below:
//
//	sqlserver / mssql   <catalog>.<schema>.<table>
//	mysql               <catalog>.<table>
//
// Both shapes LEAD with the catalog, and that is what makes per-database
// scoping in the repository layer a single rule rather than a per-driver one:
// a query is re-pointed at a sibling database — another year, another tenant,
// an archive — by rewriting the first segment, on either driver.
//
// database.username and database.password belong in the environment —
// DATABASE_USERNAME and DATABASE_PASSWORD — and never in a file that is
// committed. Validate requires both, which is what makes that stick: a
// config.yaml that leaves them blank is not a loadable configuration on
// its own, so a deployment cannot quietly start on credentials somebody
// checked in.

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/viper"
)

// driverKind classifies a driver by the ONE property this package
// has to reason about: how many levels its identifiers carry above the table
// name.
//
//	driverSQLServer  catalog.schema.table   3 parts
//	driverMySQL      database.table         2 parts
//
// Both lead with the CATALOG — the database the connection opened — and both
// permit a query to name a catalog other than that one. That shared property
// is what portability across the three accepted driver names costs and buys:
// not a DSN branch, which the database package's db.go owns, but the shape of
// every table reference the repository layer emits. See Namespace below, which
// is the whole mechanism.
type driverKind int

// DatabaseConfig holds connection, naming, and connection-pool settings for
// the single pool this service opens. String omits Username and Password
// deliberately, and the database package keeps both out of its errors; the
// note on Username says what can still name the login in a log.
//
// The section is OPTIONAL to an application that can run without a
// database. Left out, every field is zero, Configured reports false, and
// database.New answers with database.ErrNotConfigured rather than dialling.
// Supplied at all, from the file or the environment, it is validated in
// full, so a section filled in halfway fails at startup instead of quietly
// switching the database off.
type DatabaseConfig struct {
	// Driver is the database driver name.
	//
	// Normalised to lowercase by NewDatabaseConfig, so config.yaml and the
	// environment may spell it any way. A struct built by hand (db_test.go
	// does this) skips that step, which is why kind() lowercases again rather
	// than trusting the field.
	//
	// Allowed values: mysql | mssql | sqlserver
	Driver string

	// Host is the hostname or IP address of the database server.
	Host string

	// Port is the TCP port of the database server.
	//
	// Validate only requires it to be non-zero. Any other value passes here
	// and is refused by database.New, which names the key before it builds a
	// DSN — 70000 above the range, and -1 below it. AppConfig.Validate
	// range-checks app.port instead; the note in Validate explains why the two
	// rules reach opposite conclusions on purpose.
	//
	// Common defaults: MySQL → 3306, MSSQL → 1433
	Port int

	// Database is the database the connection OPENS, on both drivers. Mapped
	// from the YAML key "catalog" to keep the config driver-neutral; the Go
	// struct field is named Database for clarity in code.
	//
	// It is the one value every branch of buildDSN reads — the "database"
	// parameter on sqlserver, mysql.Config.DBName on mysql — and it is the
	// same object under two names:
	//
	//	sqlserver / mssql   a CATALOG on the instance; also the first part of
	//	                    the namespace, <catalog>.<schema>.<table>
	//	mysql               a DATABASE on the server; also the whole
	//	                    namespace, <database>.<table>
	//
	// The one property both rows share is what the rest of the service is
	// built on: this value is the LEADING segment of every qualified table
	// reference, and a query may name a catalog other than the one the session
	// opened. That is what lets the repository layer swap in a sibling
	// database by rewriting one segment, with no per-driver branch.
	Database string

	// Schema is the level between the database and the table, and exists on
	// one of the two drivers.
	//
	//	sqlserver / mssql   required — "dbo" unless migrations ran as a
	//	                    principal with a different default schema. The
	//	                    MIDDLE part of <catalog>.<schema>.<table>.
	//	mysql               must be EMPTY — MySQL has no schema level, and
	//	                    db.schema.table is a syntax error, not a subtle
	//	                    resolution difference.
	//
	// Validate enforces both rules, so a driver switch that leaves this key
	// behind fails at startup naming the key, rather than at the first query
	// with a syntax error naming a table.
	//
	// There is no silent default on the driver that takes one. An absent key
	// fails Validate exactly the way database.encrypt does, for the same
	// reason: "dbo" is right for almost every deployment and wrong for the
	// ones that matter, and a value arrived at by omission is a value nobody
	// chose.
	Schema string

	// Username is the database login name. Required.
	//
	// Not logged by this package: String omits it. Nor does the database
	// package quote the connection string that carries it. A DSN the driver
	// refuses comes back from database.New as ErrMalformedDSN, with the
	// driver's own message, which could quote the DSN, deliberately dropped.
	//
	// One route remains, and it carries the login NAME only. A failed ping
	// is wrapped with the driver's error, and a server that refuses a login
	// says which login it refused: "Login failed for user 'svc'" on SQL
	// Server, "Access denied for user 'svc'" on MySQL.
	Username string

	// Password is the database login password. Required, and not logged
	// here. Unlike the login name, it is not part of the login-failure
	// message a server sends back either.
	//
	// Leave both blank in config.yaml and inject them at runtime as
	// DATABASE_USERNAME / DATABASE_PASSWORD. Blank is not merely tidier: an
	// absent value fails Validate with "database.username is required", which
	// names the key an operator has to fix, whereas a placeholder sails
	// through Validate and fails later as a login error that does not.
	//
	// There is deliberately no list of characters to avoid in the PASSWORD,
	// and adding one would reject working credentials rather than merely
	// restate a rule. buildDSN hands the value to a constructor that owns the
	// escaping on both drivers, so any byte survives intact.
	// TestBuildDSNRoundTripsHostileCredentials parses each DSN back with the
	// driver's own parser and asserts the credentials come out byte-identical,
	// covering ; : # % @ / ? & = " { } and space.
	//
	// The USERNAME has exactly one exception, and it is on mysql:
	// url.UserPassword escapes for the MSSQL URL, but mysql.Config.FormatDSN
	// writes both fields RAW and ParseDSN recovers them by splitting on the
	// first ":" — so "us:er" would log in as "us" with the rest folded into
	// the password. buildDSN refuses that username with ErrInvalidCredential
	// rather than emitting a DSN that parses cleanly into the wrong login;
	// TestMySQLRejectsAColonInTheUsername pins it. A colon in the password is
	// unaffected, because the split has already been made by the time it is
	// reached.
	Password string

	// Encrypt controls TLS negotiation for MSSQL / SQL Server connections.
	//
	//	disable  No TLS at all.
	//	false    TLS for the login packet only; the rest of the session,
	//	         including every query and result set, travels in clear text.
	//	         This is the SQL Server default and the one most people mean
	//	         when they think they have encryption.
	//	true     TLS for the whole session. The server certificate is
	//	         verified unless trustservercertificate is also set, so a
	//	         self-signed certificate fails here — that failure is the
	//	         usual reason a deployment ends up back on "disable".
	//	strict   TDS 8.0 (SQL Server 2022+): TLS before the TDS handshake,
	//	         certificate always verified.
	//
	// The vocabulary above is SQL Server's; the key is not. The database
	// package's encryptModes translates each value into the TLS parameter
	// the configured driver understands, so the setting means the same thing
	// on all three driver names:
	//
	//	database.encrypt   sqlserver encrypt=   mysql tls=
	//	disable            disable              false
	//	false              false                preferred
	//	true               true                 true
	//	strict             strict               true
	//
	// Validate requires it for both drivers, because both read it.
	//
	// Two mismatches the table cannot show:
	//
	//   - "false" is weaker off SQL Server than on it. SQL Server still
	//     encrypts the login packet at "false", so credentials never cross in
	//     clear text; MySQL's "preferred" is opportunistic AND unverified, so
	//     a server offering no TLS gets a plaintext session.
	//   - "true" and "strict" are one setting on MySQL. It has no analogue of
	//     TDS 8.0, and verifies chain and hostname already at "true". The two
	//     stay distinct here so a deployment can change engines without
	//     editing this key.
	//
	// The verifying values need a certificate the client trusts, and no key
	// here supplies a private root — see the third note on encryptModes for
	// what that costs and why the mapping was not loosened to skip-verify /
	// require instead.
	//
	// "disable" is defensible only on a private network segment where the
	// database is unreachable from outside.
	//
	// Both the presence AND the value are checked — see the note in Validate.
	//
	// Accepted values: "disable" | "false" | "true" | "strict"
	Encrypt string

	// ConnMaxIdleTime is the maximum time a connection may remain idle in the
	// pool before it is closed. Prevents stale connections from accumulating
	// when traffic drops off.
	//
	// The failure it prevents is specific: a firewall or load balancer between
	// the app and the database silently drops idle NAT entries after a few
	// minutes, and the next query on such a connection fails with a reset
	// rather than a clean error. Keep this below the shortest idle timeout
	// on the path, and do not assume that is long: Azure Load Balancer drops
	// an idle flow after 4 minutes by default, without a reset, and an AWS
	// NAT gateway after 350 seconds. 3m clears both.
	//
	// Applied via db.SetConnMaxIdleTime in the database package's
	// applyPoolSettings. Both duration keys are required by Validate, and
	// both must be at least one second. database/sql reads zero AND any
	// negative value as "never expire", which is exactly the behaviour that
	// produces the reset above; and a bare number is nanoseconds, which
	// closes every connection the moment it goes idle. Always write a unit.
	ConnMaxIdleTime time.Duration

	// ConnMaxLifetime is the maximum total lifetime of a pooled connection
	// before it is recycled. Forces credential / config refresh and defends
	// against server-side silent disconnection of long-lived connections.
	//
	// Distinct from ConnMaxIdleTime: this one fires on a busy connection too.
	// It is what lets a password rotation or a failover to a new replica take
	// effect without a restart, since only a fresh handshake picks either up.
	// Keep it comfortably above ConnMaxIdleTime, or idle eviction never gets a
	// chance to run first. Validate does not enforce that ordering — 3m
	// idle against a 30m lifetime is the shape to copy.
	//
	// Required and at least one second, like ConnMaxIdleTime and for the
	// same reasons: zero or a negative value never recycles a connection,
	// and a bare number recycles every connection after each use.
	ConnMaxLifetime time.Duration

	// MaxIdleConns is the maximum number of idle connections kept open in the
	// pool. Setting this too low causes excessive connection churn under load.
	// Must not exceed MaxOpenConns when MaxOpenConns > 0.
	//
	// Setting MaxIdleConns to 0 disables the idle pool entirely — every
	// connection is closed as soon as it is no longer in use. This eliminates
	// pool overhead but causes a full TCP + auth handshake on every request,
	// which is usually slower than maintaining a small pool. Use 0 only in
	// environments where the database enforces a strict connection-count cap
	// and even idle connections count against that cap.
	//
	// Viper footgun: an ABSENT database.max_idle_conns key also reads as 0,
	// and applyPoolSettings calls SetMaxIdleConns unconditionally, so
	// forgetting the key does not fall back to database/sql's default of 2 —
	// it silently disables idle pooling. Always set it explicitly in
	// config.yaml.
	MaxIdleConns int

	// MaxOpenConns is the maximum number of open (in-use + idle) connections.
	// 0 means unlimited — not recommended in production.
	// Set close to MaxIdleConns for a steady-state pool with minimal churn.
	//
	// Viper footgun: an ABSENT database.max_open_conns key also reads as 0, so
	// forgetting the key silently removes the connection cap. Always set it
	// explicitly in config.yaml.
	//
	// Sizing: this is the ceiling on concurrent queries, so it also bounds how
	// many requests can be in a handler at once. Too high and the server's own
	// connection limit or memory becomes the bottleneck; too low and requests
	// queue inside database/sql with no visible error, only latency.
	// Setting it equal to MaxIdleConns gives a fixed-size pool that neither
	// grows nor churns — the simplest behaviour to reason about.
	MaxOpenConns int
}

// The driver kinds. driverUnknown is the zero value on purpose: it is what
// a lookup in validDriverKinds yields for a name with no entry, so an
// unrecognised driver needs no second signal.
const (
	driverUnknown driverKind = iota
	driverSQLServer
	driverMySQL
)

// validDriverKinds is the allowlist of supported database drivers checked by
// DatabaseConfig.Validate before a connection is attempted, and simultaneously
// the table that decides how a qualified table name is spelled for each.
//
// Membership is presence in this map: a driver with no entry here is rejected
// by Validate, and there is no second list to keep in step with this one. That
// pairing is deliberate — a bare set of names would let a driver pass
// validation without this package knowing how many parts its qualified table
// references carry.
//
// Lookups go through DatabaseConfig.kind, which lowercases and trims first, so
// "MySQL" and " sqlserver " both resolve. NewDatabaseConfig normalises the
// value once on the way in, so validation, buildDSN's own switch, and
// namespace rendering all see one spelling — a capitalised driver name cannot
// fail validation with a message about a driver the DSN builder would have
// accepted.
var validDriverKinds = map[string]driverKind{
	"sqlserver": driverSQLServer,
	"mssql":     driverSQLServer,
	"mysql":     driverMySQL,
}

// validEncryptModes is the allowlist of database.encrypt checked by
// DatabaseConfig.Validate. The vocabulary is SQL Server's, which is the
// deployed target; the database package's encryptModes translates each value
// into MySQL's tls= so the key means the same thing on either driver.
//
// The split is deliberate: this package owns which values are LEGAL, that one
// owns what each MEANS to a driver, and neither needs the other's table. They
// can drift — a value added here and not there — but the consequence is a
// startup error from buildDSN naming the value, not a connection that quietly
// comes up unencrypted. That asymmetry is why the duplication is tolerable
// where the driver allowlist's would not have been.
var validEncryptModes = map[string]struct{}{
	"disable": {},
	"false":   {},
	"true":    {},
	"strict":  {},
}

// kind classifies the configured driver. An unrecognised name yields
// driverUnknown, which is the map's zero value and the same signal Validate
// reports as an error.
//
// Lowercases and trims rather than reading Driver directly, because a
// DatabaseConfig built as a struct literal in a test never passes through
// NewDatabaseConfig's normalisation.
func (c *DatabaseConfig) kind() driverKind {
	return validDriverKinds[strings.ToLower(strings.TrimSpace(c.Driver))]
}

// namespace renders one logical catalog into a prefix for the configured
// driver.
//
// Two shapes, one per driverKind, and the catalog leads in both:
//
//	driverSQLServer  <catalog>.<schema>  both levels, joined
//	driverMySQL      <catalog>           Schema ignored; MySQL has no such
//	                                     level
//
// Ignoring rather than merging is safe on MySQL because Validate has already
// rejected a non-empty Schema there, so it can only affect a struct built by
// hand. It must not silently produce a three-part name that no MySQL server
// can parse.
//
// An unknown driver takes the MySQL path. It cannot reach a query: Validate
// fails the config and buildDSN returns ErrUnsupportedDriver, so this branch
// exists only so the method has no panicking or surprising case.
func (c *DatabaseConfig) namespace(catalog string) string {
	catalog = strings.Trim(strings.TrimSpace(catalog), ".")
	schema := strings.Trim(strings.TrimSpace(c.Schema), ".")

	switch c.kind() {
	case driverSQLServer:
		switch {
		case catalog == "":
			return schema // "" or "dbo" — bare or schema-qualified, both valid
		case schema == "":
			return catalog
		default:
			return catalog + "." + schema
		}

	default:
		return catalog
	}
}

// NewDatabaseConfig reads DatabaseConfig fields from the provided Viper
// instance.
//
// Never returns an error: absent keys and uncastable values both come back as
// zero values, which Validate rejects afterwards.
//
// This function is the authoritative list of keys the database.* section
// supports. A key present in config.yaml but missing here is dead weight —
// Viper never looks it up, so neither the file nor an environment variable can
// supply it.
//
// Key mapping note: the YAML key for the login's default database is "catalog"
// (driver-neutral terminology), but the Go struct field is named "Database"
// (idiomatic Go). The explicit v.GetString("database.catalog") call here makes
// this mapping visible and intentional.
//
// Driver is lowercased and trimmed here so that validation, DSN construction,
// and namespace rendering all see one spelling. Nothing downstream re-derives
// it from the raw config value.
//
// Every IDENTIFIER key is trimmed on the way in — driver, catalog, schema —
// because a stray space in one of them is always a typo and never a value.
// Namespace() trims what it renders too, so an untrimmed key here would make
// the stored value and the rendered prefix disagree.
//
// Username, Password, and Host are deliberately NOT trimmed. A password may
// legitimately begin or end with a space and silently editing one would
// produce a login failure this package caused; Host is left alone for symmetry
// with them, and a space there fails at dial time naming the address.
func NewDatabaseConfig(v *viper.Viper) *DatabaseConfig {
	driver := strings.ToLower(
		strings.TrimSpace(v.GetString("database.driver")))
	encrypt := strings.ToLower(
		strings.TrimSpace(v.GetString("database.encrypt")))

	return &DatabaseConfig{
		Driver:          driver,
		Host:            v.GetString("database.host"),
		Port:            v.GetInt("database.port"),
		Database:        strings.TrimSpace(v.GetString("database.catalog")),
		Schema:          strings.TrimSpace(v.GetString("database.schema")),
		Username:        v.GetString("database.username"),
		Password:        v.GetString("database.password"),
		Encrypt:         encrypt,
		ConnMaxIdleTime: v.GetDuration("database.conn_max_idle_time"),
		ConnMaxLifetime: v.GetDuration("database.conn_max_lifetime"),
		MaxIdleConns:    v.GetInt("database.max_idle_conns"),
		MaxOpenConns:    v.GetInt("database.max_open_conns"),
	}
}

// Namespace returns the qualifier prefix every table reference in the
// repository layer is built from.
//
// This is what the application hands to database.Configure, and it is the only
// place the driver's identifier grammar is applied. Everything downstream
// concatenates this prefix and a table name with a dot and knows nothing about
// which engine it is talking to — so a wrong prefix here is a wrong table
// reference in every query the service emits.
//
//	sqlserver  Catalog=appdb Schema=dbo
//	           -> "appdb.dbo" -> appdb.dbo.records
//	mysql      Catalog=appdb Schema=""
//	           -> "appdb"     -> appdb.records
//
// Both rows lead with database.catalog. That is the property per-database
// scoping relies on — it rewrites the first segment and leaves the rest
// alone — and it holds on either driver without a branch.
//
// An empty result means "emit the bare table name", which resolves against the
// connection's default database. Validate rejects an empty database.catalog,
// so that only happens for a hand-built config.
func (c *DatabaseConfig) Namespace() string { return c.namespace(c.Database) }

// Configured reports whether any database.* setting reached this section,
// which is how an application that treats the section as optional decides
// whether to open a pool at all. A nil receiver reports false.
//
// It asks whether ANY field is set, not whether the section is usable;
// that is Validate's question. The two agree after startup, because a
// section that is supplied at all is validated in full: a service that
// started with Configured false had no database section, and one that
// started with it true had a valid one.
func (c *DatabaseConfig) Configured() bool {
	return c != nil && *c != (DatabaseConfig{})
}

// Validate returns a joined error for every invalid or missing DatabaseConfig
// field.
//
// Deliberately unchecked:
//
//   - Whether the namespace actually EXISTS. Nothing here queries the server,
//     so a typo in database.catalog is a query-time error. The startup ping
//     proves only that the login can reach the server.
//   - Whether database.schema names a schema that EXISTS inside
//     database.catalog on sqlserver. Nothing here can ask, for the same reason
//     as the bullet above. What is checked is that it was NAMED, which is the
//     part a config can be wrong about silently: Namespace() renders it
//     verbatim as the middle segment of every qualified table reference.
//   - Port range. Anything non-zero passes, including 70000, which
//     database.New refuses, naming the key, before it builds a DSN.
//     AppConfig.Validate DOES range-check app.port, on the grounds that
//     net.Listen is the last thing to run and fails after the pool is open
//     and every route is registered. The argument is weaker here: the
//     database package rejects the port before any route exists, so the
//     failure is already early and already names the key. Two rules reaching
//     opposite conclusions rather than one shared rule, so the divergence
//     stays visible as a decision rather than reading as an oversight in one
//     of the two.
//   - ConnMaxLifetime > ConnMaxIdleTime. Inverting them is a misconfiguration
//     rather than an error: idle eviction simply never fires first.
//
// The VALUE of Encrypt is checked here rather than left to the driver, because
// the database package's encryptModes TRANSLATES it: on mysql there is no
// driver to forward an unknown value to, so an unrecognised value has no
// meaning anywhere in the process. One practical consequence: go-mssqldb
// accepts anything strconv.ParseBool does, so a deployment carrying
// "encrypt: 0" or "encrypt: 1" fails at startup and needs the canonical
// spelling. Case is forgiven, since the value is lowercased before it is
// compared, so "True" reads as "true".
//
// "mssql" and "sqlserver" are both accepted and map to the same registered
// driver; "mssql" is the legacy alias. Nothing depends on which one is used.
//
// Every check appends rather than returning early, so one restart surfaces
// every database.* problem at once.
func (c *DatabaseConfig) Validate() error {
	// errs is declared before the nil check only so the two statements read in
	// the same order in all Validate implementations; the nil check is what
	// must come first, since every line after it dereferences c.
	var errs []error
	if c == nil {
		return errors.New("database config was not initialised")
	}

	kind := c.kind()
	if kind == driverUnknown {
		errs = append(errs, fmt.Errorf("database.driver must be one of: "+
			"sqlserver, mssql, mysql (got %q)",
			c.Driver))
	}
	if c.Host == "" {
		errs = append(errs, errors.New("database.host is required"))
	}
	if c.Port == 0 {
		errs = append(errs, errors.New("database.port is required"))
	}
	// The message spells out what a catalog IS on each driver, because the key
	// is one word and the thing it names goes by two: a bad guess here
	// connects successfully and then cannot find a table.
	if c.Database == "" {
		errs = append(errs, errors.New(
			"database.catalog is required: it names the database this "+
				"connection opens (a catalog on sqlserver/mssql, a database "+
				"on mysql), and is also the first part of every qualified "+
				"table name on both"))
	}
	if c.Username == "" {
		errs = append(errs, errors.New("database.username is required"))
	}
	if c.Password == "" {
		errs = append(errs, errors.New("database.password is required"))
	}

	// The naming rules are the portability contract, and every one of them
	// moves a failure from the first query to startup, where the message names
	// a key.
	//
	// All of them are skipped for an unrecognised driver: the driver error
	// above is the real problem, and a follow-on complaint about
	// database.schema would only point at the wrong line.
	switch kind {
	case driverSQLServer:
		if c.Schema == "" {
			errs = append(errs, errors.New(
				`database.schema is required for the mssql and sqlserver `+
					`drivers: a SQL Server table reference is `+
					`<catalog>.<schema>.<table> and the middle part has no `+
					`default here — use "dbo" unless the migrations ran as `+
					`a principal whose default schema is something else`))
		}
	case driverMySQL:
		if c.Schema != "" {
			errs = append(errs, fmt.Errorf("database.schema must be empty "+
				"for the mysql driver (got %q): MySQL has no schema level "+
				"between the database and the table, so db.schema.table "+
				"is a syntax error rather than a different lookup",
				c.Schema))
		}
	}

	// Encrypt is required for EVERY driver, and its value is checked against
	// the vocabulary rather than forwarded. The two halves hold each other
	// up: the key is required everywhere because the database package's
	// encryptModes gives it a meaning everywhere, and it can only be
	// translated if it is one of the four.
	//
	// Not gated on kind: an unrecognised driver has already produced its own
	// error above, and an empty or misspelled encrypt is still worth reporting
	// in the same pass rather than on the next restart.
	//
	// The value is normalised again here for the same reason kind()
	// re-lowercases the driver — a DatabaseConfig built as a struct literal
	// never passed through NewDatabaseConfig.
	switch enc := strings.ToLower(strings.TrimSpace(c.Encrypt)); enc {
	case "":
		errs = append(errs, errors.New(
			`database.encrypt is required: use "disable", "false", "true", `+
				`or "strict". It is not an mssql-only key — the value is `+
				`translated into MySQL's tls= as well, and an omitted key `+
				`would mean that driver connected unencrypted with nothing `+
				`in the config saying so`))
	default:
		if _, ok := validEncryptModes[enc]; !ok {
			errs = append(errs, fmt.Errorf(`database.encrypt must be one `+
				`of: disable, false, true, strict (got %q). The short `+
				`strconv.ParseBool spellings the SQL Server driver would `+
				`take on its own, such as "0", "1", "t" and "f", are `+
				`rejected here, because the value has to be translated for `+
				`the mysql driver`,
				c.Encrypt))
		}
	}

	// Both durations: zero is the absent value and reported as required;
	// anything else under a second is out of range. That range holds both
	// mistakes worth catching — a negative value, which database/sql reads
	// as "never", and a bare number, which Viper reads as nanoseconds.
	switch {
	case c.ConnMaxIdleTime == 0:
		errs = append(errs, errors.New(
			"database.conn_max_idle_time is required"))
	case c.ConnMaxIdleTime < time.Second:
		errs = append(errs, fmt.Errorf("database.conn_max_idle_time must "+
			"be at least 1s (got %s): database/sql never closes an idle "+
			"connection when this is negative, and if this was meant as "+
			"seconds, write the unit — a bare number is nanoseconds",
			c.ConnMaxIdleTime))
	}
	switch {
	case c.ConnMaxLifetime == 0:
		errs = append(errs, errors.New(
			"database.conn_max_lifetime is required"))
	case c.ConnMaxLifetime < time.Second:
		errs = append(errs, fmt.Errorf("database.conn_max_lifetime must be "+
			"at least 1s (got %s): database/sql never recycles a connection "+
			"when this is negative, and if this was meant as seconds, write "+
			"the unit — a bare number is nanoseconds",
			c.ConnMaxLifetime))
	}
	// MaxIdleConns / MaxOpenConns invariant, covering the three checks below.
	// Neither field has a meaningful negative value, and database/sql would
	// silently reinterpret one as "unlimited" or "no idle connections", so
	// both are rejected on their own terms first. MaxIdleConns=0 (disable the
	// idle pool) is explicitly allowed and not flagged.
	//
	// The comparison is then guarded by `MaxOpenConns > 0`, because the pool
	// has no open-connection cap below that and any MaxIdleConns is valid
	// there. The guard is `> 0` and not `!= 0` because database/sql treats
	// EVERY n <= 0 as unlimited: under `!= 0` a negative max_open_conns would
	// reach the comparison and report "max_idle_conns (25) cannot exceed
	// max_open_conns (-1)" — an accurate sentence about the wrong problem.
	if c.MaxIdleConns < 0 {
		errs = append(errs, fmt.Errorf("database.max_idle_conns cannot be "+
			"negative (got %d)",
			c.MaxIdleConns))
	}
	if c.MaxOpenConns < 0 {
		errs = append(errs, fmt.Errorf("database.max_open_conns cannot be "+
			"negative (got %d)",
			c.MaxOpenConns))
	}
	if c.MaxOpenConns > 0 && c.MaxIdleConns > c.MaxOpenConns {
		errs = append(errs, fmt.Errorf("database.max_idle_conns (%d) cannot "+
			"exceed max_open_conns (%d)",
			c.MaxIdleConns,
			c.MaxOpenConns))
	}

	return errors.Join(errs...)
}

// String returns a loggable representation of DatabaseConfig.
//
// The pointer receiver means fmt only picks this up for a *DatabaseConfig.
// Printing a value copy (%v on DatabaseConfig, not &DatabaseConfig) bypasses
// it and dumps the struct fields directly — credentials included.
//
// Username and Password are absent from the format string deliberately: this
// struct DOES hold secrets, and that omission is the entire point of the
// method.
//
// The namespace is printed as the RENDERED prefix as well as the raw values,
// because the rendered form is what ends up in every query and is the thing
// worth confirming in a startup log after a driver change.
func (c *DatabaseConfig) String() string {
	if c == nil {
		return "<nil DatabaseConfig>"
	}
	return fmt.Sprintf("Driver=%s "+
		"Host=%s "+
		"Port=%d "+
		"Database=%s "+
		"Schema=%s "+
		"Namespace=%s "+
		"Encrypt=%s "+
		"ConnMaxIdleTime=%s "+
		"ConnMaxLifetime=%s "+
		"MaxIdleConns=%d "+
		"MaxOpenConns=%d",
		c.Driver,
		c.Host,
		c.Port,
		c.Database,
		c.Schema,
		c.Namespace(),
		c.Encrypt,
		c.ConnMaxIdleTime,
		c.ConnMaxLifetime,
		c.MaxIdleConns,
		c.MaxOpenConns,
	)
}
