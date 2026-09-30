package database

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"

	// Blank import: this package is never referenced by name. It is imported
	// for the side effect of its init() calling sql.Register, which is what
	// makes the driver name passed to sqlx.Open resolvable. Drop this line and
	// the driver fails at Open with "unknown driver", not at compile time.
	//
	// go-sql-driver/mysql is deliberately NOT here: buildDSN calls
	// mysql.NewConfig, so it is a named import in the group above. Its
	// init() registers the driver either way — the registration side effect
	// does not depend on how the import is spelled, only on the package being
	// linked in.
	//
	// Both are linked into the binary regardless of which one the config
	// selects, so the driver set is fixed at build time and the choice at
	// runtime is only which registered name gets used.
	_ "github.com/microsoft/go-mssqldb" // registers "sqlserver" + "mssql"

	"github.com/mas-ony/go-toolkit/config"
)

// encryptMode holds one row of the translation from database.encrypt to each
// driver's own TLS parameter. See encryptModes.
type encryptMode struct {
	mssql string // encrypt=  (go-mssqldb)
	mysql string // tls=      (go-sql-driver/mysql)
}

// pingTimeout bounds the startup connectivity check in New.
//
// It exists for one failure mode: a host that silently DROPS packets rather
// than refusing them — a wrong IP, a firewall with a DROP rule, a security
// group that never got the port. Without a deadline that case costs the OS
// TCP connect timeout, which on Linux defaults to somewhere over two minutes,
// during which the process sits mute before dying. Whoever is watching the
// container sees a hang, not a misconfiguration.
//
// A refused connection or a rejected login is unaffected: both return an error
// immediately, so this value never delays a fast failure. That is also why a
// longer timeout buys almost nothing — the only thing extra seconds could
// rescue is a server that accepts TCP but takes many seconds to complete a
// login, which on a LAN is a sign of trouble rather than something to wait
// out.
//
// Five seconds is roughly fifty times a healthy LAN handshake (prelogin, a
// TLS handshake under every encrypt mode but "disable", then auth), so it is
// generous for the good case and short enough that a bad deploy fails while
// someone is still looking at it. "disable" sends ENCRYPT_NOT_SUP in prelogin
// and negotiates no TLS at all, which is the difference between it and
// "false" — see the first note on encryptModes.
//
// One interaction to keep in mind: a verifying mode ("true", "strict") adds a
// certificate exchange and validation to the handshake this deadline covers.
// That is milliseconds on a LAN, but a server whose chain forces an OCSP or
// CRL fetch can spend seconds inside it, and the failure looks like an
// unreachable host rather than a certificate problem.
const pingTimeout = 5 * time.Second

// dialTimeout bounds a single TCP connect, at startup and for every
// reconnection the pool makes afterwards.
//
// pingTimeout above covers STARTUP only. Anything else depends on the
// context each query carries, and a caller passing context.Background()
// leaves a pool that has to re-dial mid-run with no deadline of its own to
// fall back on. The failure that exposes is the same one pingTimeout
// defends the first connect against — a host that drops packets rather than
// refusing them — arriving later, once a firewall rule changes or a server
// reboots behind a NAT that has already forgotten the flow. Without a dial
// deadline that connection waits out the OS TCP timeout, over two minutes
// on Linux, holding a pool slot the whole time.
//
// The two drivers do not start level here, which is the reason this exists as
// a setting rather than a default left alone. go-mssqldb sets DialTimeout to
// fifteen seconds per protocol handler unless the DSN says otherwise;
// mysql.Config.Timeout is zero, which sets no deadline of its own and leaves
// the operating system's TCP timeout, minutes on Linux. Matching go-mssqldb's
// number rather than inventing one keeps the two engines failing the same
// way, which is the property the rest of this package is built for.
//
// It is deliberately LONGER than pingTimeout, so at startup the ping's own
// deadline still fires first and New reports the DeadlineExceeded branch with
// its "host unreachable or dropping packets" message. A shorter value would
// hand back the driver's own i/o timeout instead — diagnosable, but not the
// message this package took the trouble to write.
//
// Only ONE branch reads it. The sqlserver DSN omits "dial timeout" because
// this value restates go-mssqldb's default, so the constant GOVERNS mysql and
// merely DESCRIBES sqlserver. That is the reverse of the argument made at
// my.TLSConfig, where "false" is emitted rather than omitted so the DSN states
// the choice instead of leaving it to be inferred, and it holds only while the
// two numbers agree: a driver bump that moves msdsn's default moves
// sqlserver's dial deadline with nothing here saying so. Setting the key on
// both branches is the fix if that matters more than the extra parameter.
//
// Only the DIAL is bounded. mysql.Config also offers ReadTimeout and
// WriteTimeout, and neither belongs here: they abort a query already in
// flight, so any value low enough to catch a dead connection is also low
// enough to kill a slow report. Bounding those is what a per-query context is
// for, and that is a separate change.
const dialTimeout = 15 * time.Second

