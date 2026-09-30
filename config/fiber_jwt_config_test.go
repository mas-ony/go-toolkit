package config

// Tests for fiber_jwt_config.go.
//
// What the section promises: a secret of at least 32 bytes, an expiry of
// at least a second, one error per absent key, and a log line that shows
// the secret's length and never the secret.

import (
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
)

// jwtSecret is a 64-character hex string, the shape "openssl rand -hex 32"
// produces.
const jwtSecret = "0123456789abcdef0123456789abcdef" +
	"0123456789abcdef0123456789abcdef"

// jwtBase is a configuration the JWT section accepts, which each rule case
// changes one key at a time.
var jwtBase = map[string]any{
	"fiber.jwt.secret": jwtSecret,
	"fiber.jwt.expiry": "15m",
}

// buildJWT adapts NewJWTConfig to the constructor shape runRules takes.
func buildJWT(v *viper.Viper) SectionConfig { return NewJWTConfig(v) }

// TestJWTValidate holds the section's rules: both keys required, the
// secret at least 32 bytes, and the expiry at least a second, which is
// where a bare number and a negative value both land.
func TestJWTValidate(t *testing.T) {
	t.Parallel()
	runRules(t, jwtBase, buildJWT, []ruleCase{
		{"no secret", map[string]any{
			"fiber.jwt.secret": nil}, "fiber.jwt.secret"},
		{"a 31-byte secret", map[string]any{
			"fiber.jwt.secret": strings.Repeat("a", 31)},
			"at least 32 bytes"},
		{"exactly 32 bytes is enough", map[string]any{
			"fiber.jwt.secret": strings.Repeat("a", 32)}, ""},
		{"no expiry", map[string]any{
			"fiber.jwt.expiry": nil}, "fiber.jwt.expiry is required"},
		// 3600 meant as an hour is 3.6µs.
		{"a bare-number expiry", map[string]any{
			"fiber.jwt.expiry": 3600}, "at least 1s"},
		{"a negative expiry", map[string]any{
			"fiber.jwt.expiry": "-15m"}, "at least 1s"},
		{"exactly one second is enough", map[string]any{
			"fiber.jwt.expiry": "1s"}, ""},
	})
}

// The floor is measured in BYTES, which is what len() counts and what an
// HMAC key is made of. A secret of eleven three-byte characters is 33
// bytes and passes, though it is only eleven characters long; ten of them
// are 30 bytes and do not. The error message says "bytes" for that reason.
func TestJWTSecretLengthCountsBytes(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		secret string
		ok     bool
	}{
		{strings.Repeat("あ", 11), true},
		{strings.Repeat("あ", 10), false},
	} {
		err := (&JWTConfig{Secret: c.secret, Expiry: time.Hour}).Validate()
		if (err == nil) != c.ok {
			t.Errorf("%d bytes in %d characters: err = %v, want ok=%v",
				len(c.secret), len([]rune(c.secret)), err, c.ok)
		}
	}
}

// An absent secret is reported as "required" and not ALSO as too short,
// which is what the switch in Validate is for. The same holds for an
// absent expiry.
func TestJWTAbsentKeysAreReportedOnce(t *testing.T) {
	t.Parallel()
	err := (&JWTConfig{}).Validate()
	if err == nil {
		t.Fatal("an empty section validated")
	}
	for _, twice := range []string{"at least 32", "at least 1s"} {
		if strings.Contains(err.Error(), twice) {
			t.Errorf("an absent key was reported twice (%q):\n%v", twice,
				err)
		}
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
	if strings.Contains(got, jwtSecret[:12]) {
		t.Errorf("String prints the secret: %s", got)
	}
}
