package config

// Tests for config.go.
//
// Most cases write a real config.yaml into a temporary directory and
// apply their changes as environment variables, so the override path a
// deployment relies on is exercised along with every rule. Every case
// that calls New starts from isolate, so variables exported by the
// shell running go test cannot leak in.
//
// Nothing here calls t.Parallel: t.Setenv panics in a parallel test,
// because the environment belongs to the whole process, and so does
// fileutil.MaxFileBytes, which some cases set.

import (
	"bytes"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mas-ony/go-toolkit/fileutil"
	"github.com/rs/zerolog"
)

// env is one case's environment overrides, variable name to value.
type env map[string]string

// committedConfig is the repository's own config.yaml. go test runs in
// the package's directory, so the path assumes this package sits two
// levels below the repository root, as internal/config does.
const committedConfig = "../../config.yaml"

// The credentials the cases supply through the environment, as
// config.yaml expects a deployment to. Both are distinctive, so
// TestLogShowsWhatIsInUse can search the log for them.
const (
	testUser     = "svc_user"
	testPassword = "s3cret;pass"

	// testBotToken has the shape BotFather issues and is worth nothing.
	testBotToken = "123456789:AAFakeTokenForTests_0123456789abcdefg"
)

// base is a small configuration New accepts once the credentials come
// from the environment: every always-validated section with its
// required keys, session mode, and no notification channel. Each case
// in TestRules changes it one concern at a time.
//
// body_limit is the exact floor validateCrossSection accepts under
// fileutil's default MaxFileBytes: 20 MiB of file plus the 1 MiB
// multipartAllowance.
const base = `
app:
  name: test-service
  version: "1.0.0"
  env: development
  port: 3000
  location: Asia/Jakarta
database:
  driver: sqlserver
  host: 127.0.0.1
  port: 1433
  catalog: app2026
  schema: dbo
  encrypt: "true"
  max_open_conns: 10
  max_idle_conns: 10
  conn_max_lifetime: 30m
  conn_max_idle_time: 5m
fiber:
  body_limit: 22020096  # the floor: 20 MiB + 1 MiB
  auth:
    mode: session
  session:
    absolute_timeout: 8h
    idle_timeout: 30m
    cookie_same_site: Lax
    cookie_secure: true
  limiter:
    max: 100
    expiration: 1m
    strategy: sliding
  listen:
    shutdown_timeout: 10s
  recover:
    enable_stack_trace: true
  requestid:
    header: X-Request-ID
  zerolog:
    fields: [error, status, method, url, latency, requestId]
`

// authBlock and sessionBlock are base's fiber.auth and fiber.session
// sections, verbatim, for the cases that leave them out with cut.
const (
	authBlock    = "  auth:\n    mode: session\n"
	sessionBlock = "  session:\n" +
		"    absolute_timeout: 8h\n" +
		"    idle_timeout: 30m\n" +
		"    cookie_same_site: Lax\n" +
		"    cookie_secure: true\n"
)

// bodyLimitLine and idHeaderLine are single lines of base, verbatim,
// for the cases that leave them out with cut.
const (
	bodyLimitLine = "  body_limit: 22020096  # the floor: 20 MiB + 1 MiB\n"
	idHeaderLine  = "    header: X-Request-ID\n"
)

// databaseBlock is base's database section, verbatim, for the case that
// leaves it out with cut.
const databaseBlock = "database:\n" +
	"  driver: sqlserver\n" +
	"  host: 127.0.0.1\n" +
	"  port: 1433\n" +
	"  catalog: app2026\n" +
	"  schema: dbo\n" +
	"  encrypt: \"true\"\n" +
	"  max_open_conns: 10\n" +
	"  max_idle_conns: 10\n" +
	"  conn_max_lifetime: 30m\n" +
	"  conn_max_idle_time: 5m\n"

// jwtMode switches base to JWT authentication, with the secret and
// expiry that mode requires. The secret is 32 bytes, the least the jwt
// section accepts.
var jwtMode = env{
	"FIBER_AUTH_MODE":  "jwt",
	"FIBER_JWT_SECRET": strings.Repeat("k", 32),
	"FIBER_JWT_EXPIRY": "1h",
}

