package config

// The app.* section of config.yaml.
//
// Keys in this section, with their environment-variable spellings:
//
//	app.name
//		APP_NAME
//	app.version
//		APP_VERSION
//	app.env
//		APP_ENV
//	app.host
//		APP_HOST
//	app.port
//		APP_PORT
//	app.location
//		APP_LOCATION
//	app.reports_base_url
//		APP_REPORTS_BASE_URL
//	app.upload_dir
//		APP_UPLOAD_DIR
//
// Eight keys, and that is the whole section: NewAppConfig reads exactly
// these. The environment spelling holds only for a Viper built by NewViper;
// see the package documentation.

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/spf13/viper"
)

// validEnvs is the allowlist checked by AppConfig.Validate.
//
// The empty-struct value is the idiomatic set: it occupies no memory, and the
// two-value map read in Validate reports membership without a sentinel value.
// Values are matched exactly — no trimming, no case folding — so "Production"
// is rejected. That strictness is deliberate: logger.New treats every
// unrecognised value as production, so a typo that slipped through would
// silently disable debug logging instead of failing at startup.
var validEnvs = map[string]struct{}{
	EnvDevelopment: {},
	EnvStaging:     {},
	EnvProduction:  {},
}

// Environment name constants used by app.env and the logger.
// Compare against these instead of bare string literals to avoid typos.
const (
	// Pretty-printed, colorized logs; DEBUG level.
	EnvDevelopment = "development"

	// JSON logs; INFO level.
	EnvStaging = "staging"

	// JSON logs; INFO level — the same logger behaviour as staging. The
	// separate value exists for future env-specific tuning.
	EnvProduction = "production"
)

// AppConfig holds application-level identity and runtime settings.
type AppConfig struct {
	// Name is the human-readable service name.
	//
	// Two consumers read it. NewFiberConfig joins it with Version into
	// fiber.Config.AppName, and the database package embeds it in a SQL
	// Server connection string as "app name=", so a connection is
	// identifiable in the server's own session list
	// (sys.dm_exec_sessions.program_name).
	//
	// The second is the one that matters in practice. AppName reaches the
	// framework's startup banner, which a JSON log pipeline usually turns
	// off; the session name is a handle on a running process that survives
	// whatever the logs are doing.
	//
	// Any byte is safe while the connection string is built in URL form,
	// which escapes a semicolon to %3B and reads it back intact. The older
	// "key=value;..." form would not be, since ";" is its field separator.
	//
	// It is not the Server response header, which the framework takes
	// verbatim from fiber.server_header and omits when that is empty.
	Name string

	// Version is the service's version string. NewFiberConfig appends it to
	// Name, with a build-time suffix when the binary was linked with one, to
	// form AppName. It does not reach the database connection string, which
	// identifies the service by Name alone.
	Version string

	// Env is the runtime environment, which chooses the log format and
	// verbosity: logger.New writes coloured console output at DEBUG for
	// "development" and JSON at INFO for everything else. Nothing in this
	// module branches on it otherwise.
	//
	// Allowed values: development | staging | production
	Env string

	// Host is the TCP bind address. Empty binds every interface, which is
	// the right default in a container whose network boundary is controlled
	// outside it. "127.0.0.1" accepts local connections only, for a service
	// that should be reachable only through a proxy on the same host.
	//
	// It is overloaded for a unix socket: when fiber.listen.listener_network
	// is "unix", a service is expected to use Host verbatim as the socket
	// PATH, with no ":port". Validate cannot enforce that pairing — it never
	// sees the listen section — so a service combining the two has to check
	// it where both are in hand, or a unix listener reaches net.Listen with
	// an empty path.
	Host string

	// Port is the TCP port to listen on. Must be non-zero.
	//
	// On the unix-socket path the whole address is replaced by Host, so the
	// port is never bound — but Validate still requires one, which means a
	// socket deployment sets a port it will not use.
	Port int

	// Location is an IANA time-zone name with two INDEPENDENT consumers,
	// and only one of them is about the process.
	//
	// First, a service is expected to assign it to time.Local at startup,
	// so log timestamps and every offsetless time.Time it builds land in
	// this zone. That happens once; changing Location without a restart has
	// no effect.
	//
	// Second, and the load-bearing one for stored data: the database
	// package injects it into the connection string — "timezone=" for SQL
	// Server, Loc for MySQL. Without it the SQL Server driver labels every
	// value decoded from DATE, DATETIME, DATETIME2 or TIME as UTC, while a
	// column defaulting to the server's local clock was written in the
	// server's zone. Every such timestamp then comes back adrift by the
	// offset between the two, silently and uniformly — the shape of error
	// that survives review because every row is wrong by the same amount.
	//
	// Validate only checks that it is non-empty. Whether it is a real zone
	// is time.LoadLocation's to decide at startup, which fails on an
	// unknown name ("Berln") rather than falling back to UTC.
	//
	// Example: "UTC", "Europe/Berlin", "America/New_York"
	Location string

	// ReportsBaseURL is the scheme and authority of an external service a
	// deployment proxies requests to — scheme, host and optional port, and
	// nothing after them.
	//
	// It is the most application-specific key in this section: it exists
	// for a service that forwards some routes to a separate renderer, and a
	// service that does not can leave it unset, which Validate accepts.
	//
	// "Nothing after them" is the contract, because the consumer is
	// expected to build the upstream URL by CONCATENATION — base + route —
	// and anything past the authority would be silently prepended to every
	// forwarded request. See Validate for what that costs.
	ReportsBaseURL string

	// UploadDir is the root under which uploaded files are written: one
	// directory per record, <UploadDir>/<id>/<name>, which is the layout the
	// fileutil package implements.
	//
	// Nothing in this package touches the filesystem with it. Storing the
	// file NAME rather than a path is what lets this value change between
	// deployments with no data migration, because the path is recomputed
	// from upload_dir, the id and the name on every access.
	//
	// A relative value is legal and resolves against the process working
	// directory on every call.
	UploadDir string
}