// encryptModes translates the four values of database.encrypt into the TLS
// parameter each driver understands:
//
//	database.encrypt   sqlserver encrypt=   mysql tls=
//	disable            disable              false
//	false              false                preferred
//	true               true                 true
//	strict             strict               true
//
// The vocabulary is SQL Server's because one of the two drivers already
// spells it that way. Without this table the mysql branch would emit no TLS
// parameter at all and connect on whatever the driver defaulted to —
// unencrypted — while the configuration carried an "encrypt" setting that
// read as if it applied. That is the gap this table closes, and it is why
// the key is required for both drivers rather than only the one whose
// vocabulary it uses.
//
// Three things the table does not say, all of which matter before trusting a
// value:
//
//   - "false" means genuinely different things. On SQL Server it still
//     encrypts the LOGIN PACKET, so credentials never cross the network in
//     clear text; only the session after it does. On MySQL the equivalent is
//     opportunistic and unverified: a server that offers no TLS gets a
//     plaintext connection, and one that does gets an unauthenticated tunnel.
//     Do not read "false" as "at least the password is safe" outside SQL
//     Server.
//
//   - "true" and "strict" collapse into one setting on MySQL. It has no
//     analogue of TDS 8.0, where TLS precedes the protocol handshake
//     rather than being negotiated inside it, and it already verifies the
//     certificate chain and the hostname at the "true" row. Keeping the two
//     distinct in config means a deployment can change engines without
//     editing this key, which is the entire point of the indirection.
//
//   - The verifying rows need a certificate the CLIENT trusts, and the trust
//     store is not configurable from here. MySQL verifies against the system
//     pool unless a tls.Config is registered with mysql.RegisterTLSConfig, and
//     this package does not expose that — so a private CA or a self-signed
//     server certificate fails at connect on "true" and "strict", the same
//     wall as SQL Server without trustservercertificate, and the usual reason
//     a deployment slides back to "disable". Adding a database.tls_root_cert
//     key is the fix if that comes up; loosening the mapping to skip-verify is
//     not, because it would make "true" mean "encrypted but unauthenticated"
//     on one of the two engines and nothing in the config would say so.
var encryptModes = map[string]encryptMode{
	"disable": {mssql: "disable", mysql: "false"},
	"false":   {mssql: "false", mysql: "preferred"},
	"true":    {mssql: "true", mysql: "true"},
	"strict":  {mssql: "strict", mysql: "true"},
}

// ErrUnsupportedDriver reports a driver name this package cannot use. New
// returns it when cfg.Driver matches no known driver, and ParseDialect
// wraps it when the same name maps to no Dialect.
//
// ONE sentinel for both, because it is one condition read twice: New needs
// the name to build a DSN and ParseDialect needs it to pick a grammar, and
// a deployment whose driver fails either has misspelled the same key. Two
// sentinels would make errors.Is depend on which of the two ran first.
//
// The two accept almost the same names. ParseDialect also takes
// "azuresql", a driver name that New cannot open; whichever of the two
// refuses a name, it refuses it with this sentinel.
//
// It is exported so callers can test for it with errors.Is rather than
// string-matching the error message.
var ErrUnsupportedDriver = errors.New("unsupported driver")

// ErrUnsupportedEncryptMode is returned by New when cfg.Encrypt is not one of
// the four values encryptModes translates. Exported for the same reason as
// ErrUnsupportedDriver.
//
// In the normal startup path this is unreachable:
// config.DatabaseConfig.Validate checks the same allowlist and fails first,
// naming the key. It exists because buildDSN cannot pass an unrecognised value
// through to the driver and let the driver complain — on MySQL there is
// nothing to pass it through TO, since the value has to be translated before
// it means anything.
var ErrUnsupportedEncryptMode = errors.New("unsupported encrypt mode")

// ErrInvalidCredential is returned by New when a credential contains a byte
// the configured driver's DSN grammar cannot carry. Exported for the same
// reason as the two sentinels above.
//
// Exactly one case reaches it: a ":" in the username on the mysql driver. The
// URL-based sqlserver DSN escapes every byte, so nothing there can trigger it.
// See the credential section of buildDSN's doc comment for why MySQL is the
// exception and why rejecting beats pretending to escape.
var ErrInvalidCredential = errors.New("invalid credential")

// ErrMalformedDSN is returned by New when the driver rejects the DSN buildDSN
// produced. The driver's own error is deliberately NOT wrapped — see the
// comment at the sqlx.Open call.
var ErrMalformedDSN = errors.New("malformed DSN")

// ErrNotConfigured is returned by New and NewContext when cfg is nil or
// holds no setting at all, which is what a deployment that leaves the
// optional database.* section out produces.
//
// It is a sentinel so that a service able to run without a database can
// tell that apart from a database it failed to reach; one that cannot run
// without one treats it as fatal like any other error from New.
// config.DatabaseConfig.Configured asks the same question before the call,
// which is the better place to branch.
var ErrNotConfigured = errors.New("database: not configured")