// prefork switches prefork on, with the grace period the listen section
// then requires: at least fiber.listen.shutdown_timeout, which is 10s in
// base.
var prefork = env{
	"FIBER_LISTEN_ENABLE_PREFORK":                "true",
	"FIBER_LISTEN_PREFORK_SHUTDOWN_GRACE_PERIOD": "15s",
}

// telegramOn switches telegram on with the two keys its section
// requires, the token supplied the way a deployment would supply it, from
// the environment.
var telegramOn = env{
	"NOTIFICATION_CHANNELS":         "telegram",
	"NOTIFICATION_TELEGRAM_TOKEN":   testBotToken,
	"NOTIFICATION_TELEGRAM_TIMEOUT": "30s",
}

// isolate blanks every environment variable that routes to a section,
// so a shell that exports DATABASE_PASSWORD or APP_ENV to run the
// service cannot change what a test sees. It matches names the way
// config.SuppliedSections routes them: a section's prefix in its
// environment spelling, alone or followed by "_".
//
// Blanking stands in for unsetting because t.Setenv restores each
// variable when the test ends and has no unsetting counterpart. An
// empty variable is as good as unset here: Viper ignores it, since
// config.NewViper leaves AllowEmptyEnv off, and SuppliedSections skips
// it.
func isolate(t *testing.T) {
	t.Helper()
	// A zero Config is enough: only the prefixes are read.
	var c Config
	var prefixes []string
	for _, s := range c.sectionList() {
		p := strings.ReplaceAll(s.Prefix, ".", "_")
		prefixes = append(prefixes, strings.ToUpper(p))
	}
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		for _, p := range prefixes {
			if name == p || strings.HasPrefix(name, p+"_") {
				t.Setenv(name, "")
				break
			}
		}
	}
}

// writeConfig writes body to config.yaml in a fresh temporary
// directory, removed when the test ends, and returns its path.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// load isolates the environment, supplies the credentials, applies set
// on top, writes body to a config.yaml and runs New on it. set may
// override the credentials too.
func load(t *testing.T, body string, set env) (*Config, error) {
	t.Helper()
	isolate(t)
	t.Setenv("DATABASE_USERNAME", testUser)
	t.Setenv("DATABASE_PASSWORD", testPassword)
	for name, value := range set {
		t.Setenv(name, value)
	}
	return New(writeConfig(t, body))
}

// mustLoad is load for a case that has to succeed: it fails the test
// at once, with New's error, if the configuration is refused.
func mustLoad(t *testing.T, body string, set env) *Config {
	t.Helper()
	c, err := load(t, body, set)
	if err != nil {
		t.Fatalf("refused:\n%v", err)
	}
	return c
}

// wantErr fails the test unless err is non-nil and mentions every
// fragment. A nil err stops the test at once, so the caller may use err
// afterwards; a missing fragment is reported and the test continues, so
// every missing one is listed.
func wantErr(t *testing.T, err error, fragments ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("accepted; want an error mentioning %q", fragments)
	}
	for _, f := range fragments {
		if !strings.Contains(err.Error(), f) {
			t.Errorf("error lacks %q:\n%v", f, err)
		}
	}
}

// cut returns body with block removed. It fails the test unless block
// occurs in body exactly once, so a change to base cannot quietly turn a
// case that leaves a section out into one that leaves nothing out.
func cut(t *testing.T, body, block string) string {
	t.Helper()
	if n := strings.Count(body, block); n != 1 {
		t.Fatalf("block occurs %d times, want once:\n%s", n, block)
	}
	return strings.Replace(body, block, "", 1)
}

// merge returns a new env holding a with b applied on top. Neither
// argument is modified, and either may be nil.
func merge(a, b env) env {
	out := make(env, len(a)+len(b))
	maps.Copy(out, a)
	maps.Copy(out, b)
	return out
}

