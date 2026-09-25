package config

// Tests for fiber_session_config.go.

import (
	"testing"

	"github.com/spf13/viper"
)

func TestContainsFold(t *testing.T) {
	t.Parallel()
	list := []string{"Status", "Method"}
	if !containsFold(list, "status") || !containsFold(list, "METHOD") {
		t.Error("containsFold should ignore case")
	}
	if containsFold(list, "latency") || containsFold(nil, "x") {
		t.Error("containsFold found something absent")
	}
}

var sessionBase = map[string]any{
	"fiber.session.absolute_timeout": "24h",
	"fiber.session.idle_timeout":     "30m",
	"fiber.session.cookie_same_site": "Lax",
}

func buildSession(v *viper.Viper) SectionConfig { return NewSessionConfig(v) }

func TestSessionValidate(t *testing.T) {
	t.Parallel()
	runRules(t, sessionBase, buildSession, []ruleCase{
		{"no absolute timeout", map[string]any{
			"fiber.session.absolute_timeout": nil},
			"fiber.session.absolute_timeout"},
		{"no idle timeout", map[string]any{
			"fiber.session.idle_timeout": nil},
			"fiber.session.idle_timeout"},
		// A session that must end before it can go idle makes the idle
		// timeout unreachable, which is a misreading of one of the two.
		{"absolute shorter than idle", map[string]any{
			"fiber.session.absolute_timeout": "10m"},
			"fiber.session.absolute_timeout"},
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
		// Browsers refuse a SameSite=None cookie that is not Secure, so
		// the session would silently never be stored.
		{"None without Secure", map[string]any{
			"fiber.session.cookie_same_site": "None"},
			"fiber.session.cookie_same_site"},
		{"None with Secure", map[string]any{
			"fiber.session.cookie_same_site": "None",
			"fiber.session.cookie_secure":    true}, ""},
	})
}
