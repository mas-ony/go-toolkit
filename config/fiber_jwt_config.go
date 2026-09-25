package config

// The fiber.jwt.* section of config.yaml.
//
// Keys in this section, with their environment-variable spellings:
//
//	fiber.jwt.expiry
//		FIBER_JWT_EXPIRY
//	fiber.jwt.secret
//		FIBER_JWT_SECRET
//
// Two keys, and that is the whole section — NewJWTConfig below reads exactly
// these. The environment spelling holds only for a Viper built by NewViper;
// see the package documentation.

import (
	"errors"
	"fmt"
	"time"

	"github.com/spf13/viper"
)

// JWTConfig holds configuration for JWT-based authentication.
type JWTConfig struct {
	// Secret is the HMAC-SHA256 signing key shared between token issuance
	// (login) and token validation (every authenticated request). Anyone who
	// knows this value can forge valid tokens, so treat it with the same care
	// as a database password:
	//   - Never commit a real value to version control.
	//   - Inject via environment variable in production:
	//       FIBER_JWT_SECRET=<value> ./myapp
	//   - Generate a strong value with: openssl rand -hex 32
	// Must be at least 32 bytes (Validate uses len(), which counts bytes not
	// Unicode code points). For ASCII secrets — the recommended hex or base64
	// strings — bytes and characters are equivalent, so "32 characters" is
	// accurate in practice. Validated at startup.
	Secret string

	// Expiry is the time-to-live for issued tokens. After this duration the
	// token is rejected and the client must re-authenticate to obtain a
	// fresh token.
	// Shorter expiries improve security; longer expiries improve UX.
	// Recommended range: "15m" (high security) to "24h" (convenience).
	// Go duration format: "15m", "1h", "8h", "24h". Must be non-zero.
	Expiry time.Duration
}

// NewJWTConfig reads JWT fields from the provided Viper instance.
//
// Never returns an error: absent keys and uncastable values both come back as
// zero values, which Validate rejects afterwards.
//
// This function is the authoritative list of keys the fiber.jwt.* section
// supports. A key present in config.yaml but missing here is dead weight —
// Viper never looks it up, so neither the file nor an environment variable can
// supply it.
func NewJWTConfig(v *viper.Viper) *JWTConfig {
	return &JWTConfig{
		Secret: v.GetString("fiber.jwt.secret"),
		Expiry: v.GetDuration("fiber.jwt.expiry"),
	}
}

// Validate returns a joined error for every invalid or missing JWTConfig
// field.
//
// A service is expected to call it only when fiber.auth.mode is "jwt" —
// AuthConfig.IsJWT is the branch. Called in session mode, it would refuse
// a deployment for leaving empty a secret that deployment never uses.
//
// Deliberately unchecked:
//
//   - Whether the secret is the SAME one the previous process used. Rotation
//     is invisible here — see the note below — and a config package has
//     nothing to compare against.
//   - Secret ENTROPY. len() counts bytes, so 32 repetitions of "a" passes and
//     is worth nothing. Nothing here can tell a generated secret from a typed
//     one, and a shape check (hex? base64?) would reject a perfectly good
//     passphrase. The length floor is the part that can be mechanised; the
//     "openssl rand -hex 32" line on the field is the part that cannot.
//   - Expiry's upper bound. A 168h token is a real deployment choice for an
//     internal tool and a bad one for a public API, and this package cannot
//     tell which it is looking at. The recommended range is on the field.
//
// Secret rotation note: changing the secret invalidates ALL existing tokens
// immediately — every currently logged-in user will receive 401 on their next
// request and must re-authenticate. Plan rotations during low-traffic periods
// or implement key versioning (e.g. kid header) if zero-downtime rotation is
// required.
//
// Every check appends rather than returning early, so one restart surfaces
// every fiber.jwt.* problem at once.
func (c *JWTConfig) Validate() error {
	// errs is declared before the nil check only so the two statements read in
	// the same order in all Validate implementations; the nil check is what
	// must come first, since every line after it dereferences c.
	var errs []error
	if c == nil {
		return errors.New("fiber.jwt config was not initialised")
	}
	// A chained else-if, so an absent key produces one error rather than two:
	// "" is caught as "required" and never also reported as too short. This is
	// the same rule AppConfig.Validate applies to app.port, where 0 is the
	// absent value and would otherwise be reported twice as well.
	if c.Secret == "" {
		errs = append(errs, errors.New("fiber.jwt.secret is required when "+
			"fiber.auth.mode is \"jwt\""))
	} else if len(c.Secret) < 32 {
		errs = append(errs, fmt.Errorf("fiber.jwt.secret must be at least "+
			"32 characters (got %d)",
			len(c.Secret)))
	}
	if c.Expiry == 0 {
		errs = append(errs, errors.New("fiber.jwt.expiry is required when "+
			"fiber.auth.mode is \"jwt\""))
	}

	return errors.Join(errs...)
}

// String returns a loggable representation of JWTConfig.
//
// The pointer receiver means fmt only picks this up for a *JWTConfig.
// Printing a value copy (%v on JWTConfig, not &JWTConfig) bypasses it and
// dumps the struct fields directly.
func (c *JWTConfig) String() string {
	if c == nil {
		return "<nil JWTConfig>"
	}
	return fmt.Sprintf("Expiry=%s "+
		"SecretLen=%d",
		c.Expiry,
		len(c.Secret))
}
