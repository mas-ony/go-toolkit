// Package config loads the service's configuration: config.yaml, with
// environment variables overriding it key by key, read into the typed
// sections of github.com/mas-ony/go-toolkit/config.
//
// New validates every section this deployment uses, then the rules no
// single section can check, and reports every problem in one error, so
// one restart lists everything to fix. It logs nothing, because the
// logger's format depends on app.env: build the logger from the result,
// then call Log.
//
//	cfg, err := config.New("config.yaml")
//	if err != nil {
//		fmt.Fprintln(os.Stderr, err)
//		os.Exit(1)
//	}
//	log := logger.New(cfg.App.Env)
//	cfg.Log(log)
//
// Whether a section is validated depends on what switches it:
//
//   - fiber.jwt and fiber.session are switched by fiber.auth.mode, and
//     notification.whatsapp and notification.email by
//     notification.channels. A switched-on section is validated even
//     when nothing supplied it, so its missing keys are named at
//     startup; a switched-off one is skipped even when supplied. An
//     absent fiber.auth.mode switches both auth sections off, as an
//     absent notification.channels switches off both channels.
//   - database, notification, fiber.auth and fiber.client are optional:
//     each is validated once any of its keys is supplied, by the file or
//     the environment.
//   - Every other section is always validated.
//
// fiber.body_limit is checked against fileutil.MaxFileBytes, so a
// service that changes that limit assigns it before calling New.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"slices"
	"strconv"
	"strings"

	fiberzerolog "github.com/gofiber/contrib/v3/zerolog"
	"github.com/gofiber/fiber/v3"
	"github.com/rs/zerolog"

	// The toolkit package shares this package's name. Go never
	// qualifies a package's own identifiers, so in this file config.X
	// always means the toolkit's X.
	"github.com/mas-ony/go-toolkit/config"
	"github.com/mas-ony/go-toolkit/fileutil"
)

// inactive stands in for a section that was supplied but is switched
// off by another key. Like config.AbsentSection it validates clean;
// unlike it, its log line says why the supplied values are not in use.
// The string is that reason.
type inactive string

// Config holds every section of the service's configuration, plus what
// Log needs to report where it came from. Build one with New.
//
// Every section field is non-nil, whether or not this deployment uses
// the section. One it does not use still holds whatever its keys read
// as: zero values when nothing supplied them, which email.New answers
// with email.ErrNotConfigured and httpclient.New refuses by name, or
// the supplied values of a section that is switched off. Check the
// switch (Auth.IsSession, Auth.IsJWT or Notification.Enabled) before
// relying on a switched section. Only validation and the startup log
// treat an unused section differently; see plan.
type Config struct {
	// App is app.*: service identity, runtime environment, bind
	// address and time zone.
	App *config.AppConfig

	// Database is database.*: driver, server, credentials, table
	// naming and connection pool. Optional: a service without a
	// database leaves the section out, and Database.Configured then
	// reports false. Open the pool only when it reports true, since
	// database.New answers an absent section with
	// database.ErrNotConfigured.
	Database *config.DatabaseConfig

	// Notification is notification.*: which channels send. Optional.
	Notification *config.NotificationConfig

	// WhatsApp is notification.whatsapp.*: how WhatsApp messages are
	// sent. In use only while notification.channels lists whatsapp.
	WhatsApp *config.WhatsAppConfig

	// Email is notification.email.*: the SMTP relay mail is handed to.
	// In use only while notification.channels lists email.
	Email *config.EmailConfig

	// Fiber is fiber.*: the HTTP server's own settings.
	Fiber *config.FiberConfig

	// Auth is fiber.auth.*: the authentication mode, which decides
	// whether Session, JWT or neither is in use. Optional: an absent
	// mode selects no authentication, so branch on Auth.IsSession and
	// Auth.IsJWT both, never on one of them with an else.
	Auth *config.AuthConfig

	// JWT is fiber.jwt.*: token signing secret and lifetime. In use
	// only while fiber.auth.mode is jwt.
	JWT *config.JWTConfig

	// Session is fiber.session.*: session cookie and timeouts. In use
	// only while fiber.auth.mode is session.
	Session *config.SessionConfig

	// Client is fiber.client.*: the outbound HTTP client. Optional.
	Client *config.ClientConfig

	// Limiter is fiber.limiter.*: the per-client rate limit.
	Limiter *config.LimiterConfig

	// Listen is fiber.listen.*: listener network, TLS, graceful
	// shutdown and prefork.
	Listen *config.ListenConfig

	// Recover is fiber.recover.*: recovery from panics in handlers.
	Recover *config.RecoverConfig

	// RequestID is fiber.requestid.*: the correlation id header.
	RequestID *config.RequestIDConfig

	// Zerolog is fiber.zerolog.*: the one-line-per-request access log.
	Zerolog *config.ZerologConfig

	// file is the path New was given, or "" when it was given none.
	file string

	// missing reports that file did not exist, so New read the
	// environment alone.
	missing bool

	// sections is the plan New validated: sectionList with every
	// section not in use replaced by a stand-in. Log walks the same
	// list, so the startup log shows exactly what was validated.
	sections []config.Section
}

