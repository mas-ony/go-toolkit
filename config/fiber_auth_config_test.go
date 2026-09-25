package config

// Tests for fiber_auth_config.go.

import (
	"testing"

	"github.com/spf13/viper"
)

// Nil-safe, and that is load-bearing: a service calls IsJWT to BUILD its
// validation list, before that list reports a nil section.
func TestIsJWT(t *testing.T) {
	t.Parallel()
	var none *AuthConfig
	if none.IsJWT() {
		t.Error("a nil AuthConfig reported jwt")
	}
	if !(&AuthConfig{Mode: "jwt"}).IsJWT() {
		t.Error(`Mode "jwt" did not report jwt`)
	}
	if (&AuthConfig{Mode: "session"}).IsJWT() {
		t.Error(`Mode "session" reported jwt`)
	}
	// An absent key selects session mode, not an error.
	if got := NewAuthConfig(viper.New()).Mode; got != "session" {
		t.Errorf("absent mode = %q, want session", got)
	}
}

func buildAuth(v *viper.Viper) SectionConfig { return NewAuthConfig(v) }

// Only two modes exist, and they are matched exactly: "JWT" is a typo in
// a manifest, not a synonym, and silently treating it as session mode
// would switch an API's authentication scheme without telling anyone.
func TestAuthValidate(t *testing.T) {
	t.Parallel()
	runRules(t, map[string]any{"fiber.auth.mode": "session"}, buildAuth,
		[]ruleCase{
			{"jwt", map[string]any{"fiber.auth.mode": "jwt"}, ""},
			// Absent is not an error — it selects session mode, which the
			// constructor does before Validate ever runs.
			{"absent", map[string]any{"fiber.auth.mode": nil}, ""},
			{"upper-case JWT", map[string]any{
				"fiber.auth.mode": "JWT"}, "fiber.auth.mode"},
			{"an unknown mode", map[string]any{
				"fiber.auth.mode": "oauth"}, "fiber.auth.mode"},
		})
}
