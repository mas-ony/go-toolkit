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
//
// Six keys, and that is the whole section: NewAppConfig reads exactly
// these. The environment spelling holds only for a Viper built by NewViper;
// see the package documentation.

import (
	"errors"
	"fmt"

	"github.com/spf13/viper"
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
	// which escapes a semicolon to %3B and reads it back intact. The
	// ADO-style "key=value;..." form would not be, since ";" is its field
	// separator.
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
}

// Environment name constants used by app.env and the logger.
// Compare against these instead of bare string literals to avoid typos.
const (
	// Pretty-printed, colorized logs; DEBUG level.
	EnvDevelopment = "development"

	// JSON logs; INFO level.
	EnvStaging = "staging"

	// JSON logs; INFO level — the same logger behaviour as staging. A
	// separate value so a deployment can say which one it is; nothing in
	// this module treats the two differently.
	EnvProduction = "production"
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
		Name:     v.GetString("app.name"),
		Version:  v.GetString("app.version"),
		Env:      v.GetString("app.env"),
		Host:     v.GetString("app.host"),
		Port:     v.GetInt("app.port"),
		Location: v.GetString("app.location"),
	}
}

// Validate returns a joined error for every invalid or missing AppConfig
// field.
//
// Deliberately unchecked:
//
//   - Whether anything consumes the key. Validate sees a *AppConfig and
//     nothing else, so it cannot tell a wired route from a dead key.
//   - Host. Empty is the correct default for a container, so it cannot be
//     required here. The one case where it IS required — a unix-socket
//     listener — needs the listen section as well, and has to be checked
//     where both are in hand.
//   - Location as an IANA name. time.LoadLocation at startup is the
//     authority and fails the process on an unknown zone.
//
// Port range IS checked, and database.port deliberately reaches the opposite
// conclusion, so the argument is worth keeping. Both have a second authority
// that would catch an out-of-range value; what differs is how LATE each one
// runs. database.New rejects a bad database.port, naming the key, when the
// pool is opened, before a single route is registered, so a duplicate check
// buys nothing. net.Listen is typically the LAST thing a service does, so a
// 70000 here would fail only after the pool was open and every route
// registered: a full startup spent to learn that one number was out of
// range.
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
		"Location=%s",
		c.Name,
		c.Version,
		c.Env,
		c.Host,
		c.Port,
		c.Location,
	)
}