// multipartAllowance is the room fiber.body_limit has to leave above
// fileutil.MaxFileBytes for the multipart envelope around an upload:
// the boundaries, the part headers carrying the filename, and any other
// form fields sent with the file. It is 1 MiB.
const multipartAllowance = 1 << 20

// optional names the sections a deployment may leave out entirely,
// keyed by the names sectionList gives them. Each is validated only
// once some source supplies at least one of its keys; see plan.
//
// Leaving out notification or auth is itself a setting rather than a
// gap: no channel sends, and no authentication is selected. Both are
// switches, so what they leave out is decided in inUse. Leaving out
// database or client means the service uses neither: Database.Configured
// reports false, and httpclient.New refuses an empty base URL.
//
// Every other section that no key switches (see inUse) is validated
// even when nothing supplied it, so a missing key is reported by name
// instead of running as a zero value.
var optional = map[string]bool{
	"database":     true,
	"notification": true,
	"auth":         true,
	"client":       true,
}

// sectionList pairs every section with the name it is logged under and
// the key prefix its keys are nested behind, in the order the startup
// log prints them.
//
// Both strings carry weight. The name is what optional and inUse key
// on. The prefix is how config.SuppliedSections routes a key from the
// file, or an environment variable, back to its section, so a prefix
// that does not match the section's keys makes a supplied section look
// absent.
//
// A section of the application's own belongs here too: anything with
// Validate and String is validated and logged with the rest.
func (c *Config) sectionList() []config.Section {
	return []config.Section{
		{Name: "app", Prefix: "app", Value: c.App},
		{Name: "database", Prefix: "database", Value: c.Database},
		{Name: "notification", Prefix: "notification",
			Value: c.Notification},
		{Name: "whatsapp", Prefix: "notification.whatsapp",
			Value: c.WhatsApp},
		{Name: "email", Prefix: "notification.email", Value: c.Email},
		{Name: "fiber", Prefix: "fiber", Value: c.Fiber},
		{Name: "auth", Prefix: "fiber.auth", Value: c.Auth},
		{Name: "jwt", Prefix: "fiber.jwt", Value: c.JWT},
		{Name: "session", Prefix: "fiber.session", Value: c.Session},
		{Name: "client", Prefix: "fiber.client", Value: c.Client},
		{Name: "limiter", Prefix: "fiber.limiter", Value: c.Limiter},
		{Name: "listen", Prefix: "fiber.listen", Value: c.Listen},
		{Name: "recover", Prefix: "fiber.recover", Value: c.Recover},
		{Name: "requestid", Prefix: "fiber.requestid",
			Value: c.RequestID},
		{Name: "zerolog", Prefix: "fiber.zerolog", Value: c.Zerolog},
	}
}