// setMaxFileBytes sets fileutil.MaxFileBytes, the upload cap the
// body_limit rule reads, for the rest of the test and restores it when
// the test ends.
func setMaxFileBytes(t *testing.T, n int) {
	t.Helper()
	prev := fileutil.MaxFileBytes
	fileutil.MaxFileBytes = n
	t.Cleanup(func() { fileutil.MaxFileBytes = prev })
}

// logSections runs Log on c and returns each section line's config
// field, keyed by section name, along with the whole log as written. The
// source line has no section field, so it appears only in the latter.
func logSections(t *testing.T, c *Config) (map[string]string, string) {
	t.Helper()
	var buf bytes.Buffer
	c.Log(zerolog.New(&buf))
	got := map[string]string{}
	for _, line := range strings.Split(buf.String(), "\n") {
		var entry struct{ Section, Config string }
		if json.Unmarshal([]byte(line), &entry) == nil &&
			entry.Section != "" {
			got[entry.Section] = entry.Config
		}
	}
	return got, buf.String()
}

// logSource runs Log on c and returns the file field of the
// "configuration source" line, the first one written.
func logSource(t *testing.T, c *Config) string {
	t.Helper()
	var buf bytes.Buffer
	c.Log(zerolog.New(&buf))
	line, _, _ := strings.Cut(buf.String(), "\n")
	var entry struct{ File string }
	if err := json.Unmarshal([]byte(line), &entry); err != nil {
		t.Fatalf("log line %q: %v", line, err)
	}
	return entry.File
}

// TestCommittedConfigLoads loads the repository's own config.yaml with
// the environment supplying what the file leaves blank, as a deployment
// would. If this fails, the committed file cannot start the service.
// FIBER_JWT_SECRET is supplied although the file selects no
// authentication mode, so the test keeps passing if the file switches
// to jwt.
//
// fileutil.MaxFileBytes is left at its default, the cap main runs with.
// A main that assigns another cap has to assign it here too, or this
// test checks the committed body_limit against the wrong floor.
func TestCommittedConfigLoads(t *testing.T) {
	isolate(t)
	t.Setenv("DATABASE_USERNAME", testUser)
	t.Setenv("DATABASE_PASSWORD", testPassword)
	t.Setenv("FIBER_JWT_SECRET", jwtMode["FIBER_JWT_SECRET"])
	if _, err := New(committedConfig); err != nil {
		t.Fatalf("%s does not load:\n%v", committedConfig, err)
	}
}

// TestCommittedConfigLoadsWithEveryChannel switches all three channels on
// in the repository's own config.yaml, supplying only the token the file
// leaves blank. The examples in the file are then known to be valid when
// they are used, and not only while their channels are off, and the
// values it gives telegram are the ones the section reads.
func TestCommittedConfigLoadsWithEveryChannel(t *testing.T) {
	isolate(t)
	t.Setenv("DATABASE_USERNAME", testUser)
	t.Setenv("DATABASE_PASSWORD", testPassword)
	t.Setenv("FIBER_JWT_SECRET", jwtMode["FIBER_JWT_SECRET"])
	t.Setenv("NOTIFICATION_CHANNELS", "whatsapp,email,telegram")
	t.Setenv("NOTIFICATION_TELEGRAM_TOKEN", testBotToken)

	c, err := New(committedConfig)
	if err != nil {
		t.Fatalf("%s does not load with every channel on:\n%v",
			committedConfig, err)
	}
	if got := strings.Join(c.Notification.Channels, ","); got !=
		"whatsapp,email,telegram" {
		t.Errorf("channels = %q, want all three", got)
	}

	got, raw := logSections(t, c)
	const want = "Token=(set) APIURL=https://api.telegram.org (default) " +
		"Timeout=30s Async=true"
	if got["telegram"] != want {
		t.Errorf("telegram logged %q, want %q", got["telegram"], want)
	}
	if strings.Contains(raw, testBotToken) {
		t.Errorf("the startup log carries the bot token:\n%s", raw)
	}
}

