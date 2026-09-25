package config

// The fiber.auth.* section of config.yaml.
//
// Keys in this section, with their environment-variable spellings:
//
//	fiber.auth.mode
//		FIBER_AUTH_MODE
//
// One key, and that is the whole section — NewAuthConfig below reads exactly
// that one. The environment spelling holds only for a Viper built by NewViper;
// see the package documentation.

import (
	"errors"
	"fmt"

	"github.com/spf13/viper"
)

// AuthConfig selects which authentication mechanism a service uses.
//
// Setting fiber.auth.mode switches between the two at deployment time
// without touching code. A service is expected to validate only the
// sub-section matching the active mode — SessionConfig for "session",
// JWTConfig for "jwt" — and skip the other, which IsJWT exists to decide.
//
// Supported modes:
//   - "session" — a server-side session. The session ID travels in an
//     HttpOnly cookie and is looked up in the store on every request. Suits
//     a same-origin single-page application, where the frontend and backend
//     share a domain and there is no CORS or client-side token handling.
//   - "jwt" — a stateless HMAC-SHA256 signed bearer token, returned in the
//     login response and sent back as "Authorization: Bearer <token>". The
//     signature and expiry are checked on every request with no store
//     lookup. Suits mobile applications, third-party API clients, and a
//     frontend served from a different domain.
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
//
// It is the one branch a service needs: whether to register session
// middleware, which login and logout behaviour to wire, and which of the
// two auth sub-sections to validate.
//
// Nil-safe, and that is load-bearing rather than defensive habit. A service
// aggregating its sections typically calls this to BUILD its validation
// list, before that list has run — so a nil *AuthConfig must not panic here
// and lose the "not initialised" error the list is about to report. A nil
// receiver reports false, which selects session mode: the same default
// NewAuthConfig applies to an absent key.
func (c *AuthConfig) IsJWT() bool { return c != nil && c.Mode == "jwt" }

// Validate returns a joined error for every invalid or missing AuthConfig
// field.
//
// Deliberately unchecked:
//
//   - Whether the sub-section for the selected mode is USABLE. That is
//     JWTConfig.Validate's or SessionConfig.Validate's to answer, and a
//     service runs whichever IsJWT selects; neither is visible from here.
//   - Whether the service actually branched the same way. This value
//     selects the mode; nothing in this package can see the middleware
//     stack that honours it.
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