// inUse reports whether the key that switches the named section selects
// it and, when it does not, the reason for the startup log. A section no
// key switches is always in use.
//
// Each auth section is in use only under its own mode, the branches the
// service takes when it wires its middleware (config.AuthConfig's
// IsSession and IsJWT). An absent mode selects neither, and so does an
// unrecognised one: the auth section reports that itself, and its error
// is the one to fix before either mechanism can be judged.
func (c *Config) inUse(name string) (bool, string) {
	mode := "fiber.auth.mode is " + c.Auth.Mode
	if c.Auth.Mode == "" {
		mode = "fiber.auth.mode is not set"
	}
	switch name {
	case "jwt":
		return c.Auth.IsJWT(), mode
	case "session":
		return c.Auth.IsSession(), mode
	case "whatsapp":
		return c.Notification.Enabled(config.ChannelWhatsApp),
			"whatsapp is not in notification.channels"
	case "email":
		return c.Notification.Enabled(config.ChannelEmail),
			"email is not in notification.channels"
	}
	return true, ""
}

// plan returns sectionList with every section this deployment does not
// use replaced by a stand-in that validates clean and logs what it is.
// supplied comes from config.SuppliedSections, which counts a key in
// the file and a non-empty environment variable alike.
//
// Each section meets exactly one of these cases, tested in this order:
//
//   - Switched off and supplied: replaced by inactive, whose log line
//     names the key that switched it off. A secret the section would
//     demand, such as fiber.jwt.secret in session mode, is then not
//     demanded for values nothing reads.
//   - Switched off and not supplied: replaced by config.AbsentSection.
//   - Optional and not supplied: replaced by config.AbsentSection too.
//   - Anything else keeps its real value and is validated even when
//     nothing supplied it, so its missing keys are named at startup
//     instead of running as zero values. That includes a switched-on
//     section nobody wrote, such as fiber.jwt once the mode is jwt.
func (c *Config) plan(supplied config.Supplied) []config.Section {
	list := c.sectionList()
	for i, s := range list {
		on, why := c.inUse(s.Name)
		switch {
		case !on && supplied.Configured(s.Name):
			// Switched off, yet supplied: say why it is unused.
			list[i].Value = inactive(why)
		case !on, optional[s.Name] && !supplied.Configured(s.Name):
			// Switched off with nothing supplied, or optional and
			// left out: nothing to validate, nothing to show.
			list[i].Value = config.AbsentSection{}
		}
	}
	return list
}