// TestCredentialsAreRequired loads base without the credentials, which
// the file deliberately leaves out. New has to name both keys rather
// than start a service whose first login cannot succeed.
func TestCredentialsAreRequired(t *testing.T) {
	isolate(t)
	_, err := New(writeConfig(t, base))
	wantErr(t, err, "database.username is required",
		"database.password is required")
}

// TestMissingFileIsNamed points New at a file that does not exist, with
// an environment that configures nothing. A missing file is tolerated,
// so the failure is the missing keys; the error has to say the file was
// not found, or that list would read as a broken file.
func TestMissingFileIsNamed(t *testing.T) {
	isolate(t)
	_, err := New(filepath.Join(t.TempDir(), "absent.yaml"))
	wantErr(t, err, "absent.yaml not found", "app.name is required")
}

// TestEnvironmentAloneLoads gives New no file to read, in both ways
// that can happen: no path at all, and a path that does not exist. The
// environment supplies everything the always-validated sections and
// the cross-section rules require, which is a complete configuration,
// so both load, and the startup log's source line says which of the
// two it was.
func TestEnvironmentAloneLoads(t *testing.T) {
	set := env{
		"APP_NAME":                      "test-service",
		"APP_VERSION":                   "1.0.0",
		"APP_ENV":                       "development",
		"APP_PORT":                      "3000",
		"APP_LOCATION":                  "Asia/Jakarta",
		"FIBER_BODY_LIMIT":              "22020096",
		"FIBER_LIMITER_MAX":             "100",
		"FIBER_LIMITER_EXPIRATION":      "1m",
		"FIBER_LIMITER_STRATEGY":        "sliding",
		"FIBER_LISTEN_SHUTDOWN_TIMEOUT": "10s",
		"FIBER_REQUESTID_HEADER":        "X-Request-ID",
		"FIBER_ZEROLOG_FIELDS":          "error,status,requestId",
	}
	absent := filepath.Join(t.TempDir(), "absent.yaml")
	for _, c := range []struct {
		name string
		path string
		want string
	}{
		{"no path", "", "(none: environment only)"},
		{"missing file", absent,
			absent + " (not found: environment only)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			isolate(t)
			for name, value := range set {
				t.Setenv(name, value)
			}
			cfg, err := New(c.path)
			if err != nil {
				t.Fatalf("refused:\n%v", err)
			}
			if got := logSource(t, cfg); got != c.want {
				t.Errorf("file logged %q, want %q",
					got, c.want)
			}
		})
	}
}

// TestUnparseableFileIsRefused feeds New malformed YAML. It has to be
// refused as a read error, not mistaken for a missing file and replaced
// by the environment.
func TestUnparseableFileIsRefused(t *testing.T) {
	isolate(t)
	_, err := New(writeConfig(t, "app: [\n"))
	wantErr(t, err, "reading ")
	if strings.Contains(err.Error(), "not found") {
		t.Errorf("a parse error reads as a missing file:\n%v", err)
	}
}

