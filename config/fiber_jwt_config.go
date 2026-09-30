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
// Two keys, and that is the whole section — NewJWTConfig below reads
// exactly these. The environment spelling holds only for a Viper built by
// NewViper; see the package documentation.
//
// The section is in use only while fiber.auth.mode is "jwt"; AuthConfig
// is the switch.

import (
	"errors"
	"fmt"
	"time"

	"github.com/spf13/viper"
)

// JWTConfig holds configuration for JWT-based authentication: the key
// tokens are signed with, and how long each token stays valid.
type JWTConfig struct {
	// Secret is the HMAC-SHA256 signing key shared between token issuance
	// (login) and token validation (every authenticated request). Anyone
	// who knows this value can forge valid tokens, so treat it with the
	// same care as a database password:
	//   - Never commit a real value to version control.
	//   - Inject it through the environment in production:
	//       FIBER_JWT_SECRET=<value> ./myapp
	//   - Generate a strong value with: openssl rand -hex 32
	//
	// Must be at least 32 BYTES. Validate measures with len(), which
	// counts bytes rather than characters. For the recommended hex or
	// base64 secrets the two are the same; a secret with multi-byte
	// characters reaches the floor in fewer than 32 of them. Validated at
	// startup.
	Secret string

	// Expiry is the time-to-live of an issued token. Once it passes, the
	// token is rejected and the client has to log in again for a fresh
	// one. Shorter is safer and longer is more convenient: "15m" (high
	// security) to "24h" (convenience) is the usual range.
	//
	// Go duration format: "15m", "1h", "8h", "24h". Required, and at least
	// one second. A bare number parses as NANOSECONDS, so 3600 meant as an
	// hour is 3.6µs, and every token would expire before its first use;
	// the one-second floor turns that into a startup error. Always write a
	// unit.
	Expiry time.Duration
}

// minJWTSecretBytes is the shortest fiber.jwt.secret Validate accepts.
//
// 32 bytes is the output size of SHA-256, and RFC 7518 requires an HS256
// key at least as long as the hash output: a shorter key caps the
// signature's strength at the key's own length.
const minJWTSecretBytes = 32

// NewJWTConfig reads JWT fields from the provided Viper instance.
//
// Never returns an error: absent keys and uncastable values both come back
// as zero values, which Validate rejects afterwards.
//
// This function is the authoritative list of keys the fiber.jwt.* section
// supports. A key present in config.yaml but missing here is dead weight —
// Viper never looks it up, so neither the file nor an environment variable
// can supply it.
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
// AuthConfig.IsJWT is the branch. Called in any other mode, it would
// refuse a deployment for leaving empty a secret that deployment never
// uses.
//
// Deliberately unchecked:
//
//   - Whether the secret is the SAME one the previous process used.
//     Rotation is invisible here — see the note below — and a config
//     package has nothing to compare against.
//   - Secret ENTROPY. len() counts bytes, so 32 repetitions of "a" passes
//     and is worth nothing. Nothing here can tell a generated secret from
//     a typed one, and a shape check (hex? base64?) would reject a
//     perfectly good passphrase. The length floor is the part that can be
//     mechanised; the "openssl rand -hex 32" line on the field is the part
//     that cannot.
//   - Expiry's upper bound. A 168h token is a real deployment choice for
//     an internal tool and a bad one for a public API, and this package
//     cannot tell which it is looking at. The recommended range is on the
//     field.
//
// Secret rotation note: changing the secret invalidates ALL existing
// tokens immediately — every currently logged-in user receives 401 on
// their next request and has to authenticate again. Plan rotations for
// low-traffic periods, or implement key versioning (e.g. a kid header)
// where rotation has to go unnoticed.
//
// Every check appends rather than returning early, so one restart surfaces
// every fiber.jwt.* problem at once.
func (c *JWTConfig) Validate() error {
	// errs is declared before the nil check only so the two statements
	// read in the same order in all Validate implementations; the nil
	// check is what must come first, since every line after it
	// dereferences c.
	var errs []error
	if c == nil {
		return errors.New("fiber.jwt config was not initialised")
	}

	// A switch rather than two ifs, so an absent key produces one error
	// rather than two: "" is caught as "required" and never also reported
	// as too short. AppConfig.Validate applies the same rule to app.port,
	// where 0 is the absent value.
	switch {
	case c.Secret == "":
		errs = append(errs, errors.New("fiber.jwt.secret is required when "+
			"fiber.auth.mode is \"jwt\""))
	case len(c.Secret) < minJWTSecretBytes:
		errs = append(errs, fmt.Errorf("fiber.jwt.secret must be at least "+
			"%d bytes (got %d)",
			minJWTSecretBytes,
			len(c.Secret)))
	}

	// The same shape for the expiry: absent is "required", and anything
	// else under a second is out of range. That range holds both mistakes
	// worth catching: a bare number, read as nanoseconds, and a negative
	// value, which would issue every token already expired.
	switch {
	case c.Expiry == 0:
		errs = append(errs, errors.New("fiber.jwt.expiry is required when "+
			"fiber.auth.mode is \"jwt\""))
	case c.Expiry < time.Second:
		errs = append(errs, fmt.Errorf("fiber.jwt.expiry must be at least "+
			"1s (got %s): a shorter token expires before the client can "+
			"use it, and if this was meant as seconds, write the unit — a "+
			"bare number is nanoseconds",
			c.Expiry))
	}

	return errors.Join(errs...)
}

// String returns a loggable representation of JWTConfig.
//
// The pointer receiver means fmt only picks this up for a *JWTConfig.
// Printing a value copy (%v on JWTConfig, not &JWTConfig) bypasses it and
// dumps the struct fields directly, the secret included.
//
// The secret is reported by LENGTH only. That is enough to tell "unset"
// from "set" in a startup log, and to spot a value that was cut short on
// its way into the environment, without printing any of it.
func (c *JWTConfig) String() string {
	if c == nil {
		return "<nil JWTConfig>"
	}
	return fmt.Sprintf("Expiry=%s "+
		"SecretLen=%d",
		c.Expiry,
		len(c.Secret))
}