// validateCrossSection holds the rules the toolkit leaves to the
// application because no section's Validate can check them: each needs
// keys from two sections or, for fiber.body_limit, a key and a limit
// set in code (fileutil.MaxFileBytes). Every rule appends rather than
// returning early, so all of them report in one pass.
func (c *Config) validateCrossSection() []error {
	var errs []error

	// fiber.body_limit against fileutil.MaxFileBytes. For a request
	// larger than the body limit, Fiber answers 413 before any handler
	// runs, so the limit has to cover the largest file fileutil accepts
	// plus the multipart envelope around it. A cap of 0 or less means
	// fileutil accepts any size, and the floor is then the envelope
	// alone.
	//
	// Fiber replaces a limit of 0 or less, which is what an absent key
	// reads as, with its own fiber.DefaultBodyLimit, and the floor is
	// held against that replacement, since it is the limit Fiber
	// enforces. Under the default file cap the replacement is far too
	// small, so an absent key is still refused there.
	minBody := multipartAllowance
	if fileutil.MaxFileBytes > 0 {
		minBody += fileutil.MaxFileBytes
	}
	limit, got := c.Fiber.BodyLimit, strconv.Itoa(c.Fiber.BodyLimit)
	if limit <= 0 {
		limit = fiber.DefaultBodyLimit
		got += fmt.Sprintf(", which Fiber replaces with its own %d",
			limit)
	}
	if limit < minBody {
		errs = append(errs, fmt.Errorf("fiber.body_limit must be "+
			"at least %d (got %s): one upload request carries a "+
			"file of up to fileutil.MaxFileBytes (%d) plus %d "+
			"bytes of multipart envelope",
			minBody, got, fileutil.MaxFileBytes, multipartAllowance))
	}

	// app.host against fiber.listen.listener_network. On a unix socket
	// app.host is the socket path (see Addr), and an empty one would
	// fail inside net.Listen with a message naming neither key.
	// Whitespace alone counts as empty.
	if c.Listen.ListenerNetwork == fiber.NetworkUnix &&
		strings.TrimSpace(c.App.Host) == "" {
		errs = append(errs, errors.New("app.host is required when "+
			"fiber.listen.listener_network is unix: it is the "+
			"socket path, and an empty one fails inside "+
			"net.Listen"))
	}

	// fiber.requestid.header against fiber.zerolog.fields. The access
	// log's requestId field reads the id from the X-Request-ID
	// response header, a name fixed in gofiber/contrib/v3/zerolog, so
	// renaming the header while that field is logged empties it on
	// every line.
	//
	// The comparison is exact, casing included. A re-cased name works
	// only while fasthttp normalises header names, which is a key of
	// its own (fiber.disable_header_normalizing), so the rule does not
	// lean on it. An empty header is left to the requestid section,
	// which reports it as required.
	header := c.RequestID.Header
	logsID := slices.Contains(c.Zerolog.Fields,
		fiberzerolog.FieldRequestID)
	if logsID && header != "" &&
		header != config.DefaultRequestIDHeader {
		errs = append(errs, fmt.Errorf("fiber.requestid.header "+
			"must be %q while fiber.zerolog.fields includes %s "+
			"(got %q): the access log reads the id from the %s "+
			"header only, so any other spelling can log it empty "+
			"on every request", config.DefaultRequestIDHeader,
			fiberzerolog.FieldRequestID, header,
			config.DefaultRequestIDHeader))
	}

	// fiber.listen.enable_prefork against fiber.auth.mode. Each prefork
	// worker is a process with its own in-memory session store, so under
	// session auth a request that lands on another worker arrives logged
	// out. JWT and no authentication keep no login state in the process,
	// so neither is refused. Drop this rule if the application installs
	// a shared session Storage.
	//
	// The rate limiter's counters and the database pool are per worker
	// too, but prefork multiplies those limits rather than breaking
	// requests, so config.yaml documents them and nothing refuses them.
	if c.Auth.IsSession() && c.Listen.EnablePrefork {
		errs = append(errs, errors.New("fiber.listen.enable_prefork "+
			"cannot be true while fiber.auth.mode is session: "+
			"every worker keeps its own in-memory session store, "+
			"so a request that lands on another worker arrives "+
			"logged out"))
	}

	// fiber.zerolog.levels against app.env. The rule assumes the access
	// log writes through the logger.New logger, handed to the
	// middleware with ZerologConfig.WithLogger. That logger drops
	// everything below debug in development and below info elsewhere,
	// so a response class logged under that floor is never written.
	// The floor mirrors logger.New and has to move with it. Absent
	// levels mean the middleware's own error, warn and info, which
	// always pass.
	minLevel := zerolog.InfoLevel
	if c.App.Env == config.EnvDevelopment {
		minLevel = zerolog.DebugLevel
	}
	for _, level := range c.Zerolog.Levels {
		if level >= minLevel {
			continue
		}
		errs = append(errs, fmt.Errorf("fiber.zerolog.levels "+
			"includes %s, but the logger for app.env %q drops "+
			"everything below %s, so that response class is "+
			"never logged", level, c.App.Env, minLevel))
	}

	return errs
}

// validate runs every planned section's Validate, then
// validateCrossSection, and joins the results so one restart reports
// every problem. It returns nil when there is none.
//
// The cross-section rules run even when a section they read has failed
// its own checks, so one bad value can produce two messages. Each names
// its keys, so both point at the same fix.
func (c *Config) validate() error {
	var errs []error
	for _, s := range c.sections {
		if err := s.Value.Validate(); err != nil {
			errs = append(errs, err)
		}
	}
	errs = append(errs, c.validateCrossSection()...)
	return errors.Join(errs...)
}