// isHostName reports whether host has the shape of something a DSN can
// carry as a host: a DNS name or an IPv4 address, made of ASCII letters,
// digits, "-", "." and "_", or an IPv6 address, which adds ":". It is a
// shape check and not a resolver, so nothing is looked up.
//
// Brackets are refused rather than stripped. net.JoinHostPort adds them
// to an IPv6 address itself, and a second pair makes an address neither
// driver can parse.
func isHostName(host string) bool {
	if host == "" {
		return false
	}
	for i := 0; i < len(host); i++ {
		c := host[i]
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		case c == '-', c == '.', c == '_', c == ':':
		default:
			return false
		}
	}
	return true
}

// encryptFor resolves cfg.Encrypt to its per-driver spellings.
//
// Lowercases and trims first, matching how the config constructor
// normalises the value, so a DatabaseConfig built as a struct literal
// behaves the same as one read from a config file.
//
// The allowlist is closed rather than forwarding the value to the driver,
// because this function TRANSLATES it: an unrecognised value has no defined
// meaning here and there is no driver left to hand it to. One practical
// consequence: go-mssqldb lowercases the value and then accepts a wider
// vocabulary than this table — "mandatory", "yes", "1" and "t" all mean the
// same thing to it as "true", and "optional", "no", "0" and "f" the same as
// "false" — so a configuration carrying "encrypt: 0" or "encrypt: yes"
// fails at startup and needs the canonical spelling. That failure names the
// key.
//
// Case and surrounding whitespace are NOT part of that: this function
// lowercases and trims before the lookup, so "True" and " STRICT " still work.
func encryptFor(encrypt string) (encryptMode, error) {
	mode, ok := encryptModes[strings.ToLower(strings.TrimSpace(encrypt))]
	if !ok {
		return encryptMode{}, fmt.Errorf(
			`%w: %q (want "disable", "false", "true", or "strict")`,
			ErrUnsupportedEncryptMode, encrypt)
	}
	return mode, nil
}

// checkTarget refuses the settings that would otherwise reach a driver's
// parser unchecked — database.host, database.port and app.location — and
// returns the loaded location, which the mysql branch hands to the driver.
//
// It exists for the sqlserver branch, where all three become text in the
// DSN that sqlx.Open hands to go-mssqldb. A value its parser rejects comes
// back from there as ErrMalformedDSN with the cause deliberately dropped,
// so a mistyped zone, a port of 70000 and a host written as a named
// instance would each read "malformed DSN", naming nothing. Here each names
// its key instead. The mysql branch runs the same checks, so that a
// setting means the same thing whichever engine reads it.
//
// The config type's own validation leaves exactly these three to this
// point: it requires the host and the location without checking their
// shape, and it does not range-check database.port. Its reasons hold,
// since this still runs before a single route is registered.
//
// No message quotes a credential, so each is safe to log verbatim.
func checkTarget(
	cfg *config.DatabaseConfig,
	app *config.AppConfig,
) (*time.Location, error) {
	switch {
	case strings.Contains(cfg.Host, `\`):
		return nil, fmt.Errorf(`database.host %q names a SQL Server `+
			`instance ("host\instance"), which this package cannot reach `+
			`by name: connect to the instance's TCP port instead`,
			cfg.Host)
	case !isHostName(cfg.Host):
		return nil, fmt.Errorf("database.host %q is not a host name or an "+
			"IP address (write an IPv6 address without brackets)",
			cfg.Host)
	}
	if cfg.Port < 1 || cfg.Port > 65535 {
		return nil, fmt.Errorf("database.port must be between 1 and 65535 "+
			"(got %d)",
			cfg.Port)
	}
	loc, err := time.LoadLocation(app.Location)
	if err != nil {
		return nil, fmt.Errorf("invalid app.location %q: %w",
			app.Location,
			err)
	}
	return loc, nil
}