// trimBaseURL normalises a base URL to the form a consumer concatenates
// against: no surrounding whitespace, no trailing slash.
//
// "http://host:5019/" and "http://host:5019" therefore reach Validate and
// String as one string, so a stray slash can neither produce a doubled one
// upstream nor make two identical deployments log different values.
func trimBaseURL(s string) string {
	return strings.TrimRight(strings.TrimSpace(s), "/")
}

// NewAppConfig reads AppConfig fields from the provided Viper instance.
//
// Never returns an error: absent keys and uncastable values both come back as
// zero values, which Validate rejects afterwards.
//
// This function is the authoritative list of keys the app.* section supports.
// A key present in config.yaml but missing here is dead weight — Viper never
// looks it up, so neither the file nor an environment variable can supply it.
func NewAppConfig(v *viper.Viper) *AppConfig {
	return &AppConfig{
		Name:           v.GetString("app.name"),
		Version:        v.GetString("app.version"),
		Env:            v.GetString("app.env"),
		Host:           v.GetString("app.host"),
		Port:           v.GetInt("app.port"),
		Location:       v.GetString("app.location"),
		ReportsBaseURL: trimBaseURL(v.GetString("app.reports_base_url")),
		UploadDir:      v.GetString("app.upload_dir"),
	}
}