// New reads the YAML file at path, lets the environment override it key
// by key, and returns the configuration only when every section in use,
// and every rule validateCrossSection checks, is valid. Otherwise it
// returns no Config and one error joining every problem found.
//
// The file is parsed as YAML whatever its extension. A missing file is
// tolerated, so a deployment can run from the environment alone; if the
// environment then falls short, the error says the file was not found,
// or the list of missing keys would read as a broken file. A file that
// exists but cannot be read or parsed is refused outright. An empty
// path skips the file and reads the environment alone.
func New(path string) (*Config, error) {
	v := config.NewViper()
	missing := false
	if path != "" {
		v.SetConfigFile(path)
		v.SetConfigType("yaml")
		err := v.ReadInConfig()
		// Viper returns the file system's own error, so a missing
		// file is told apart from one that is unreadable or malformed.
		missing = errors.Is(err, fs.ErrNotExist)
		if err != nil && !missing {
			return nil, fmt.Errorf("reading %s: %w", path, err)
		}
	}

	// Every section is read, in use or not: which ones are in use is
	// only known once fiber.auth and notification have been read.
	c := &Config{
		App:          config.NewAppConfig(v),
		Database:     config.NewDatabaseConfig(v),
		Notification: config.NewNotificationConfig(v),
		WhatsApp:     config.NewWhatsAppConfig(v),
		Email:        config.NewEmailConfig(v),
		Fiber:        config.NewFiberConfig(v),
		Auth:         config.NewAuthConfig(v),
		JWT:          config.NewJWTConfig(v),
		Session:      config.NewSessionConfig(v),
		Client:       config.NewClientConfig(v),
		Limiter:      config.NewLimiterConfig(v),
		Listen:       config.NewListenConfig(v),
		Recover:      config.NewRecoverConfig(v),
		RequestID:    config.NewRequestIDConfig(v),
		Zerolog:      config.NewZerologConfig(v),
		file:         path,
		missing:      missing,
	}
	c.sections = c.plan(config.SuppliedSections(v, c.sectionList()))

	if err := c.validate(); err != nil {
		if missing {
			return nil, fmt.Errorf("%s not found, and the "+
				"environment alone is not a complete "+
				"configuration:\n%w", path, err)
		}
		return nil, err
	}
	return c, nil
}

// Addr is the address to hand to Fiber's Listen. On a unix socket
// (fiber.listen.listener_network unix) it is app.host verbatim: the
// socket path, with no port. Otherwise it is app.host and app.port
// joined as host:port. An empty host listens on every interface, and an
// IPv6 address is written without brackets, since JoinHostPort adds
// them.
func (c *Config) Addr() string {
	if c.Listen.ListenerNetwork == fiber.NetworkUnix {
		return c.App.Host
	}
	return net.JoinHostPort(c.App.Host, strconv.Itoa(c.App.Port))
}

// Log writes the startup record of what New loaded, all at info level:
// one "configuration source" line naming the file, then one
// "configuration loaded" line per section, in sectionList order.
//
// Each section line is the section's String, which masks passwords,
// tokens and secrets. A section this deployment does not use shows as
// <not configured> or <inactive: reason> rather than as zero values
// nobody chose.
//
// The source line keeps a missing file's path, so a mistyped path
// shows in the log even when the environment alone was enough to start.
func (c *Config) Log(log zerolog.Logger) {
	source := c.file
	switch {
	case c.file == "":
		source = "(none: environment only)"
	case c.missing:
		source = c.file + " (not found: environment only)"
	}
	log.Info().Str("file", source).Msg("configuration source")
	for _, s := range c.sections {
		log.Info().
			Str("section", s.Name).
			Str("config", s.Value.String()).
			Msg("configuration loaded")
	}
}

// String renders the stand-in for the startup log, for example
// "<inactive: fiber.auth.mode is session>".
func (s inactive) String() string { return "<inactive: " + string(s) + ">" }

// Validate reports nothing: none of a switched-off section's values is
// in use, so none of them can be wrong.
func (inactive) Validate() error { return nil }
