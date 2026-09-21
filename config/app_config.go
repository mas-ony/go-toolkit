// app_config.go covers the app.* section of config.yaml.
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
// Seven keys, and that is the whole section — NewAppConfig below reads exactly
// these. See doc.go for how the environment spelling is derived and which
// tests hold it up.

package config

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
	// Name is the human-readable service name. NewFiberConfig joins it with
	// Version into fiber.Config.AppName, which router.New passes through to
	// fiber.New; and pkg/database embeds it in the MSSQL connection string as
	// "app name=" so the connection is identifiable in SQL Server's Activity
	// Monitor (sys.dm_exec_sessions.program_name).
	//
	// Fiber's use of it reaches nobody wherever the startup banner is off.
	// AppName shows in that banner, and
	// fiber.listen.disable_startup_message suppresses it — the setting a
	// JSON log pipeline wants. What does carry the assembled string is
	// The "configuration loaded" startup line: it walks the section list and
	// logs FiberConfig.String, which leads with AppName=, at Info on every
	// start — in JSON, in exactly the deployments where the banner is
	// suppressed.
	//
	// That line is a record of what was loaded, though, not a handle on a
	// running process. The SQL Server session is the handle, which is what
	// makes the "app name=" use the one that actually matters.
	//
	// The value is unconstrained while the DSN is built in the URL form,
	// which escapes a semicolon to %3B through url.Values and reads it back
	// intact — any byte is safe. The ADO form ("key=value;...") would not be,
	// since ";" is its field separator, so a switch back to it makes this a
	// constrained string again.
	//
	// It is NOT part of the Server response header. Fiber sets that header
	// from fiber.Config.ServerHeader verbatim — a separate value carried from
	// fiber.server_header — and omits the header entirely when it is empty.
	// AppName plays no part in it.
	Name string

	// Version is the semantic version string. NewFiberConfig concatenates it
	// with Name and, when the binary was linked with -ldflags, a buildTime
	// suffix — "myapp 1.0.20260805120000" — to form AppName.
	//
	// Where that lands is the same story as Name above, and worth not reading
	// twice: AppName's Fiber destination is the startup banner, which
	// fiber.listen.disable_startup_message suppresses, so where the
	// assembled string is actually read is the application's
	// "configuration loaded" line, which logs FiberConfig.String.
	//
	// It does not reach the DSN: pkg/database uses Name alone for "app name=",
	// so the SQL Server session identifies the service without a version.
	Version string

	// Env is the runtime environment. Controls log format and verbosity.
	//
	// logger.New picks the console writer, DEBUG level, and caller annotation
	// for "development" and JSON at INFO for everything else. Nothing else
	// branches on Env — there is no environment-gated route, middleware, or
	// database behaviour.
	//
	// Allowed values: development | staging | production
	Env string

	// Host is the TCP bind address. Empty string binds to all interfaces
	// (0.0.0.0), which is the correct default for container deployments where
	// the network boundary is controlled externally. Set to "127.0.0.1" to
	// accept local connections only (e.g. when a reverse proxy runs on the
	// same host and the app should not be reachable directly).
	//
	// Overloaded for unix sockets: when fiber.listen.listener_network is
	// "unix", the application uses Host verbatim as the listen address, with
	// no ":port" suffix, so it must be the socket PATH (e.g.
	// "/run/myapp.sock"). Validate does not require Host — empty is the
	// correct container default — and AppConfig.Validate structurally cannot
	// catch the socket case anyway, since it never sees ListenConfig.
	// Cross-section validation holds both sections and rejects
	// (listener_network == "unix" && host == "") there, which is what keeps
	// this pairing from reaching net.Listen as an empty path.
	Host string

	// Port is the TCP port the HTTP server listens on. Must be non-zero.
	//
	// Ignored on the unix-socket path, where the application replaces the
	// whole "host:port" address with Host. Validate still demands a non-zero
	// value there, so a socket deployment has to set a port it will never
	// bind.
	Port int

	// Location is an IANA time-zone name with two INDEPENDENT consumers. Both
	// matter, and only one of them is about the process.
	//
	// First: the application assigns it to time.Local, so log timestamps and
	// every offsetless time.Time the process constructs land in this zone.
	// That assignment happens once at startup and Config is never re-read, so
	// changing Location without restarting has no effect.
	//
	// Second, and the load-bearing one for stored data: the DSN builder
	// injects it into the connection string — "timezone=" on sqlserver, Loc
	// on mysql. Without it the sqlserver driver labels every value it decodes
	// from DATE / DATETIME / DATETIME2 / TIME as UTC, while a column whose
	// default is the server's local wall clock was written in the server's
	// zone. Every such timestamp then comes back adrift by the offset between
	// the two, silently and uniformly, which is the shape of error that
	// survives review because every row is wrong by the same amount.
	//
	// Validate only checks that this field is non-empty. Whether the value is
	// a valid IANA name is determined by time.LoadLocation at startup, which
	// returns an error for unrecognised zones (e.g. "Berln" instead of
	// "Europe/Berlin"). A bad zone name therefore causes a startup failure,
	// not a silent fallback to UTC. The sqlserver driver parses the value
	// again when it builds the DSN, so a host without tzdata fails there too.
	//
	// Example: "UTC", "Europe/Berlin", "America/New_York"
	Location string

	// ReportsBaseURL is the scheme + host (+ port) of the external
	// report-rendering service that /api/reports/:report proxies to. Scheme
	// and authority only — handler.ReportsHandler appends
	// reportsUpstreamPrefix ("/api/reports/") and the report name itself, and
	// trims a trailing slash from this value on the way in so the join cannot
	// double up.
	ReportsBaseURL string

	// UploadDir is the root under which uploaded files are written, one
	// directory per record: <UploadDir>/<id>/<sanitised file name>. The
	// stored name and the on-disk name can differ when the sanitiser rewrites
	// a character, and reapplying it on every read is what keeps the mapping
	// stable without the sanitised form ever being persisted.
	//
	// Nothing in this package touches the filesystem with it; it is passed to
	// whichever helper the application writes files through. Storing the file
	// NAME rather than a path is what lets this value change between
	// deployments with no data migration — the path is recomputed from
	// upload_dir, the id, and the name on every access.
	//
	// A relative value is legal and resolves against the process working
	// directory on every call. A startup probe may report it in absolute form
	// without writing that form back here, so the two spellings stay
	// interchangeable and New returns configuration exactly as parsed.
	UploadDir string
}

