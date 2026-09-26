package config

// Tests for config.go. Each case writes a real config.yaml and applies its
// changes as environment variables, so the override path a deployment
// relies on is exercised along with every rule. t.Setenv rules out
// t.Parallel.

import (
	"bytes"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

// committedConfig is the repository's own config.yaml, relative to this
// package: the layout the package documentation assumes.
const committedConfig = "../../config.yaml"

// The credentials every case supplies through the environment, the way
// config.yaml expects a deployment to.
const (
	testUser     = "svc_user"
	testPassword = "s3cret;pass"
)

// base is a small configuration New accepts once the credentials come
// from the environment. Each case changes it one setting at a time.
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
  body_limit: 22020096  # the least New accepts: 20 MiB + 1 MiB
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

// env is one case's environment overrides.
type env map[string]string

// jwtMode switches the base to JWT authentication with everything that
// mode requires.
var jwtMode = env{
	"FIBER_AUTH_MODE":  "jwt",
	"FIBER_JWT_SECRET": strings.Repeat("k", 32),
	"FIBER_JWT_EXPIRY": "1h",
}

// The repository's config.yaml has to load once the environment supplies
// what it leaves blank, or the service it configures cannot start.
func TestCommittedConfigLoads(t *testing.T) {
	isolate(t)
	t.Setenv("DATABASE_USERNAME", testUser)
	t.Setenv("DATABASE_PASSWORD", testPassword)
	t.Setenv("FIBER_JWT_SECRET", jwtMode["FIBER_JWT_SECRET"])
	if _, err := New(committedConfig); err != nil {
		t.Fatalf("%s does not load:\n%v", committedConfig, err)
	}
}

// Without the environment the credentials are missing, and New names
// both keys rather than starting on a login that cannot succeed.
func TestCredentialsAreRequired(t *testing.T) {
	isolate(t)
	_, err := New(writeConfig(t, base))
	wantErr(t, err, "database.username is required",
		"database.password is required")
}

// A missing file is tolerated so the environment alone can configure a
// deployment, and the error names it, or the list of required keys that
// follows would read as a broken file.
func TestMissingFileIsNamed(t *testing.T) {
	isolate(t)
	_, err := New(filepath.Join(t.TempDir(), "absent.yaml"))
	wantErr(t, err, "absent.yaml not found", "app.name is required")
}

// A file that does not parse is refused outright rather than treated as
// missing.
func TestUnparseableFileIsRefused(t *testing.T) {
	isolate(t)
	_, err := New(writeConfig(t, "app: [\n"))
	wantErr(t, err, "reading ")
	if strings.Contains(err.Error(), "not found") {
		t.Errorf("a parse error reads as a missing file:\n%v", err)
	}
}

// TestRules changes the base one setting at a time. A case with a
// fragment must be refused with an error naming it; a case without one
// must load, which pins the way out beside each refusal.
func TestRules(t *testing.T) {
	prefork := env{
		"FIBER_LISTEN_ENABLE_PREFORK":                "true",
		"FIBER_LISTEN_PREFORK_SHUTDOWN_GRACE_PERIOD": "15s",
	}
	cases := []struct {
		name string
		set  env
		want string
	}{
		{"the base loads", nil, ""},

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
		{"a channel not chosen is not validated",
			env{"NOTIFICATION_EMAIL_HOST": "smtp.example.go.id"}, ""},

		{"body_limit one byte under the floor",
			env{"FIBER_BODY_LIMIT": "22020095"},
			"fiber.body_limit must be at least 22020096"},

		{"a unix socket demands app.host",
			env{"FIBER_LISTEN_LISTENER_NETWORK": "unix"},
			"app.host is required when"},
		{"a unix socket with app.host", env{
			"FIBER_LISTEN_LISTENER_NETWORK": "unix",
			"APP_HOST":                      "/run/service.sock",
		}, ""},

		{"a renamed request id header while requestId is logged",
			env{"FIBER_REQUESTID_HEADER": "X-Correlation-ID"},
			`fiber.requestid.header must be "X-Request-ID"`},
		{"a renamed request id header while requestId is not logged",
			env{
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
			if got := mustLoad(t, base, c.set).Addr(); got != c.want {
				t.Errorf("Addr() = %q, want %q", got, c.want)
			}
		})
	}
}

// The startup log says, per section, whether it is in use, and never
// carries a credential.
func TestLogShowsWhatIsInUse(t *testing.T) {
	c := mustLoad(t, base, env{
		"FIBER_JWT_SECRET":        jwtMode["FIBER_JWT_SECRET"],
		"NOTIFICATION_EMAIL_HOST": "smtp.example.go.id",
	})
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
	for name, want := range map[string]string{
		"notification": "<not configured>",
		"whatsapp":     "<not configured>",
		"email":        "<inactive: email is not in notification.channels>",
		"jwt":          "<inactive: fiber.auth.mode is session>",
		"client":       "<not configured>",
	} {
		if got[name] != want {
			t.Errorf("section %s logged %q, want %q", name, got[name], want)
		}
	}
	if !strings.HasPrefix(got["session"], "AbsoluteTimeout=8h0m0s ") {
		t.Errorf("session logged %q, want its values", got["session"])
	}
	for _, secret := range []string{
		testUser, testPassword, jwtMode["FIBER_JWT_SECRET"],
	} {
		if strings.Contains(buf.String(), secret) {
			t.Errorf("the startup log carries %q", secret)
		}
	}
}

// isolate blanks every variable that routes into a section, so a shell
// that exports DATABASE_PASSWORD or APP_ENV to run the service cannot
// change what a test sees. Viper and SuppliedSections both treat an empty
// variable as unset.
func isolate(t *testing.T) {
	t.Helper()
	var c Config
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		for _, s := range c.sectionList() {
			p := strings.ToUpper(strings.ReplaceAll(s.Prefix, ".", "_"))
			if name == p || strings.HasPrefix(name, p+"_") {
				t.Setenv(name, "")
				break
			}
		}
	}
}

// load writes body to a config.yaml, supplies the credentials, applies
// set on top, and runs New on the result.
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

// mustLoad is load for a case that has to succeed.
func mustLoad(t *testing.T, body string, set env) *Config {
	t.Helper()
	c, err := load(t, body, set)
	if err != nil {
		t.Fatalf("refused:\n%v", err)
	}
	return c
}

// writeConfig writes body to a config.yaml in a fresh directory and
// returns its path.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// wantErr fails the test unless err mentions every fragment.
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

// merge returns a copy of a with b applied on top.
func merge(a, b env) env {
	out := maps.Clone(a)
	maps.Copy(out, b)
	return out
}
