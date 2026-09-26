// Package config loads the service configuration: config.yaml, with
// environment variables overriding it key by key, read into the typed
// sections of github.com/mas-ony/go-toolkit/config.
//
// New validates every section the deployment uses, then the pairings no
// single section can see, and reports every problem at once. It logs
// nothing, because the logger's format depends on app.env: build the
// logger from the result, then call Log.
//
//	cfg, err := config.New("config.yaml")
//	if err != nil {
//		fmt.Fprintln(os.Stderr, err)
//		os.Exit(1)
//	}
//	log := logger.New(cfg.App.Env)
//	cfg.Log(log)
//
// fiber.body_limit is checked against fileutil.MaxFileBytes, so a service
// that changes that limit assigns it before calling New.
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
	"github.com/rs/zerolog"

	kit "github.com/mas-ony/go-toolkit/config"
	"github.com/mas-ony/go-toolkit/fileutil"
)

// multipartAllowance is the room fiber.body_limit has to leave above
// fileutil.MaxFileBytes for the multipart envelope around an upload: the
// boundaries, the part headers carrying the filename, and any other form
// fields sent with the file.
const multipartAllowance = 1 << 20

// optional names the sections a deployment may leave out entirely. Every
// section not named here, and not switched by another key (see plan), is
// validated even when nothing supplied it, so a missing key is reported
// by name instead of running as a zero value.
var optional = map[string]bool{
	"notification": true,
	"client":       true,
}

// Config holds every section of the toolkit's configuration.
//
// Every field is non-nil. A section this deployment does not use still
// holds what its keys read as, usually zero values, which email.New
// answers with email.ErrNotConfigured and httpclient.New refuses by name.
type Config struct {
	App          *kit.AppConfig
	Database     *kit.DatabaseConfig
	Notification *kit.NotificationConfig
	WhatsApp     *kit.WhatsAppConfig
	Email        *kit.EmailConfig
	Fiber        *kit.FiberConfig
	Auth         *kit.AuthConfig
	JWT          *kit.JWTConfig
	Session      *kit.SessionConfig
	Client       *kit.ClientConfig
	Limiter      *kit.LimiterConfig
	Listen       *kit.ListenConfig
	Recover      *kit.RecoverConfig
	RequestID    *kit.RequestIDConfig
	Zerolog      *kit.ZerologConfig

	file     string
	sections []kit.Section
}

// New reads the YAML file at path, lets the environment override it, and
// returns the configuration only when every section in use, and every
// pairing validateCrossSection checks, is valid.
//
// A missing file is tolerated, so a deployment can run from the
// environment alone; a file that does not parse is not. An empty path
// reads the environment alone.
func New(path string) (*Config, error) {
	v := kit.NewViper()
	missing := false
	if path != "" {
		v.SetConfigFile(path)
		v.SetConfigType("yaml")
		err := v.ReadInConfig()
		missing = errors.Is(err, fs.ErrNotExist)
		if err != nil && !missing {
			return nil, fmt.Errorf("reading %s: %w", path, err)
		}
	}

	c := &Config{
		App:          kit.NewAppConfig(v),
		Database:     kit.NewDatabaseConfig(v),
		Notification: kit.NewNotificationConfig(v),
		WhatsApp:     kit.NewWhatsAppConfig(v),
		Email:        kit.NewEmailConfig(v),
		Fiber:        kit.NewFiberConfig(v),
		Auth:         kit.NewAuthConfig(v),
		JWT:          kit.NewJWTConfig(v),
		Session:      kit.NewSessionConfig(v),
		Client:       kit.NewClientConfig(v),
		Limiter:      kit.NewLimiterConfig(v),
		Listen:       kit.NewListenConfig(v),
		Recover:      kit.NewRecoverConfig(v),
		RequestID:    kit.NewRequestIDConfig(v),
		Zerolog:      kit.NewZerologConfig(v),
	}
	if path != "" && !missing {
		c.file = path
	}
	c.sections = c.plan(kit.SuppliedSections(v, c.sectionList()))

	if err := c.validate(); err != nil {
		if missing {
			return nil, fmt.Errorf("%s not found, and the environment "+
				"alone is not a complete configuration:\n%w", path, err)
		}
		return nil, err
	}
	return c, nil
}