// TestFileIsYAMLWhateverItsName loads base from a file whose extension
// names no format. New has to parse it as YAML all the same, where
// Viper left to itself would go by the extension and refuse the file,
// and the startup log's source line has to name the file as given.
func TestFileIsYAMLWhateverItsName(t *testing.T) {
	isolate(t)
	t.Setenv("DATABASE_USERNAME", testUser)
	t.Setenv("DATABASE_PASSWORD", testPassword)
	path := filepath.Join(t.TempDir(), "service.conf")
	if err := os.WriteFile(path, []byte(base), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := New(path)
	if err != nil {
		t.Fatalf("%s does not load:\n%v", path, err)
	}
	if got := logSource(t, cfg); got != path {
		t.Errorf("file logged %q, want %q", got, path)
	}
}

// TestRules changes base one concern at a time and checks New's
// verdict. A case with a want fragment must be refused with an error
// containing it; a case without one must load, which pins the way out
// beside each refusal.
func TestRules(t *testing.T) {
	cases := []struct {
		name string
		set  env
		want string
	}{
		{"the base loads", nil, ""},

		// plan: a switched-on section is validated even when nothing
		// supplied it, and a switched-off one is skipped even when
		// supplied.
		{"jwt mode demands its secret",
			env{"FIBER_AUTH_MODE": "jwt"},
			"fiber.jwt.secret is required"},
		{"jwt mode with its secret", jwtMode, ""},
		{"a chosen channel demands its section",
			env{"NOTIFICATION_CHANNELS": "whatsapp,email"},
			"notification.email.host is required"},
		{"a chosen channel with its section", env{
			"NOTIFICATION_CHANNELS":      "whatsapp,email",
			"NOTIFICATION_EMAIL_HOST":    "smtp.example.go.id",
			"NOTIFICATION_EMAIL_PORT":    "587",
			"NOTIFICATION_EMAIL_TLS":     "starttls",
			"NOTIFICATION_EMAIL_FROM":    "noreply@example.go.id",
			"NOTIFICATION_EMAIL_TIMEOUT": "30s",
		}, ""},
		// The host alone would fail the email section's own checks,
		// so loading shows the section was skipped.
		{"a channel not chosen is not validated", env{
			"NOTIFICATION_EMAIL_HOST": "smtp.example.go.id",
		}, ""},
		{"telegram demands its section",
			env{"NOTIFICATION_CHANNELS": "telegram"},
			"notification.telegram.token is required"},
		{"telegram with its section", telegramOn, ""},
		{"a chosen telegram section is held to its rules",
			merge(telegramOn, env{
				"NOTIFICATION_TELEGRAM_API_URL": "http://bot.example.go.id",
			}), "clear text"},
		// The timeout alone would fail the telegram section's own
		// checks, so loading shows the section was skipped.
		{"telegram not chosen is not validated", env{
			"NOTIFICATION_TELEGRAM_TIMEOUT": "1ns",
		}, ""},

		// plan: an optional section is validated once any of its keys
		// is supplied. Left out, as in base, it is not.
		{"a supplied optional section is validated",
			env{"FIBER_CLIENT_RETRIES": "-1"},
			"fiber.client.retries cannot be negative"},
		{"a supplied optional section that is valid",
			env{"FIBER_CLIENT_RETRIES": "2"}, ""},
		{"a supplied notification section is validated",
			env{"NOTIFICATION_CHANNELS": "sms"},
			"notification.channels[0] is not a channel"},

		// validateCrossSection, one rule at a time. base sits exactly
		// on the body_limit floor, so "the base loads" is that rule's
		// way out.
		{"body_limit one byte under the floor",
			env{"FIBER_BODY_LIMIT": "22020095"},
			"fiber.body_limit must be at least 22020096"},

		{"a unix socket demands app.host",
			env{"FIBER_LISTEN_LISTENER_NETWORK": "unix"},
			"app.host is required when"},
		{"a unix socket refuses a blank app.host", env{
			"FIBER_LISTEN_LISTENER_NETWORK": "unix",
			"APP_HOST":                      "  ",
		}, "app.host is required when"},
		{"a unix socket with app.host", env{
			"FIBER_LISTEN_LISTENER_NETWORK": "unix",
			"APP_HOST":                      "/run/service.sock",
		}, ""},

		{"a renamed id header while requestId is logged",
			env{"FIBER_REQUESTID_HEADER": "X-Correlation-ID"},
			`fiber.requestid.header must be "X-Request-ID"`},
		{"a re-cased id header while requestId is logged",
			env{"FIBER_REQUESTID_HEADER": "x-request-id"},
			`fiber.requestid.header must be "X-Request-ID"`},
		{"a renamed id header while requestId is not logged", env{
			"FIBER_REQUESTID_HEADER": "X-Correlation-ID",
			"FIBER_ZEROLOG_FIELDS":   "error,status,method,url",
		}, ""},

		{"prefork under session auth", prefork,
			"fiber.listen.enable_prefork cannot be true"},
		{"prefork under jwt auth", merge(jwtMode, prefork), ""},

		{"debug outside development", env{
			"APP_ENV":              "production",
			"FIBER_ZEROLOG_LEVELS": "error,warn,debug",
		}, "fiber.zerolog.levels includes debug"},
		{"debug in development",
			env{"FIBER_ZEROLOG_LEVELS": "error,warn,debug"}, ""},
		{"trace even in development",
			env{"FIBER_ZEROLOG_LEVELS": "error,warn,trace"},
			"fiber.zerolog.levels includes trace"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := load(t, base, c.set)
			if c.want == "" {
				if err != nil {
					t.Fatalf("refused:\n%v", err)
				}
				return
			}
			wantErr(t, err, c.want)
		})
	}
}