// applyPoolSettings configures the sqlx connection pool.
//
// Ordering note: SetMaxOpenConns is called before SetMaxIdleConns by
// convention, but database/sql keeps the two limits consistent from both
// directions — SetMaxIdleConns clamps the idle limit to the current open
// limit, and SetMaxOpenConns shrinks an already-set idle limit when the new
// open limit is lower. The final pool configuration is therefore identical in
// either order.
//
// The clamp is a backstop rather than the live behaviour: the config type's
// own validation rejects MaxIdleConns > MaxOpenConns at startup naming both
// keys, so a validated config that would be silently clamped never reaches
// this function. That check is guarded by MaxOpenConns > 0, and the guard
// costs nothing here: a zero open limit means unlimited, so no idle value
// can exceed it and there is nothing to clamp. Only a DatabaseConfig built
// as a struct literal can arrive with an idle limit ABOVE the open one and
// have it silently lowered to match.
//
// Zero means something different on each of the first two setters, and on
// neither of them does it mean what a reader expects.
//
// SetMaxIdleConns(0) retains NO idle connections. database/sql's documented
// default of 2 applies only while the setter has never been CALLED, and this
// function calls it unconditionally, so an absent max_idle_conns key does not
// fall back to that default — it disables idle pooling, and every query pays
// a full TCP and auth handshake. SetMaxOpenConns(0) means UNLIMITED. A
// DatabaseConfig that omits both therefore gets an unbounded pool that closes
// each connection the moment it goes idle, which is the worst of the two
// shapes and the one nobody asks for.
//
// Negative is not a third case: both setters treat it exactly as they treat
// zero. There is no value that switches the idle limit off in the sense of
// "let database/sql decide" once this function has run.
//
// ConnMaxIdleTime and ConnMaxLifetime are independent:
//   - ConnMaxIdleTime evicts connections that have been sitting unused in the
//     pool for too long, preventing stale TCP connections after periods of low
//     traffic (e.g. overnight).
//   - ConnMaxLifetime recycles every connection regardless of activity,
//     forcing a fresh handshake at regular intervals. This is the defence
//     against server-side silent disconnects of long-lived connections and
//     ensures credential/config changes (e.g. password rotation) take effect
//     promptly. Under a verifying encrypt mode it is also what picks up a
//     rotated SERVER certificate without a restart.
//
// All four setters are safe to call on a live pool, so pool tuning could be
// made reloadable without reopening anything — the values simply take
// effect for subsequent checkouts. This function does not do that; it
// applies one configuration once.
func applyPoolSettings(db *sqlx.DB, cfg *config.DatabaseConfig) {
	db.SetMaxOpenConns(cfg.MaxOpenConns)
	db.SetMaxIdleConns(cfg.MaxIdleConns)
	db.SetConnMaxIdleTime(cfg.ConnMaxIdleTime)
	db.SetConnMaxLifetime(cfg.ConnMaxLifetime)
}