// Validate returns a joined error for every invalid or missing AppConfig
// field.
//
// Deliberately unchecked:
//
//   - Whether anything consumes the key. Validate sees a *AppConfig and
//     nothing else, so it cannot tell a wired route from a dead key.
//   - Whether ReportsBaseURL is REACHABLE. Nothing here dials it, so a
//     host that does not resolve, a port nothing listens on and a renderer
//     that is simply down all pass startup, and surface per request in
//     whatever proxies to it. That is the right place: the renderer may
//     restart independently of this service, and a startup probe would
//     turn its downtime into this service's downtime.
//   - Host. Empty is the correct default for a container, so it cannot be
//     required here. The one case where it IS required — a unix-socket
//     listener — needs the listen section as well, and has to be checked
//     where both are in hand.
//   - Location as an IANA name. time.LoadLocation at startup is the
//     authority and fails the process on an unknown zone.
//   - UploadDir. A startup probe is the authority: it has to stat the path
//     and create and delete a file to learn anything, and every check in
//     this method is a pure function of the struct. "Existing" rather than
//     "created on demand" is the property worth probing for — a helper that
//     created every missing level would let a misspelled path start cleanly
//     and write uploads somewhere nobody provisioned.
//
// Port range IS checked, and database.port deliberately reaches the opposite
// conclusion, so the argument is worth keeping. Both have a second authority
// that would catch an out-of-range value; what differs is how LATE each one
// runs. The driver rejects a bad database.port when the pool is opened,
// before a single route is registered, so a duplicate check buys nothing.
// net.Listen is typically the LAST thing a service does, so a 70000 here
// would fail only after the pool was open and every route registered — a
// full startup spent to learn one number is four digits too long.
//
// The check is a chained else-if so an absent key produces one error rather
// than two: 0 is caught as "required" and never also reported as out of range.
// It runs on the unix-socket path too, where the application drops the port
// entirely — a socket deployment has to set a port it will never bind, which
// is the same requirement the zero check already imposed.
//
// Every check appends rather than returning early, so one restart surfaces
// every app.* problem at once.
func (c *AppConfig) Validate() error {
	// errs is declared before the nil check only so the two statements read in
	// the same order in all Validate implementations; the nil check is what
	// must come first, since every line after it dereferences c.
	var errs []error
	if c == nil {
		return errors.New("app config was not initialised")
	}
	if c.Name == "" {
		errs = append(errs, errors.New("app.name is required"))
	}
	if c.Version == "" {
		errs = append(errs, errors.New("app.version is required"))
	}
	if _, ok := validEnvs[c.Env]; !ok {
		errs = append(errs, fmt.Errorf("app.env must be one of: development, "+
			"staging, production (got %q)",
			c.Env))
	}
	if c.Port == 0 {
		errs = append(errs, errors.New("app.port is required"))
	} else if c.Port < 0 || c.Port > 65535 {
		errs = append(errs, fmt.Errorf("app.port must be between 1 and 65535 "+
			"(got %d)",
			c.Port))
	}
	if c.Location == "" {
		errs = append(errs, errors.New("app.location is required"))
	}
	// Presence and SHAPE are both checked, and the shape half is the part that
	// earns its keep. The consumer builds the upstream URL by concatenation —
	// base + "/route/" + name — so anything past the authority is silently
	// prepended to every forwarded request: a value ending in "/api" yields
	// "/api/api/route/name" and a 404 from the upstream naming neither this
	// key nor the doubling. url.Parse accepts all of that happily, which is
	// why the parse result is inspected rather than just its error.
	//
	// USERINFO is rejected on its own terms rather than folded into the
	// path/query/fragment branch below, because it is the one part of a URL
	// that is a credential. It would survive the concatenation into every
	// upstream request, and String prints this field verbatim — so a password
	// written here would reach the log aggregator on every start.
	if c.ReportsBaseURL != "" {
		if u, err := url.Parse(c.ReportsBaseURL); err != nil {
			errs = append(errs, fmt.Errorf("app.reports_base_url is not a valid "+
				"URL (got %q): %w",
				c.ReportsBaseURL,
				err))
		} else if u.Scheme != "http" && u.Scheme != "https" {
			errs = append(errs, fmt.Errorf("app.reports_base_url must start "+
				"with http:// or https:// (got %q)",
				c.ReportsBaseURL))
		} else if u.Host == "" {
			errs = append(errs, fmt.Errorf("app.reports_base_url must name a "+
				"host (got %q)",
				c.ReportsBaseURL))
		} else if u.User != nil {
			// The offending value is NOT echoed, unlike every other branch here.
			// A service typically hands a validation error straight to a fatal
			// log call, so reporting the value would copy the credential into
			// exactly the place this check exists to keep it out of. Only the
			// corrected form is printed, and it drops the userinfo by
			// construction.
			errs = append(errs, fmt.Errorf("app.reports_base_url must not "+
				"carry userinfo: the value is concatenated into every upstream "+
				"URL and is printed verbatim when the configuration is logged, "+
				"so a credential here reaches both — use %q and authenticate "+
				"to the upstream some other way",
				u.Scheme+"://"+u.Host))
		} else if u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
			errs = append(errs, fmt.Errorf("app.reports_base_url must be scheme "+
				"+ host (+ port) only, with no path, query, or fragment (got "+
				"%q): the consumer appends its own route, so anything here is "+
				"prepended to every upstream URL — use %q",
				c.ReportsBaseURL,
				u.Scheme+"://"+u.Host))
		}
	}

	return errors.Join(errs...)
}

// String returns a loggable representation of AppConfig.
//
// The pointer receiver means fmt only picks this up for a *AppConfig. Printing
// a value copy (%v on AppConfig, not &AppConfig) bypasses it and dumps the
// struct fields directly.
func (c *AppConfig) String() string {
	if c == nil {
		return "<nil AppConfig>"
	}
	return fmt.Sprintf("Name=%s "+
		"Version=%s "+
		"Env=%s "+
		"Host=%s "+
		"Port=%d "+
		"Location=%s "+
		"ReportsBaseURL=%s "+
		"UploadDir=%s",
		c.Name,
		c.Version,
		c.Env,
		c.Host,
		c.Port,
		c.Location,
		c.ReportsBaseURL,
		c.UploadDir,
	)
}