// trimBaseURL normalises a base URL to the form the reports proxy concatenates
// against: no surrounding whitespace, no trailing slash.
//
// Both variants — "http://host:5019/" and "http://host:5019" — therefore reach
// Validate, String, and handler.NewReportsHandler as one string, so a stray
// slash cannot produce a doubled one upstream and cannot make two identical
// deployments log different values. handler.NewReportsHandler trims again on
// its own copy; that is belt-and-braces, not a second authority.
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
//   - Whether ReportsBaseURL is REACHABLE. Nothing here dials it, so a host
//     that does not resolve, a port nothing listens on, and a report service
//     that is simply down all pass startup and fail per-request in
//     ReportsHandler.forward, which maps them to a 502 naming the upstream.
//     That is the right place for it: the report service is allowed to restart
//     independently of this one, and a startup probe would turn its downtime
//     into this service's downtime.
//   - Host. Empty is the correct default for a container (bind all
//     interfaces), so it cannot be required here. The one case where it IS
//     required — a unix-socket listener — is enforced by
//     Cross-section validation, which can see both sections.
//   - Location as an IANA name. time.LoadLocation at startup is the
//     authority and fails the process on an unknown zone.
//   - UploadDir beyond non-emptiness. A startup probe is the authority: it
//     requires an EXISTING writable directory and fails otherwise — the same
//     division as Location, where time.LoadLocation holds the real check. It
//     belongs there rather than here because it has to stat a path and CREATE
//     AND DELETE A FILE to learn anything. Every check in this method is a
//     pure function of the struct, which is what lets the whole of it be
//     exercised without a filesystem.
//     "Existing" rather than "created on demand" is the load-bearing half. A
//     helper that walks the chain and creates every missing level would let a
//     misspelled path start cleanly and write uploads into a directory nobody
//     provisioned. The probe is what turns that typo into a startup error.
//
// Port range IS checked, and the argument for it is worth keeping because
// database.port deliberately reaches the opposite conclusion. Both keys have a
// second authority that would catch an out-of-range value on its own; what
// differs is HOW LATE each one runs. The driver rejects a bad database.port
// when the pool is opened, before a single route is registered, so the
// duplicate check would buy nothing. net.Listen is the LAST thing to run, so
// a 70000 here failed only after the pool was open, every route registered,
// and the startup line written — a full startup spent to learn one number is
// four digits too long. See the matching entry in DatabaseConfig.Validate.
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
	// earns its keep. handler.ReportsHandler builds the upstream URL by
	// concatenation — baseURL + "/api/reports/" + name — so anything past the
	// authority is silently prepended to every report request: a value ending
	// in "/api" yields "/api/api/reports/disposisi" and a 404 from the report
	// service naming neither this key nor the doubling. url.Parse accepts all
	// of that happily, which is why the parse result is inspected rather than
	// just its error.
	//
	// USERINFO is rejected on its own terms rather than folded into the
	// path/query/fragment branch below, because it is the one part of a URL
	// that is a credential. It survives the same concatenation into every
	// upstream request, and String below prints this field verbatim into
	// main.Setup's "configuration loaded" line — so a password written here
	// reaches the log aggregator on every start, by the same route the
	// caution on DatabaseConfig.Username describes and that
	// DatabaseConfig.String exists to keep closed.
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
			// Reporting it would copy the credential into an error that
			// main.Setup hands to log.Fatal().Err(), which is the leak this
			// check exists to prevent. Only the corrected form is printed, and
			// that form drops the userinfo by construction.
			errs = append(errs, fmt.Errorf("app.reports_base_url must not "+
				"carry userinfo: the value is concatenated into every upstream "+
				"report URL and is printed verbatim in the startup log, so a "+
				"credential here reaches both — use %q and authenticate to the "+
				"report service some other way",
				u.Scheme+"://"+u.Host))
		} else if u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
			errs = append(errs, fmt.Errorf("app.reports_base_url must be scheme "+
				"+ host (+ port) only, with no path, query, or fragment (got "+
				"%q): the reports proxy appends \"/api/reports/<name>\" itself, "+
				"so anything here is prepended to every report URL — use %q",
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
		"ReportsBaseURL=%s"+
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
