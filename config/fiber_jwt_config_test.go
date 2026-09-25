package config

// Tests for fiber_jwt_config.go.

import (
	"strings"
	"testing"

	"github.com/spf13/viper"
)

// A 64-character hex string, the shape "openssl rand -hex 32" produces.
const jwtSecret = "0123456789abcdef0123456789abcdef" +
	"0123456789abcdef0123456789abcdef"

var jwtBase = map[string]any{
	"fiber.jwt.secret": jwtSecret,
	"fiber.jwt.expiry": "15m",
}

func buildJWT(v *viper.Viper) SectionConfig { return NewJWTConfig(v) }

func TestJWTValidate(t *testing.T) {
	t.Parallel()
	runRules(t, jwtBase, buildJWT, []ruleCase{
		{"no secret", map[string]any{
			"fiber.jwt.secret": nil}, "fiber.jwt.secret"},
		{"a 31-byte secret", map[string]any{
			"fiber.jwt.secret": strings.Repeat("a", 31)}, "at least 32"},
		{"exactly 32 bytes is enough", map[string]any{
			"fiber.jwt.secret": strings.Repeat("a", 32)}, ""},
		{"no expiry", map[string]any{
			"fiber.jwt.expiry": nil}, "fiber.jwt.expiry"},
	})
}

// The floor is measured in BYTES, which is what len() counts and what an
// HMAC key is made of. A secret of eleven three-byte characters is 33
// bytes and passes, though it is only eleven characters long; ten of them
// are 30 bytes and do not. For the recommended hex and base64 secrets the
// two measures agree, which is why the error message can say "characters".
func TestJWTSecretLengthCountsBytes(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		secret string
		ok     bool
	}{
		{strings.Repeat("あ", 11), true},
		{strings.Repeat("あ", 10), false},
	} {
		err := (&JWTConfig{Secret: c.secret, Expiry: 1}).Validate()
		if (err == nil) != c.ok {
			t.Errorf("%d bytes in %d characters: err = %v, want ok=%v",
				len(c.secret), len([]rune(c.secret)), err, c.ok)
		}
	}
}

// An absent secret is reported as "required" and not ALSO as too short,
// which is what the chained else-if in Validate is for.
func TestJWTEmptySecretIsReportedOnce(t *testing.T) {
	t.Parallel()
	err := (&JWTConfig{Expiry: 1}).Validate()
	if err == nil {
		t.Fatal("an empty secret validated")
	}
	if strings.Contains(err.Error(), "at least 32") {
		t.Errorf("an empty secret was reported twice:\n%v", err)
	}
}

// String reports the secret's length, which is enough to tell "unset"
// from "set" in a startup log, and never the secret itself.
func TestJWTStringShowsOnlyTheLength(t *testing.T) {
	t.Parallel()
	got := NewJWTConfig(withKeys(jwtBase, nil)).String()
	if !strings.Contains(got, "SecretLen=64") {
		t.Errorf("String = %q, want SecretLen=64", got)
	}
}