// buildDSN constructs the driver registration name and DSN for the configured
// driver. The returned dsn embeds credentials and must never be logged or
// included in error messages returned to the client.
//
// cfg.Driver is lowercased AND trimmed before matching, so configured
// values are case-insensitive and tolerate surrounding whitespace ("MySQL",
// "mysql", "MYSQL" and " mysql " all work) — the same normalisation
// encryptFor applies to the encrypt key, for the same reason. The config
// constructor already does it; this repeats the step because a
// DatabaseConfig built as a struct literal never passed through it.
//
// Each branch below carries the reasoning for its own parameters. What follows
// here is only what spans both branches.
//
// # The returned driver name also selects sqlx's placeholder dialect
//
// This function's first return value is handed to sqlx.Open and becomes
// db.DriverName(), which sqlx.BindType maps to a placeholder style. A caller
// that builds queries with :name placeholders depends on that mapping: it
// lets sqlx.Named expand them to positional ?, then calls db.Rebind to
// rewrite those into the driver's real syntax.
//
//	"sqlserver"  ->  AT       ->  @p1, @p2, ...   ✓ what SQL Server wants
//	"mysql"      ->  QUESTION ->  ?               ✓
//	"mssql"      ->  UNKNOWN  ->  Rebind is a NO-OP
//
// That last row is why the sqlserver/mssql branch returns "sqlserver" for
// both config values even though go-mssqldb registers "mssql" too. The two
// are separate registrations rather than aliases, and they differ in how
// they treat query text: the "mssql" name rewrites ODBC-style placeholders,
// while "sqlserver" passes the query through untouched and expects native
// @p1 parameters. Returning the name the operator typed would look tidier
// and would break every query built this way: Rebind would pass the ?
// placeholders through unchanged, SQL Server would reject them as syntax
// errors, and the failure would appear at query time rather than at startup
// — with nothing in the message pointing back at the configured driver
// name.
//
// The same trap applies to any driver added later. Check sqlx's bind table
// before choosing what to return here; an unrecognised name degrades silently
// to UNKNOWN rather than erroring.
//
// # Credentials: one branch escapes, one rejects
//
// The sqlserver URL DSN hands the username and password to url.UserPassword,
// which owns the escaping. Every byte survives as a percent-escape the driver
// decodes back to the original, so a ";" cannot truncate the DSN.
//
// MySQL is NOT in that group. mysql.Config.FormatDSN writes the two fields
// RAW, and the enclosing guard is quoted with them because the paragraph
// below turns on it:
//
//	if len(cfg.User) > 0 {
//	    buf.WriteString(cfg.User)
//	    if len(cfg.Passwd) > 0 {
//	        buf.WriteByte(':')
//	        buf.WriteString(cfg.Passwd)
//	    }
//	    buf.WriteByte('@')
//	}
//
// and ParseDSN recovers them by splitting on the FIRST ":" before the last
// "@". A colon in the USERNAME therefore shifts the split — "us:er" logs in as
// "us" with the rest folded into the password — and the DSN grammar offers
// nothing to escape it with. The branch below refuses that username instead of
// emitting a string that parses cleanly into the wrong login.
//
// The guard is deliberately that narrow, and the narrowness is executed
// rather than asserted: this package's tests sweep every byte from 0x01 to
// 0x7e through FormatDSN and back through ParseDSN in each field
// independently, and ":" in the username is the only one that does not
// survive. A colon in the PASSWORD is fine, because by the time it is
// reached the split has already been made.
//
// One further MySQL asymmetry, guarded elsewhere: the outer
// `if len(cfg.User) > 0` above means an EMPTY username makes FormatDSN omit
// the credential section entirely, dropping the password with it. The
// config type's own validation requires a non-empty username, so this
// function does not check it again.
//
// The DATABASE NAME is the field a reader reaches for next, on the reasoning
// that it lands in the same raw DSN from the same config file — and it needs
// no guard, because it is the one field FormatDSN does escape: it writes
// url.PathEscape(cfg.DBName) and ParseDSN reads it back through
// url.PathUnescape. A name carrying "/" or "?" therefore survives intact
// rather than shifting the parser's split the way a colon in the username
// does. That is a property of the driver rather than of this function, so it
// is worth re-checking on a bump, which is what this package's round-trip
// tests are for.
//
// The escaping rules are stated once, by the tests that execute them rather
// than by a comment that can quietly go stale: each DSN is parsed back with
// the driver's own parser, and the MySQL username exception is pinned
// separately. A driver bump that changes escaping breaks those.
//
// # Three settings are checked before either DSN exists
//
// database.host, database.port and app.location reach the sqlserver DSN as
// text, and a value go-mssqldb's parser rejects comes back from sqlx.Open
// as ErrMalformedDSN, its cause dropped to keep the DSN out of the logs. So
// checkTarget refuses such values first, in both branches, and names the
// key. The credentials are not among them: those are escaped or refused
// above, never checked for shape.
func buildDSN(
	cfg *config.DatabaseConfig,
	app *config.AppConfig,
) (driver, dsn string, err error) {
	// Every branch needs host and port as one string, and net.JoinHostPort is
	// the only correct way to join them: a bare IPv6 literal has to be
	// bracketed ("[::1]:1433") or the trailing ":port" is unparseable. A plain
	// fmt.Sprintf("%s:%d") yields a broken address for any v6 host — the same
	// class of bug as the credential escaping, one level up.
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))

	// encryptFor is called inside each branch rather than once up here, so
	// that a config wrong in both places reports the DRIVER first. That is the
	// more fundamental of the two, and the encrypt vocabulary is only
	// meaningful once the driver is known to be one this function can serve.

	// Normalise to lowercase so config values are case-insensitive.
	switch strings.ToLower(strings.TrimSpace(cfg.Driver)) {
	case "sqlserver", "mssql":
		mode, merr := encryptFor(cfg.Encrypt)
		if merr != nil {
			return "", "", merr
		}
		// On this branch a host, port or zone the driver's parser rejects
		// would otherwise surface as ErrMalformedDSN naming no key; see
		// checkTarget. The location it loads is not needed here: the DSN
		// carries the zone by name.
		if _, terr := checkTarget(cfg, app); terr != nil {
			return "", "", terr
		}

		// URL form ("sqlserver://..."), not the ADO form ("key=value;"):
		// msdsn.getDsnType dispatches on the prefix and both forms land in the
		// same parameter map, so every key below means the same thing under
		// either. The difference is who escapes — url.UserPassword does it
		// here, whereas the ADO form pushes that job onto the caller.
		//
		// Path is left empty deliberately. splitConnectionStringURL reads a
		// URL path as a NAMED INSTANCE (server = host + "\" + path), not as
		// the database, so putting cfg.Database there would quietly point the
		// connection at an instance that does not exist. It goes in the query,
		// which is where the ADO form put it too.
		//
		// The four query keys:
		//
		//   - database is the catalog the session opens in, overriding the
		//     login's own default database. A caller that qualifies every
		//     query with its own namespace is unaffected by it; this only
		//     decides where an unqualified statement lands.
		//   - app name shows up in SQL Server's Activity Monitor
		//     (sys.dm_exec_sessions.program_name), which aids troubleshooting
		//     and auditing in shared environments.
		//   - encrypt controls TLS negotiation. The four accepted values reach
		//     the driver unchanged, travelling through encryptModes so the
		//     mysql branch can be given an equivalent — which is why an
		//     unrecognised value is rejected rather than forwarded.
		//   - timezone is the location go-mssqldb uses to label values it
		//     DECODES from offsetless column types (DATE, DATETIME, DATETIME2,
		//     TIME). See the note below.
		//
		// A fifth key is deliberately absent: "dial timeout". The driver
		// already applies the value dialTimeout matches, so setting it here
		// would restate a default rather than change anything. The mysql
		// branch has to set its equivalent because that driver sets no dial
		// deadline of its own.
		q := url.Values{}
		q.Set("database", cfg.Database)
		q.Set("app name", app.Name)
		q.Set("encrypt", mode.mssql)
		q.Set("timezone", app.Location)

		// On timezone, at more length, because it defaults to time.UTC and
		// that default is wrong for any schema whose offsetless columns
		// hold a LOCAL wall clock — a DATETIME2 defaulted from
		// SYSDATETIME(), say. Decoding as UTC relabels a local time as a
		// UTC one, and every timestamp the API serves is adrift by the
		// zone's whole offset.
		//
		// DECODES ONLY, which contradicts upstream's own field comment
		// ("encoding and decoding"). The call graph is what this paragraph
		// follows, not that comment, and it runs through two separate entry
		// points that are easy to conflate when re-checking:
		//
		//   - msdsn.EncodeParameters.GetTimezone, the exported method, is
		//     reached only from readFixedType, readByteLenTypeWithEncoding
		//     and readVariantTypeWithEncoding — all decode.
		//   - getTimezone(*Conn), the unexported helper wrapping it, is
		//     reached only from Stmt.makeParamExtra and Bulk.makeParam. Both
		//     are write-side, and neither is reachable unless a caller uses
		//     them: makeParamExtra consults the location for its civil.Date
		//     and civil.DateTime cases only, so a caller passing no civil
		//     types and issuing no bulk copies never enters either.
		//
		// A plain time.Time goes through makeParam, which sends
		// DATETIMEOFFSET(7) built from the VALUE's own offset; this parameter
		// is not consulted. Re-check both entry points on a go-mssqldb bump,
		// because the conclusion depends on them.
		//
		// The missing write half belongs to whatever type is written to
		// those columns: it has to convert into time.Local before the
		// driver sees the value. Paired with a marshaller that emits the
		// offset rather than normalising to UTC, both column families come
		// out right.
		//
		// An IANA name's slash is escaped by url.Values.Encode along with
		// every other query value, so it needs no special handling here.
		//
		// Two preconditions: app.Location must be a valid IANA name, and
		// the host must carry tzdata, since msdsn.Parse calls
		// time.LoadLocation and fails the DSN otherwise. checkTarget has
		// established both by the time this runs, loading the location the
		// same way, so a bad one is reported by name rather than as a
		// malformed DSN. On a scratch or distroless image, embed the
		// database with a blank import of time/tzdata.
		u := url.URL{
			Scheme:   "sqlserver",
			User:     url.UserPassword(cfg.Username, cfg.Password),
			Host:     addr,
			RawQuery: q.Encode(),
		}
		return "sqlserver", u.String(), nil

	case "mysql":
		mode, merr := encryptFor(cfg.Encrypt)
		if merr != nil {
			return "", "", merr
		}

		// The one credential check in this function, and the reason the
		// credential section above splits the two branches rather than
		// claiming both escape. FormatDSN writes the username unescaped and
		// ParseDSN splits on the first ":", so a colon here does not fail — it
		// silently produces a different login. Refusing is the only outcome
		// that stays honest.
		//
		// Checked before the DSN is assembled so the error names the config
		// key rather than arriving later as an authentication failure
		// against a username nobody typed. The value itself is kept out of
		// the message, which is what makes the error safe for a caller to
		// log verbatim.
		if strings.Contains(cfg.Username, ":") {
			return "", "", fmt.Errorf(
				`%w: database.username may not contain ":" on the mysql `+
					`driver — the DSN grammar has no escape for it and the `+
					`login would be silently truncated`,
				ErrInvalidCredential)
		}

		// The location is resolved before the Config is built because
		// my.Loc needs it and there is nothing useful to fall back to: UTC
		// would be the silent wrong answer this branch exists to avoid.
		// checkTarget loads it, beside the host and port checks both
		// branches share. A process that loads the same location at
		// startup fails before reaching here; it is handled anyway because
		// that ordering is a property of the caller, not of this package.
		loc, terr := checkTarget(cfg, app)
		if terr != nil {
			return "", "", terr
		}
		my := mysql.NewConfig()
		my.User = cfg.Username
		my.Passwd = cfg.Password
		my.Net = "tcp"
		my.Addr = addr
		my.DBName = cfg.Database

		// parseTime decodes DATETIME/TIMESTAMP as time.Time rather than
		// []byte, which is what datetime.Datetime.Scan's primary path expects.
		// Without it every time column takes Scan's []byte fallback instead.
		my.ParseTime = true

		// Loc labels offsetless columns with the configured zone rather
		// than UTC, so MySQL DATETIME values round-trip. Unlike
		// go-mssqldb, this driver applies Loc on the WRITE path too, which
		// is why a value type that converts before handing over finds this
		// conversion already done.
		my.Loc = loc

		// The dial deadline. Zero — the driver's default — means the OS TCP
		// timeout, which is minutes. See dialTimeout for why the number is
		// go-mssqldb's rather than one chosen here.
		my.Timeout = dialTimeout

		// clientFoundRows makes the server report rows MATCHED rather than
		// rows CHANGED in sql.Result.RowsAffected, and it is not a tuning
		// knob — it is what keeps a row count meaning the same thing on
		// both engines.
		//
		// A repository that reads RowsAffected() == 0 as "no such id" and
		// turns it into sql.ErrNoRows — and then a 404 — depends on the two
		// engines counting the same thing. On SQL Server @@ROWCOUNT counts
		// matched rows, so an UPDATE that sets a column to the value it
		// already holds still reports 1 and the row is found. MySQL's
		// default is the other convention: an UPDATE that changes nothing
		// reports 0.
		//
		// Without this flag, then, re-submitting an unedited form against a
		// row that exists returns 404 on MySQL and 204 on SQL Server, from
		// the same code, with nothing in either layer aware of the
		// difference — and the 404 is the more damaging direction, since a
		// client may reasonably treat it as "the record was deleted
		// underneath me".
		//
		// Turning it ON rather than teaching each row-count helper to tell
		// the engines apart: every such helper would need its own dialect
		// branch, and each is one more place the difference has to be
		// remembered. The DSN is where the engine is already known.
		//
		// The one other statement it changes is INSERT ... ON DUPLICATE KEY
		// UPDATE, and only in its no-op case: an update that leaves the row
		// as it was reports 1 instead of 0. A fresh insert still reports 1
		// and an update that changes the row still reports 2. This is the
		// note to re-read before trusting that statement's row count.
		my.ClientFoundRows = true

		// TLSConfig takes either a built-in name ("true", "false",
		// "skip-verify", "preferred") or a name handed to
		// mysql.RegisterTLSConfig. encryptModes produces three of those
		// built-ins — "true", "false" and "preferred" — and never
		// "skip-verify". ParseDSN can therefore always read back what
		// FormatDSN writes here, which this package's tests check.
		//
		// What separates the two omitted-verification names is NOT
		// verification. Config.normalize gives both the same
		// tls.Config{InsecureSkipVerify: true}; "preferred" additionally
		// sets AllowFallbackToPlaintext. So "preferred" is
		// opportunistic-and-unverified and "skip-verify" is
		// required-and-unverified, and the mapping avoids the latter
		// because nothing in the configuration would say that "false" had
		// become mandatory TLS. Neither authenticates the server — the
		// first note on encryptModes is the one to read before trusting the
		// "false" row.
		//
		// Note that "false" is emitted rather than omitted. The driver's
		// default with no tls parameter is also no TLS, so this changes
		// nothing at connect time; it makes the DSN state the choice instead
		// of leaving it to be inferred, which is the same reason the config
		// requires the key.
		my.TLSConfig = mode.mysql

		// FormatDSN is the driver's own serialiser and the inverse of
		// mysql.ParseDSN for everything except the username case guarded
		// above. It also emits only non-default parameters, so the DSN stays
		// short.
		return "mysql", my.FormatDSN(), nil

	default:
		return "", "", fmt.Errorf("%w: %q", ErrUnsupportedDriver, cfg.Driver)
	}
}