// TestEveryProblemIsReported breaks two sections and two cross-section
// rules in one configuration. New has to name all of it in one error,
// or fixing what it did report would only uncover the rest on the next
// restart: a failed section must hide neither another section nor the
// cross-section rules, and a rule must report every value it refuses.
func TestEveryProblemIsReported(t *testing.T) {
	_, err := load(t, base, env{
		"APP_ENV":                "prod",
		"FIBER_LIMITER_MAX":      "0",
		"FIBER_REQUESTID_HEADER": "X-Correlation-ID",
		"FIBER_ZEROLOG_LEVELS":   "error,debug,trace",
	})
	wantErr(t, err,
		"app.env must be one of",
		"fiber.limiter.max must be a positive number",
		`fiber.requestid.header must be "X-Request-ID"`,
		"fiber.zerolog.levels includes debug",
		"fiber.zerolog.levels includes trace")
}

// TestAbsentIDHeaderIsReportedOnce leaves fiber.requestid.header out
// while requestId is logged. The requestid section reports the missing
// key, and the header rule in validateCrossSection has to stay silent
// rather than report the same gap again as a wrong spelling.
func TestAbsentIDHeaderIsReportedOnce(t *testing.T) {
	_, err := load(t, cut(t, base, idHeaderLine), nil)
	wantErr(t, err, "fiber.requestid.header is required")
	if strings.Contains(err.Error(), "fiber.requestid.header must be") {
		t.Errorf("the absent header is reported twice:\n%v", err)
	}
}

// TestAddr covers the shapes Addr produces: host:port with an empty or
// a specific host, a bracketed IPv6 address, and a bare socket path on
// a unix listener.
func TestAddr(t *testing.T) {
	for _, c := range []struct {
		name string
		set  env
		want string
	}{
		{"every interface", nil, ":3000"},
		{"loopback", env{"APP_HOST": "127.0.0.1"}, "127.0.0.1:3000"},
		{"ipv6", env{"APP_HOST": "::1"}, "[::1]:3000"},
		{"unix socket", env{
			"APP_HOST":                      "/run/service.sock",
			"FIBER_LISTEN_LISTENER_NETWORK": "unix",
		}, "/run/service.sock"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := mustLoad(t, base, c.set).Addr()
			if got != c.want {
				t.Errorf("Addr() = %q, want %q", got, c.want)
			}
		})
	}
}

// TestLogShowsWhatIsInUse checks the startup log's section lines. The
// case supplies keys for three switched-off sections, fiber.jwt through
// its secret, notification.email through its host and
// notification.whatsapp through its one key, so the log has to mark
// those inactive with the reason, mark untouched optional and
// switched-off sections as not configured, print the sections in use
// with their values, and never print a credential.
//
// notification itself stays not configured, which holds only while each
// channel's prefix routes the channel's variables to it and not to the
// section above it.
func TestLogShowsWhatIsInUse(t *testing.T) {
	c := mustLoad(t, base, env{
		"FIBER_JWT_SECRET":            jwtMode["FIBER_JWT_SECRET"],
		"NOTIFICATION_EMAIL_HOST":     "smtp.example.go.id",
		"NOTIFICATION_WHATSAPP_ASYNC": "true",
	})
	got, raw := logSections(t, c)
	for name, want := range map[string]string{
		"notification": "<not configured>",
		"telegram":     "<not configured>",
		"whatsapp": "<inactive: whatsapp is not in " +
			"notification.channels>",
		"email": "<inactive: email is not in " +
			"notification.channels>",
		"jwt":    "<inactive: fiber.auth.mode is session>",
		"client": "<not configured>",
	} {
		if got[name] != want {
			t.Errorf("section %s logged %q, want %q",
				name, got[name], want)
		}
	}
	for _, name := range []string{
		"app", "database", "fiber", "auth", "session", "limiter",
		"listen", "recover", "requestid", "zerolog",
	} {
		if v := got[name]; v == "" || strings.HasPrefix(v, "<") {
			t.Errorf("section %s logged %q, want its values",
				name, v)
		}
	}
	if !strings.HasPrefix(got["session"], "AbsoluteTimeout=8h0m0s ") {
		t.Errorf("session logged %q, want its values", got["session"])
	}
	for _, secret := range []string{
		testUser, testPassword, jwtMode["FIBER_JWT_SECRET"],
	} {
		if strings.Contains(raw, secret) {
			t.Errorf("the startup log carries %q", secret)
		}
	}
}