// sectionList pairs every section with the key prefix it is nested
// behind. A section of the application's own belongs here too: anything
// with Validate and String is validated and logged with the rest.
func (c *Config) sectionList() []kit.Section {
	return []kit.Section{
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

// plan returns the section list with every section this deployment does
// not use swapped for a stand-in that validates clean and logs as such.
//
// Four sections are switched by another key: fiber.jwt and fiber.session
// by fiber.auth.mode, and notification.whatsapp and notification.email by
// notification.channels. One that is switched on is validated even when
// nothing supplied it, so its missing keys are named at startup; one that
// is switched off is skipped even when supplied, so a secret it never
// uses is not demanded. A section in optional is validated only when
// supplied, and every other section always.
func (c *Config) plan(supplied kit.Supplied) []kit.Section {
	list := c.sectionList()
	for i, s := range list {
		on, why := c.inUse(s.Name)
		switch {
		case !on && supplied.Configured(s.Name):
			list[i].Value = inactive(why)
		case !on, optional[s.Name] && !supplied.Configured(s.Name):
			list[i].Value = kit.AbsentSection{}
		}
	}
	return list
}

// inUse reports whether the key that switches the named section selects
// it, and when it does not, why. A section no key switches is always in
// use.
func (c *Config) inUse(name string) (bool, string) {
	mode := "fiber.auth.mode is " + c.Auth.Mode
	switch name {
	case "jwt":
		return c.Auth.IsJWT(), mode
	case "session":
		return !c.Auth.IsJWT(), mode
	case "whatsapp":
		return c.Notification.Enabled(kit.ChannelWhatsApp),
			"whatsapp is not in notification.channels"
	case "email":
		return c.Notification.Enabled(kit.ChannelEmail),
			"email is not in notification.channels"
	}
	return true, ""
}

// validate runs every section's Validate, then validateCrossSection, and
// joins the results so one restart reports every problem.
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

// validateCrossSection holds the rules the toolkit leaves to the
// application, because each needs keys from two sections and a section's
// Validate only ever sees its own.
func (c *Config) validateCrossSection() []error {
	var errs []error

	// Fiber answers 413 for a request over body_limit before any handler
	// runs, and 0 means its own 4 MiB, so the limit has to cover the
	// largest file fileutil accepts plus the envelope around it.
	minBody := multipartAllowance
	if fileutil.MaxFileBytes > 0 {
		minBody += fileutil.MaxFileBytes
	}
	if c.Fiber.BodyLimit < minBody {
		errs = append(errs, fmt.Errorf("fiber.body_limit must be at "+
			"least %d (got %d): one upload request carries a file of up "+
			"to fileutil.MaxFileBytes (%d) plus %d bytes of multipart "+
			"envelope, and an absent key reads as 0, which Fiber "+
			"replaces with its own 4 MiB",
			minBody, c.Fiber.BodyLimit, fileutil.MaxFileBytes,
			multipartAllowance))
	}

	// On a unix listener app.host is the socket path; see Addr.
	if c.Listen.ListenerNetwork == "unix" &&
		strings.TrimSpace(c.App.Host) == "" {
		errs = append(errs, errors.New("app.host is required when "+
			"fiber.listen.listener_network is unix: it is the socket "+
			"path, and an empty one fails inside net.Listen"))
	}

	// The access log reads the id from a fixed X-Request-ID header, so
	// renaming the header empties the requestId field on every line.
	if c.RequestID.Header != "" &&
		c.RequestID.Header != kit.DefaultRequestIDHeader &&
		slices.Contains(c.Zerolog.Fields, fiberzerolog.FieldRequestID) {
		errs = append(errs, fmt.Errorf("fiber.requestid.header must be "+
			"%q while fiber.zerolog.fields includes %s (got %q): the "+
			"access log reads the id from the %s header only, so any "+
			"other name logs it empty on every request",
			kit.DefaultRequestIDHeader, fiberzerolog.FieldRequestID,
			c.RequestID.Header, kit.DefaultRequestIDHeader))
	}

	// Each prefork worker keeps its own in-memory session store. Drop
	// this rule if the application installs a shared session Storage.
	if !c.Auth.IsJWT() && c.Listen.EnablePrefork {
		errs = append(errs, errors.New("fiber.listen.enable_prefork "+
			"cannot be true while fiber.auth.mode is session: every "+
			"worker keeps its own in-memory session store, so a request "+
			"that lands on another worker arrives logged out"))
	}

	// logger.New floors the logger at debug in development and at info
	// everywhere else, and a request logged below the floor is dropped.
	minLevel := zerolog.InfoLevel
	if c.App.Env == kit.EnvDevelopment {
		minLevel = zerolog.DebugLevel
	}
	for _, level := range c.Zerolog.Levels {
		if level < minLevel {
			errs = append(errs, fmt.Errorf("fiber.zerolog.levels "+
				"includes %s, but the logger for app.env %q drops "+
				"everything below %s, so that response class is never "+
				"logged", level, c.App.Env, minLevel))
		}
	}

	return errs
}

// Addr is the address to hand to Fiber's Listen: app.host verbatim, as
// the socket path, when fiber.listen.listener_network is unix, and
// host:port otherwise.
func (c *Config) Addr() string {
	if c.Listen.ListenerNetwork == "unix" {
		return c.App.Host
	}
	return net.JoinHostPort(c.App.Host, strconv.Itoa(c.App.Port))
}

// Log writes one line per section through its String, which masks
// passwords, tokens and secrets. A section this deployment does not use
// is shown as such rather than as zero values nobody chose.
func (c *Config) Log(log zerolog.Logger) {
	file := c.file
	if file == "" {
		file = "(none: environment only)"
	}
	log.Info().Str("file", file).Msg("configuration source")
	for _, s := range c.sections {
		log.Info().
			Str("section", s.Name).
			Str("config", s.Value.String()).
			Msg("configuration loaded")
	}
}

// inactive stands in for a section that was supplied but is switched off
// by another key. Like kit.AbsentSection it validates clean; unlike it,
// the log line says why the values are not in use.
type inactive string

func (s inactive) String() string { return "<inactive: " + string(s) + ">" }

func (inactive) Validate() error { return nil }