// New opens a validated, pooled database connection for the given
// configuration.
//
// This is the shortcut form of NewContext, for a caller that holds no
// context at the point it opens the pool — the usual case, since this runs
// before anything is being served. The deadline is the same either way; a
// caller-supplied context only adds a second way to end the wait.
//
// Steps:
//  1. Builds a driver-specific DSN from cfg (credentials are embedded; never
//     log it).
//  2. Opens the connection pool via sqlx.Open — this validates the DSN format
//     but performs no network I/O. The pool is not yet connected at this
//     point.
//  3. Applies pool settings (see applyPoolSettings for the clamping
//     behaviour).
//  4. Pings the server, under a pingTimeout deadline, to verify the
//     credentials and network path. If the ping fails the pool is closed
//     before returning so the caller never holds a reference to a broken pool.
//
// The deadline covers STARTUP only. Queries issued later carry whatever
// context their caller supplies, so a caller passing context.Background()
// gets no runtime bound from this or any other timeout here.
//
// A successful ping proves the network path, the credentials, and that the
// login can reach its default database (cfg.Database). It proves NOTHING
// about any QUALIFIED namespace a caller prefixes its queries with, which
// this ping never exercises — so a catalog that does not exist, or that
// this login cannot read, surfaces at the first query rather than here.
func New(cfg *config.DatabaseConfig, app *config.AppConfig) (*sqlx.DB, error) {
	return NewContext(context.Background(), cfg, app)
}