// TestLogShowsTelegramWithoutItsToken switches telegram on and checks the
// startup line it gets: the values the environment gave it, and the token
// reported as set and never printed.
func TestLogShowsTelegramWithoutItsToken(t *testing.T) {
	c := mustLoad(t, base, telegramOn)
	got, raw := logSections(t, c)

	const want = "Token=(set) APIURL=https://api.telegram.org (default) " +
		"Timeout=30s Async=false"
	if got["telegram"] != want {
		t.Errorf("telegram logged %q, want %q", got["telegram"], want)
	}
	if strings.Contains(raw, testBotToken) {
		t.Errorf("the startup log carries the bot token:\n%s", raw)
	}
}

// TestLogMarksASuppliedTelegramSectionInactive supplies the token without
// choosing the channel. The section is skipped, and the log says it was
// supplied and why it is not in use. That holds only while the section's
// prefix routes NOTIFICATION_TELEGRAM_* to it and not to notification,
// which would otherwise count the variable as its own.
func TestLogMarksASuppliedTelegramSectionInactive(t *testing.T) {
	c := mustLoad(t, base, env{"NOTIFICATION_TELEGRAM_TOKEN": testBotToken})
	got, raw := logSections(t, c)

	for name, want := range map[string]string{
		"telegram": "<inactive: telegram is not in " +
			"notification.channels>",
		"notification": "<not configured>",
	} {
		if got[name] != want {
			t.Errorf("section %s logged %q, want %q",
				name, got[name], want)
		}
	}
	if strings.Contains(raw, testBotToken) {
		t.Errorf("the startup log carries the bot token:\n%s", raw)
	}
}

// TestAuthIsOptional leaves fiber.auth.mode out. With no mode, neither
// auth section is in use: fiber.session is not demanded when absent and
// is skipped when present, prefork is allowed, and the startup log says
// which sections were left out and why the supplied ones are unused.
func TestAuthIsOptional(t *testing.T) {
	noAuth := cut(t, base, authBlock)
	noSession := cut(t, noAuth, sessionBlock)

	t.Run("no session keys are demanded", func(t *testing.T) {
		mustLoad(t, noSession, nil)
	})
	t.Run("prefork is allowed", func(t *testing.T) {
		mustLoad(t, noSession, prefork)
	})
	t.Run("an unknown mode is still refused", func(t *testing.T) {
		_, err := load(t, noSession, env{"FIBER_AUTH_MODE": "JWT"})
		wantErr(t, err, `fiber.auth.mode "JWT" is not supported`)
	})
	t.Run("the log says what is unused", func(t *testing.T) {
		got, _ := logSections(t, mustLoad(t, noAuth, env{
			"FIBER_JWT_SECRET": jwtMode["FIBER_JWT_SECRET"],
		}))
		for name, want := range map[string]string{
			"auth":    "<not configured>",
			"session": "<inactive: fiber.auth.mode is not set>",
			"jwt":     "<inactive: fiber.auth.mode is not set>",
		} {
			if got[name] != want {
				t.Errorf("section %s logged %q, want %q",
					name, got[name], want)
			}
		}
	})
}

