package config

// Tests for fiber_auth_config.go.
//
// What the section promises: the key is optional, an absent or empty mode
// selects no authentication and validates clean, the two modes are
// matched exactly, and neither branch helper selects a mechanism nobody
// named, not even through a nil receiver.

import (
	"testing"

	"github.com/spf13/viper"
)

// buildAuth adapts NewAuthConfig to the constructor shape runRules takes.
func buildAuth(v *viper.Viper) SectionConfig { return NewAuthConfig(v) }

// The key is optional. An absent one reads as an empty Mode, which selects
// no authentication rather than either mechanism, and validates clean.
func TestAuthModeIsOptional(t *testing.T) {
	t.Parallel()
	c := NewAuthConfig(viper.New())
	if c.Mode != "" {
		t.Errorf("absent mode = %q, want empty", c.Mode)
	}
	if c.IsSession() || c.IsJWT() {
		t.Error("an absent mode selected a mechanism")
	}
	if err := c.Validate(); err != nil {
		t.Errorf("an absent mode was refused: %v", err)
	}
}

// Each helper answers true for its own mode and nothing else. An empty
// mode, a misspelled one, and a nil receiver select neither. The nil case
// is load-bearing: a service calls these to BUILD its validation list,
// before that list reports a nil section.
func TestAuthBranches(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name         string
		cfg          *AuthConfig
		session, jwt bool
	}{
		{"nil", nil, false, false},
		{"empty", &AuthConfig{}, false, false},
		{"session", &AuthConfig{Mode: AuthModeSession}, true, false},
		{"jwt", &AuthConfig{Mode: AuthModeJWT}, false, true},
		{"upper-case JWT", &AuthConfig{Mode: "JWT"}, false, false},
	} {
		if got := c.cfg.IsSession(); got != c.session {
			t.Errorf("%s: IsSession = %t, want %t", c.name, got,
				c.session)
		}
		if got := c.cfg.IsJWT(); got != c.jwt {
			t.Errorf("%s: IsJWT = %t, want %t", c.name, got, c.jwt)
		}
	}
}

// Only two modes exist, and they are matched exactly: "JWT" is a typo in
// a manifest, not a synonym, and silently treating it as some other mode
// would switch an API's authentication scheme without telling anyone.
// "none" is refused too, because no authentication is written by leaving
// the key out, and the error says so.
func TestAuthValidate(t *testing.T) {
	t.Parallel()
	runRules(t, map[string]any{"fiber.auth.mode": "session"}, buildAuth,
		[]ruleCase{
			{"jwt", map[string]any{"fiber.auth.mode": "jwt"}, ""},
			{"absent", map[string]any{"fiber.auth.mode": nil}, ""},
			{"empty", map[string]any{"fiber.auth.mode": ""}, ""},
			{"upper-case JWT", map[string]any{
				"fiber.auth.mode": "JWT"}, "fiber.auth.mode"},
			{"a padded mode", map[string]any{
				"fiber.auth.mode": " jwt"}, "fiber.auth.mode"},
			{"none", map[string]any{
				"fiber.auth.mode": "none"}, "leave it unset"},
			{"an unknown mode", map[string]any{
				"fiber.auth.mode": "oauth"}, "fiber.auth.mode"},
		})
}

// An empty mode is logged in words, so the startup line cannot be read as
// a mode that failed to print.
func TestAuthString(t *testing.T) {
	t.Parallel()
	for mode, want := range map[string]string{
		"":              "Mode=(none)",
		AuthModeSession: "Mode=session",
		AuthModeJWT:     "Mode=jwt",
	} {
		if got := (&AuthConfig{Mode: mode}).String(); got != want {
			t.Errorf("String with Mode %q = %q, want %q", mode, got, want)
		}
	}
}