// NewContext is New with an explicit context, mirroring the WithTx /
// WithTxContext split in tx.go.
//
// ctx governs the startup ping and nothing else. The pool that NewContext
// returns outlives it: cancelling ctx afterwards closes no connection and
// aborts no query, because every query is bounded by the context its own
// caller passes. Handing a request context to this function would therefore
// be a category mistake rather than a tighter bound.
//
// The pingTimeout deadline still applies, and applies on top of ctx rather
// than instead of it: whichever of the two fires first ends the ping. A ctx
// that carries a shorter deadline shortens the wait; one that carries a
// longer deadline does not lengthen it, so pingTimeout stays the ceiling on
// how long a misconfigured host can hold up startup.
//
// What an already-cancelled ctx buys is the one case New cannot express: a
// process shutting down, or a startup sequence whose other half has already
// failed, stops waiting on a host that will never answer instead of
// spending pingTimeout finding out.
func NewContext(
	ctx context.Context,
	cfg *config.DatabaseConfig,
	app *config.AppConfig,
) (*sqlx.DB, error) {
	// Checked before anything else, so an absent section is reported as
	// absent rather than as an unsupported driver named "".
	if !cfg.Configured() {
		return nil, ErrNotConfigured
	}

	// A nil AppConfig is a wiring mistake rather than an absent section:
	// every deployment has an app.* section, and buildDSN reads its name and
	// location on both branches. Refused as an error here, where it would
	// otherwise be a nil dereference inside the DSN builder.
	if app == nil {
		return nil, errors.New("database: app config was not initialised")
	}

	driver, dsn, err := buildDSN(cfg, app)
	if err != nil {
		return nil, err
	}

	// sqlx.Open wraps sql.Open, which does not dial. Both drivers here
	// implement driver.DriverContext, so the DSN is parsed eagerly and a
	// malformed one fails right here rather than on first use.
	//
	// The driver's error is deliberately NOT wrapped. A driver reporting a
	// parse failure may quote the connection string it was given, and that
	// string contains the password; a caller that hands a startup error
	// straight to a fatal log call would then route a credential to the log
	// aggregator. The package doc promises the DSN never leaves this
	// package, and this is where that promise is either kept or broken.
	//
	// Neither pinned driver interpolates the DSN into a parse error.
	// go-mssqldb declines the cause on purpose — splitConnectionStringURL
	// returns a fixed "invalid URL format" with its own comment saying the
	// original may contain credentials — and go-sql-driver returns
	// package-level sentinels (errInvalidDSNUnescaped, errInvalidDSNAddr)
	// that interpolate nothing. So the guard protects against a driver bump
	// rather than against either driver as pinned.
	//
	// What is lost is the driver's description of what it disliked, which
	// is why buildDSN first checks the three settings a person can get
	// wrong in a way the driver's parser would reject — database.host,
	// database.port and app.location — and names the key when one is
	// unusable. What reaches this error is a DSN refused for a reason no
	// setting explains, and the driver and the sentinel are enough to
	// report that. Restore the cause with `: %w`, err if it ever proves
	// hard to diagnose — knowing that it puts credentials back in the logs.
	db, err := sqlx.Open(driver, dsn)
	if err != nil {
		return nil, fmt.Errorf(
			"opening %s connection: %w",
			driver,
			ErrMalformedDSN)
	}

	applyPoolSettings(db, cfg)

	// PingContext rather than Ping: see pingTimeout for what the deadline is
	// defending against. The deadline is derived from the CALLER's context
	// rather than from context.Background(), so the two bounds compose:
	// whichever of ctx and pingTimeout ends first ends the ping, and an
	// already-cancelled ctx returns without a packet being sent.
	//
	// cancel releases the timer as soon as NewContext returns, whichever
	// way it returns.
	ctx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()

	start := time.Now()
	if err := db.PingContext(ctx); err != nil {
		// Close the pool before returning so the caller never holds a
		// reference to a broken pool. sqlx.Open only validates the DSN — it
		// does not test the actual connection. Without this Close, the pool's
		// background connection-opener goroutine would leak for the lifetime
		// of the process even though no usable connection was ever
		// established.
		//
		// Closing also unblocks anything still dialling: PingContext returns
		// the moment the deadline fires, but the driver's in-flight dial is a
		// separate goroutine that would otherwise run to its own timeout.
		_ = db.Close()

		// Separate the two failures in the message, because they point at
		// completely different things to go and check. A deadline means the
		// host never answered — wrong address, or a firewall dropping rather
		// than refusing. Anything else means the server DID answer and said
		// no: bad credentials, unknown database, TLS refused, certificate not
		// trusted. Without this split both arrive as "ping failed" and the
		// first hour goes to working out which one it was.
		//
		// Both of these DO wrap the driver's error, which is the opposite
		// of the decision made at sqlx.Open a few lines up, so the
		// asymmetry is worth stating rather than leaving to look like an
		// oversight. What the Open guard defends against is a driver
		// quoting the CONNECTION STRING it was handed; a ping error is
		// raised after the DSN has already been parsed into a config
		// struct, and what it reports is the network or the server's own
		// refusal — an address, a TLS failure, a login rejection naming the
		// user at most. The password is not in that path in either driver.
		// And unlike a malformed DSN, which the configuration explains on
		// its own, a ping failure is undiagnosable without the cause.
		//
		// The deadline message reports the wait as measured rather than as
		// pingTimeout: a caller's context with a shorter deadline ends the
		// wait sooner, and the message should not name one that did not
		// happen.
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf(
				"ping timed out after %s (host unreachable or dropping "+
					"packets): %w",
				time.Since(start).Round(100*time.Millisecond),
				err)
		}
		return nil, fmt.Errorf("ping failed: %w", err)
	}

	return db, nil
}