// TestLogNamesItsSource checks the "configuration source" line for the
// three ways New can end up reading its input: a file that was read, a
// path that did not exist, and no path at all. Log reads only the file
// fields for that line, so each case sets them directly; with no
// sections planned, the source line is the only one written.
func TestLogNamesItsSource(t *testing.T) {
	for _, c := range []struct {
		name string
		cfg  Config
		want string
	}{
		{"file read", Config{file: "config.yaml"}, "config.yaml"},
		{"file missing", Config{file: "config.yaml", missing: true},
			"config.yaml (not found: environment only)"},
		{"no file", Config{}, "(none: environment only)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := logSource(t, &c.cfg); got != c.want {
				t.Errorf("file logged %q, want %q",
					got, c.want)
			}
		})
	}
}

// TestDatabaseIsOptional leaves the database section out, from the file
// and from the environment, which load otherwise fills with credentials.
// An empty variable supplies nothing, so blanking the two leaves no source
// at all. The rest still loads, the startup log says the section was not
// configured, and Configured tells the application not to open a pool.
func TestDatabaseIsOptional(t *testing.T) {
	c := mustLoad(t, cut(t, base, databaseBlock), env{
		"DATABASE_USERNAME": "",
		"DATABASE_PASSWORD": "",
	})
	if c.Database.Configured() {
		t.Error("Database.Configured() = true with no database section")
	}
	if got, _ := logSections(t, c); got["database"] != "<not configured>" {
		t.Errorf("database logged %q, want <not configured>",
			got["database"])
	}

	// Supplying any one key switches the section back on, in full.
	_, err := load(t, cut(t, base, databaseBlock), nil)
	wantErr(t, err, "database.host is required")
}

// TestBodyLimitIsJudgedAsFiberEnforcesIt leaves fiber.body_limit out.
// Fiber then enforces its own 4 MiB, and the floor is held against that
// rather than against the literal 0: under fileutil's default 20 MiB
// cap it is refused, with a message saying what 0 turns into, and under
// a 2 MiB cap, which 4 MiB covers, the same file loads.
func TestBodyLimitIsJudgedAsFiberEnforcesIt(t *testing.T) {
	noLimit := cut(t, base, bodyLimitLine)

	_, err := load(t, noLimit, nil)
	wantErr(t, err, "fiber.body_limit must be at least 22020096",
		"which Fiber replaces with its own 4194304")

	setMaxFileBytes(t, 2<<20)
	mustLoad(t, noLimit, nil)
}

// TestBodyLimitFollowsTheFileCap raises fileutil.MaxFileBytes before
// New, the way main has to. The floor moves with it, so a cap raised in
// code is caught at startup when config.yaml's body_limit was left
// behind.
func TestBodyLimitFollowsTheFileCap(t *testing.T) {
	setMaxFileBytes(t, 50<<20)

	_, err := load(t, base, nil)
	wantErr(t, err, "fiber.body_limit must be at least 53477376",
		"fileutil.MaxFileBytes (52428800)")

	mustLoad(t, base, env{"FIBER_BODY_LIMIT": "53477376"})
}

// TestBodyLimitWithoutAFileCap switches fileutil's cap off, which 0 and
// every negative value do alike. The floor is then the multipart
// envelope alone, and the refusal says that no cap is set instead of
// quoting a cap of 0 bytes.
func TestBodyLimitWithoutAFileCap(t *testing.T) {
	for _, c := range []struct {
		name     string
		maxBytes int
	}{
		{"zero", 0},
		{"negative", -1},
	} {
		t.Run(c.name, func(t *testing.T) {
			setMaxFileBytes(t, c.maxBytes)

			_, err := load(t, base,
				env{"FIBER_BODY_LIMIT": "1048575"})
			wantErr(t, err,
				"fiber.body_limit must be at least 1048576",
				"sets no file cap")

			mustLoad(t, base, env{"FIBER_BODY_LIMIT": "1048576"})
		})
	}
}
