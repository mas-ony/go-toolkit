package config

// Tests for fiber_session_config.go.
//
// What the section promises: both timeouts present, positive, at least a
// second, and ordered so that the middleware never panics on anything
// Validate passed; SameSite naming a real mode in any casing; and a None
// cookie declared Secure, since the middleware serves it Secure anyway.

import (
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3/middleware/session"
	"github.com/spf13/viper"
)

// sessionBase is a configuration the session section accepts, which each
// rule case changes one key at a time.
var sessionBase = map[string]any{
	"fiber.session.absolute_timeout": "24h",
	"fiber.session.idle_timeout":     "30m",
	"fiber.session.cookie_same_site": "Lax",
}

// buildSession adapts NewSessionConfig to the constructor shape runRules
// takes.
func buildSession(v *viper.Viper) SectionConfig { return NewSessionConfig(v) }

// containsFold, the comparison validSameSite goes through, matches under
// case folding and finds nothing that is not in the list, least of all in
// an empty one. A misspelling is absent however it is capitalised.
func TestContainsFold(t *testing.T) {
	t.Parallel()
	list := []string{"Strict", "Lax"}
	if !containsFold(list, "strict") || !containsFold(list, "LAX") {
		t.Error("containsFold should ignore case")
	}
	if containsFold(list, "Strcit") || containsFold(nil, "Lax") {
		t.Error("containsFold found something absent")
	}
}

// TestSessionValidate holds the section's rules, each beside the value
// that satisfies it.
func TestSessionValidate(t *testing.T) {
	t.Parallel()
	runRules(t, sessionBase, buildSession, []ruleCase{
		{"no absolute timeout", map[string]any{
			"fiber.session.absolute_timeout": nil},
			"fiber.session.absolute_timeout is required"},
		// The middleware reads a negative ceiling as no ceiling at all.
		{"a negative absolute timeout", map[string]any{
			"fiber.session.absolute_timeout": "-1h"},
			"fiber.session.absolute_timeout must be positive"},
		// 86400 meant as a day is 86.4µs.
		{"a bare-number absolute timeout", map[string]any{
			"fiber.session.absolute_timeout": 86400},
			"fiber.session.absolute_timeout must be at least 1s"},
		{"no idle timeout", map[string]any{
			"fiber.session.idle_timeout": nil},
			"fiber.session.idle_timeout is required"},
		// The middleware replaces a negative idle timeout with 30m, which
		// would then panic against the 10m ceiling of the next case.
		{"a negative idle timeout", map[string]any{
			"fiber.session.idle_timeout": "-5m"},
			"fiber.session.idle_timeout must be positive"},
		{"a bare-number idle timeout", map[string]any{
			"fiber.session.idle_timeout": 1800},
			"fiber.session.idle_timeout must be at least 1s"},
		// A session that must end before it can go idle makes the idle
		// timeout unreachable, which is a misreading of one of the two.
		{"absolute shorter than idle", map[string]any{
			"fiber.session.absolute_timeout": "10m"},
			"must be greater than or equal to"},
		{"absolute equal to idle", map[string]any{
			"fiber.session.absolute_timeout": "30m"}, ""},
		// Required rather than defaulted: an empty value is served as Lax
		// by the middleware, where neither the file nor a startup line
		// shows it.
		{"no SameSite", map[string]any{
			"fiber.session.cookie_same_site": nil},
			"fiber.session.cookie_same_site"},
		{"an unknown SameSite", map[string]any{
			"fiber.session.cookie_same_site": "Sometimes"},
			"fiber.session.cookie_same_site"},
		{"SameSite ignores case", map[string]any{
			"fiber.session.cookie_same_site": "strict"}, ""},
		// The middleware serves a SameSite=None cookie as Secure whatever
		// cookie_secure says, so false there would be reported but not
		// served.
		{"None without Secure", map[string]any{
			"fiber.session.cookie_same_site": "None"},
			"fiber.session.cookie_secure must be true"},
		{"None with Secure", map[string]any{
			"fiber.session.cookie_same_site": "None",
			"fiber.session.cookie_secure":    true}, ""},
	})
}

// A bad timeout reports itself and not ALSO a pairing it caused: the
// pairing is compared only once both halves are in range. A negative idle
// timeout under a 10m ceiling is the case where the two could both fire.
func TestSessionOutOfRangeTimeoutIsReportedOnce(t *testing.T) {
	t.Parallel()
	err := (&SessionConfig{Config: &session.Config{
		AbsoluteTimeout: 10 * time.Minute,
		IdleTimeout:     -5 * time.Minute,
		CookieSameSite:  "Lax",
	}}).Validate()
	if err == nil {
		t.Fatal("a negative idle timeout validated")
	}
	if strings.Contains(err.Error(), "greater than or equal") {
		t.Errorf("the pairing was reported beside its cause:\n%v", err)
	}
}

// Validate stands between config.yaml and a startup panic: session.New
// panics when the ceiling is below the idle timeout it ends up with, after
// it has replaced a zero or negative one with 30m. So every pairing that
// Validate passes has to build the middleware without panicking. The grid
// includes zero and negative idle timeouts, which the middleware replaces
// before it compares, so a check reading only the values as written would
// miss them.
func TestSessionValidatePassesNothingThatPanics(t *testing.T) {
	t.Parallel()
	durations := []time.Duration{-time.Hour, -5 * time.Minute, 0,
		time.Nanosecond, 10 * time.Minute, 30 * time.Minute, time.Hour}
	for _, abs := range durations {
		for _, idle := range durations {
			c := &SessionConfig{Config: &session.Config{
				AbsoluteTimeout: abs,
				IdleTimeout:     idle,
				CookieSameSite:  "Lax",
			}}
			if c.Validate() != nil {
				continue
			}
			func() {
				defer func() {
					if p := recover(); p != nil {
						t.Errorf("absolute=%s idle=%s validated, then "+
							"session.New panicked: %v", abs, idle, p)
					}
				}()
				_ = session.New(*c.Config)
			}()
		}
	}
}

// The log line names the cookie the middleware will actually use: the
// default when no Extractor is set, which is the only way this section
// builds one, and a placeholder rather than a panic when the embedded
// Config was never built.
//
// The default is spelled twice, once in sessionCookieName and once in the
// middleware's ConfigDefault, so the two are compared first: a Fiber
// release that renamed its cookie fails here, not in a log line.
func TestSessionStringNamesTheCookie(t *testing.T) {
	t.Parallel()
	if def := session.ConfigDefault.Extractor.Key; def != sessionCookieName {
		t.Errorf("sessionCookieName = %q, but the middleware's default "+
			"cookie is %q", sessionCookieName, def)
	}

	got := NewSessionConfig(withKeys(sessionBase, nil)).String()
	want := "CookieName=session_id (middleware default)"
	if !strings.Contains(got, want) {
		t.Errorf("String = %q, want it to contain %q", got, want)
	}
	if got := (&SessionConfig{}).String(); got !=
		"<uninitialised SessionConfig>" {
		t.Errorf("String on a nil Config = %q", got)
	}
}
