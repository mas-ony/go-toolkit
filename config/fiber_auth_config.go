package config

// The fiber.auth.* section of config.yaml.
//
// Keys in this section, with their environment-variable spellings:
//
//	fiber.auth.mode
//		FIBER_AUTH_MODE
//
// One key, and that is the whole section — NewAuthConfig below reads
// exactly that one. The environment spelling holds only for a Viper built
// by NewViper; see the package documentation.

import (
	"errors"
	"fmt"

	"github.com/spf13/viper"
)

// AuthConfig selects which authentication mechanism a service uses, if
// any.
//
// It is a switch rather than a configuration. Each mechanism keeps its
// settings in a section of its own, fiber.session.* (SessionConfig) and
// fiber.jwt.* (JWTConfig), and this section decides which of them is in
// use, so moving a deployment from one to the other touches no code.
//
// Mode has three states, and a service has to branch on all three:
//
//	switch {
//	case cfg.Auth.IsSession():
//		// register the session middleware; validate SessionConfig
//	case cfg.Auth.IsJWT():
//		// register the JWT middleware; validate JWTConfig
//	default:
//		// no authentication: register neither, validate neither
//	}
//
// Only the section the active mode selects is validated. Validating the
// other would refuse a deployment for leaving empty a secret, or a
// timeout, that it never uses.
type AuthConfig struct {
	// Mode is the active authentication mechanism: AuthModeSession,
	// AuthModeJWT, or empty for none.
	//
	// Empty is what an absent key reads as, and it is a setting rather
	// than a mistake: it selects no authentication. So a service that has
	// routes to protect should test for it where it wires its middleware
	// and refuse to start, rather than serve those routes unguarded; only
	// the service knows whether it has any.
	//
	// Matched EXACTLY, so "JWT" and " jwt" are refused rather than guessed
	// at, and so is "none": String prints an empty mode as "(none)", which
	// is a description and not a value.
	//
	// The environment cannot switch authentication off once config.yaml
	// sets a mode. An empty FIBER_AUTH_MODE is not a value — NewViper
	// leaves AllowEmptyEnv off — so it leaves the file's mode in place;
	// delete the key from the file instead.
	//
	// Allowed values: "session" | "jwt" | "" (none)
	Mode string
}

// The values fiber.auth.mode accepts. Compare against these, or better
// through IsSession and IsJWT, rather than against bare literals.
//
// There is deliberately no constant for "no authentication". It is the
// ABSENCE of a mode, written by leaving the key out, rather than a third
// word an operator could misspell into one of the other two.
const (
	// AuthModeSession selects a server-side session. The session ID
	// travels in a cookie and is looked up in the store on every request.
	// It suits a single-page application served from the same domain as
	// the API, where there is no CORS to configure and no token for
	// client-side code to hold. Configured by SessionConfig.
	AuthModeSession = "session"

	// AuthModeJWT selects a stateless HMAC-SHA256 signed bearer token,
	// returned by the login response and sent back as
	// "Authorization: Bearer <token>". Its signature and expiry are
	// checked on every request with no store lookup. It suits mobile
	// applications, third-party API clients, and a frontend served from
	// another domain. Configured by JWTConfig.
	AuthModeJWT = "jwt"
)

// NewAuthConfig reads AuthConfig fields from the provided Viper instance.
//
// Never returns an error: an absent key comes back as an empty Mode, which
// is a valid setting, and a value that names no mode is left for Validate
// to reject.
//
// This function is the authoritative list of keys the fiber.auth.* section
// supports. A key present in config.yaml but missing here is dead weight —
// Viper never looks it up, so neither the file nor an environment variable
// can supply it.
//
// The value is read verbatim, neither trimmed nor lowercased, because
// Validate matches it exactly: a misspelling is reported by name instead
// of being guessed into a mode nobody wrote.
func NewAuthConfig(v *viper.Viper) *AuthConfig {
	return &AuthConfig{Mode: v.GetString("fiber.auth.mode")}
}

// IsSession reports whether the active mode is AuthModeSession: whether to
// register the session middleware, wire the session login and logout
// handlers, and validate SessionConfig.
//
// Nil-safe, and that is load-bearing rather than defensive habit. A
// service aggregating its sections typically calls this to BUILD its
// validation list, before that list has run, so a nil *AuthConfig must not
// panic here and lose the "not initialised" error the list is about to
// report. A nil receiver reports false, as an empty Mode does.
//
// An unrecognised mode reports false as well. Validate refuses it, so a
// service that validates before it wires never branches on one.
func (c *AuthConfig) IsSession() bool {
	return c != nil && c.Mode == AuthModeSession
}

// IsJWT reports whether the active mode is AuthModeJWT: whether to register
// the JWT middleware, wire the token-issuing login handler, and validate
// JWTConfig. Nil-safe, for the reason IsSession gives.
func (c *AuthConfig) IsJWT() bool {
	return c != nil && c.Mode == AuthModeJWT
}

// Validate returns a joined error for every invalid AuthConfig field.
//
// An empty Mode passes, because no authentication is a setting rather than
// an omission. Any other value has to be one of the two modes, spelled
// exactly.
//
// Deliberately unchecked:
//
//   - Whether the section the mode selects is USABLE. That is
//     SessionConfig.Validate's or JWTConfig.Validate's to answer, and a
//     service runs whichever IsSession or IsJWT selects; neither is
//     visible from here.
//   - Whether the service branched the same way. This value selects the
//     mode; nothing in this package can see the middleware stack that
//     honours it.
//   - Whether the service NEEDS authentication. Only the service knows
//     whether it has routes to protect, so only it can refuse an empty
//     Mode; see the field.
//
// Every check appends rather than returning early, so one restart surfaces
// every fiber.auth.* problem at once.
func (c *AuthConfig) Validate() error {
	// errs is declared before the nil check only so the two statements
	// read in the same order in all Validate implementations; the nil
	// check is what must come first, since every line after it
	// dereferences c.
	var errs []error
	if c == nil {
		return errors.New("fiber.auth config was not initialised")
	}

	switch c.Mode {
	case "", AuthModeSession, AuthModeJWT:
		// No authentication, or one of the two mechanisms.
	default:
		errs = append(errs, fmt.Errorf("fiber.auth.mode %q is not "+
			"supported: choose %q or %q, or leave it unset for no "+
			"authentication",
			c.Mode,
			AuthModeSession,
			AuthModeJWT))
	}

	return errors.Join(errs...)
}

// String returns a loggable representation of AuthConfig.
//
// The pointer receiver means fmt only picks this up for a *AuthConfig.
// Printing a value copy (%v on AuthConfig, not &AuthConfig) bypasses it and
// dumps the struct fields directly.
//
// An empty Mode prints as "(none)" rather than as nothing, so the startup
// line says in words that no authentication was selected instead of
// looking like a value that failed to print. The parentheses mark it as a
// description: "none" is not a value Validate accepts.
func (c *AuthConfig) String() string {
	if c == nil {
		return "<nil AuthConfig>"
	}
	mode := c.Mode
	if mode == "" {
		mode = "(none)"
	}

	return fmt.Sprintf(
		"Mode=%s",
		mode,
	)
}
