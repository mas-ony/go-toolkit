// fiber_auth_config.go covers the fiber.auth.* section of config.yaml.
//
// Keys in this section, with their environment-variable spellings:
//
//	fiber.auth.mode
//		FIBER_AUTH_MODE
//
// One key, and that is the whole section — NewAuthConfig below reads exactly
// that one. See doc.go for how the environment spelling is derived and which
// tests hold it up.

package config

import (
	"errors"
	"fmt"

	"github.com/spf13/viper"
)

// AuthConfig controls which authentication mechanism the application uses.
//
// Set fiber.auth.mode in config.yaml to switch between modes at deployment
// time without touching any code. Only the sub-config that corresponds to the
// active mode is validated at startup (SessionConfig for "session", JWTConfig
// for "jwt"); the other is silently skipped.
//
// Supported modes:
//   - "session" — server-side session stored in memory. The session ID is sent
//     to the client as an HttpOnly cookie and validated on every request by
//     looking it up in the store. The session middleware is registered
//     globally. Best for same-origin single-page apps where the frontend and
//     backend share the same domain (no CORS complications, no client-side
//     token logic).
//   - "jwt" — stateless HMAC-SHA256 signed Bearer token. The signed token is
//     returned in the login response body; clients send it as:
//     Authorization: Bearer <token>
//     The server validates the signature and expiry on every request with no
//     session-store lookup. The session middleware is NOT registered.
//     Best for mobile apps, third-party API clients, or deployments where the
//     frontend is served from a different domain.
type AuthConfig struct {
	// Mode is the active authentication mechanism. Validated against the set
	// of known modes at startup so typos in config.yaml are caught immediately
	// rather than causing a silent fallback.
	//
	// Allowed values: "session" | "jwt"
	Mode string
}

// NewAuthConfig reads AuthConfig fields from the provided Viper instance.
//
// Never returns an error: absent keys and uncastable values both come back as
// zero values, which Validate rejects afterwards.
//
// This function is the authoritative list of keys the fiber.auth.* section
// supports. A key present in config.yaml but missing here is dead weight —
// Viper never looks it up, so neither the file nor an environment variable can
// supply it.
//
// An absent or empty fiber.auth.mode selects "session", so the whole section
// can be omitted. Validate therefore never sees an empty Mode — see the note
// there for what that omission costs.
func NewAuthConfig(v *viper.Viper) *AuthConfig {
	mode := v.GetString("fiber.auth.mode")
	if mode == "" {
		mode = "session"
	}
	return &AuthConfig{Mode: mode}
}

// IsJWT reports whether the active auth mode is "jwt".
// Used by router.New and handler constructors to conditionally register
// the session middleware and select the correct login/logout behaviour.
//
// IsJWT is also used by Config.validate to decide which auth sub-config
// (JWTConfig vs SessionConfig) to pass to its validation loop — only the
// active sub-config is validated at startup.
//
// Nil-safe, and that is load-bearing rather than defensive habit:
// Config.validate calls this BEFORE the validation loop runs, so a nil
// *AuthConfig would panic here and lose the "not initialised" error that the
// loop is about to produce. A nil receiver reports false, which selects
// session mode — the same default NewAuthConfig applies to an absent key.
func (c *AuthConfig) IsJWT() bool { return c != nil && c.Mode == "jwt" }

// Validate returns a joined error for every invalid or missing AuthConfig
// field.
//
// Deliberately unchecked:
//
//   - Whether the sub-config for the selected mode is USABLE. Config.validate
//     runs exactly one of JWTConfig.Validate and SessionConfig.Validate based
//     on IsJWT(), and neither is visible from here.
//   - Whether router.New actually branched the same way. This value selects
//     the mode; nothing in this package can see the middleware stack that
//     honours it.
//   - An EMPTY Mode. NewAuthConfig substitutes "session" for it before this
//     method ever runs, so "" is unreachable here and an error message naming
//     it would be dead text. The consequence is worth stating plainly:
//     deleting the fiber.auth section is not a configuration error, it is a
//     silent selection of session mode.
//
// Every check appends rather than returning early, so one restart surfaces
// every fiber.auth.* problem at once.
func (c *AuthConfig) Validate() error {
	// errs is declared before the nil check only so the two statements read in
	// the same order in all Validate implementations; the nil check is what
	// must come first, since every line after it dereferences c.
	var errs []error
	if c == nil {
		return errors.New("fiber.auth config was not initialised")
	}

	switch c.Mode {
	case "session", "jwt":
	default:
		errs = append(errs, fmt.Errorf("fiber.auth.mode %q is not supported; "+
			"choose \"session\" or \"jwt\"",
			c.Mode))
	}

	return errors.Join(errs...)
}

// String returns a loggable representation of AuthConfig.
//
// The pointer receiver means fmt only picks this up for a *AuthConfig.
// Printing a value copy (%v on AuthConfig, not &AuthConfig) bypasses it and
// dumps the struct fields directly.
func (c *AuthConfig) String() string {
	if c == nil {
		return "<nil AuthConfig>"
	}
	return fmt.Sprintf(
		"Mode=%s",
		c.Mode,
	)
}
