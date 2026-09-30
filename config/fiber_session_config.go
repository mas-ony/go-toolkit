package config

// The fiber.session.* section of config.yaml.
//
// Keys in this section, with their environment-variable spellings:
//
//	fiber.session.absolute_timeout
//		FIBER_SESSION_ABSOLUTE_TIMEOUT
//	fiber.session.cookie_domain
//		FIBER_SESSION_COOKIE_DOMAIN
//	fiber.session.cookie_http_only
//		FIBER_SESSION_COOKIE_HTTP_ONLY
//	fiber.session.cookie_path
//		FIBER_SESSION_COOKIE_PATH
//	fiber.session.cookie_same_site
//		FIBER_SESSION_COOKIE_SAME_SITE
//	fiber.session.cookie_secure
//		FIBER_SESSION_COOKIE_SECURE
//	fiber.session.cookie_session_only
//		FIBER_SESSION_COOKIE_SESSION_ONLY
//	fiber.session.idle_timeout
//		FIBER_SESSION_IDLE_TIMEOUT
//
// Eight keys, and that is the whole section — NewSessionConfig below reads
// exactly these. The environment spelling holds only for a Viper built by
// NewViper; see the package documentation.
//
// The section is in use only while fiber.auth.mode is "session";
// AuthConfig is the switch.

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3/extractors"
	"github.com/gofiber/fiber/v3/middleware/session"
	"github.com/spf13/viper"
)

// SessionConfig wraps session.Config so it takes part in the same
// Validate/String lifecycle as every other section.
//
// A service uses and validates it only while fiber.auth.mode is "session"
// (AuthConfig.IsSession). In "jwt" mode, and with no mode at all, the
// session middleware is not registered and this section is not consulted.
//
// In-memory store note: session.NewStore substitutes memory.New() for a
// nil Storage, so the store is per-PROCESS and shared with nothing. When
// EnablePrefork is true (fiber_listen_config.go), each worker has its own
// isolated store, and a request landing on a different worker than the
// one that issued the cookie appears unauthenticated — intermittently, and
// only under load, which is the worst shape of bug to diagnose. Either
// disable prefork or install a shared Storage (e.g. Redis) before enabling
// both prefork and session auth at once. Nothing here can check that
// pairing — it needs ListenConfig — so the application's cross-section
// validation has to reject it instead.
//
// The same store is why a restart logs every user out, and why replicas
// behind a load balancer need sticky sessions or a shared Storage.
type SessionConfig struct {
	// Embedded as a POINTER, so the wrapper and the embedded struct can be
	// nil independently — hence the two-part nil checks in Validate and
	// String. Embedding also promotes the fields (c.IdleTimeout rather
	// than c.Config.IdleTimeout), which is why a nil embedded pointer
	// panics on field access rather than at the method call.
	*session.Config
}

// sessionCookieName is the name the session cookie is served under when
// nothing overrides it, and it is NOT one of the eight keys above.
//
// The name lives on session.Config.Extractor, whose default is
// extractors.FromCookie("session_id"), and that field is a struct carrying
// a func — nothing YAML can express, so NewSessionConfig leaves it zero
// and the middleware's configDefault substitutes the default. Every cookie
// ATTRIBUTE is configurable here; the cookie NAME is not.
//
// String does not print it unconditionally: it derives the name from the
// Extractor, and reaches for this constant only when that field is zero,
// which is exactly when the middleware substitutes its own default. See
// the derivation note on String.
//
// The middleware spells that default separately, in session.ConfigDefault,
// and nothing in the compiler ties the two spellings together, so
// TestSessionStringNamesTheCookie holds them equal. A Fiber release that
// renamed the cookie would fail that test instead of leaving the startup
// line naming a cookie nobody sets.
const sessionCookieName = "session_id"

// validSameSite is the allowlist checked by SessionConfig.Validate, in the
// canonical spelling its error message lists.
//
// Matched case-INSENSITIVELY, mirroring the utils.EqualFold comparison in
// the middleware's setCookieAttributes: rejecting "strict" here would
// reject a value the middleware honours exactly as written.
var validSameSite = []string{"Strict", "Lax", "None"}

// containsFold reports whether s equals any element of list under Unicode
// case folding.
//
// Local to this file because validSameSite is its only caller: the one
// allowlist in this package whose canonical spellings are mixed-case, and
// so the one compared with EqualFold. The others normalise the value's
// case and look it up in a map, or match it exactly.
func containsFold(list []string, s string) bool {
	for _, v := range list {
		if strings.EqualFold(v, s) {
			return true
		}
	}
	return false
}

// NewSessionConfig reads session fields from the provided Viper instance.
//
// Never returns an error: absent keys and uncastable values both come back
// as zero values, which Validate rejects afterwards.
//
// This function is the authoritative list of keys the fiber.session.*
// section supports. A key present in config.yaml but missing here is dead
// weight — Viper never looks it up, so neither the file nor an environment
// variable can supply it.
//
// # The six fields NOT read here
//
// session.Config has fourteen exported fields and this constructor
// populates eight. The remaining six are absent for the same reason: none
// can be expressed in YAML at any spelling. Storage and Store are an
// interface and a pointer to live objects; Next, ErrorHandler, and
// KeyGenerator are funcs; Extractor is a struct carrying one. No scalar,
// sequence, or mapping could carry any of them.
//
// The middleware supplies a working default for all six, and two of those
// defaults are worth knowing rather than just noting:
//
//   - A nil Storage means session.NewStore substitutes memory.New() — the
//     per-process store described on the struct above.
//   - A zero Extractor means configDefault substitutes
//     extractors.FromCookie("session_id"), which is where the cookie NAME
//     comes from. See sessionCookieName.
//
// The other four are unremarkable: session.New builds a missing Store
// with NewStore, a nil Next is never consulted so the middleware never
// skips, a nil ErrorHandler falls back to DefaultErrorHandler (a log line
// and a 500), and a nil KeyGenerator becomes utils.SecureToken. Changing
// any of the six means editing this file, or installing the value on a
// copy at the application's call site the way
// RecoverConfig.WithStackTraceHandler does.
func NewSessionConfig(v *viper.Viper) *SessionConfig {
	return &SessionConfig{
		Config: &session.Config{
			// Hard upper bound on session lifetime regardless of
			// activity. After this duration the session is destroyed
			// even if the user has been continuously active.
			//
			// Zero is legal to the MIDDLEWARE — it means "no absolute
			// timeout, expire on idle alone" — and Validate rejects it
			// anyway, along with a negative value, which the middleware
			// reads the same way. That is a policy stricter than
			// Fiber's, not a repair of it: a session that renews forever
			// on activity is a credential with no expiry, and this
			// package wants a hard ceiling. Relaxing it is a change to
			// Validate alone.
			AbsoluteTimeout: v.GetDuration(
				"fiber.session.absolute_timeout"),

			// Domain attribute of the Set-Cookie header. Empty uses the
			// request host (same-host cookies only). A parent domain
			// (e.g. ".example.com") shares sessions across its
			// subdomains.
			CookieDomain: v.GetString("fiber.session.cookie_domain"),

			// HttpOnly flag on the session cookie. When true, JavaScript
			// cannot read the cookie through document.cookie, which
			// mitigates XSS-based session hijacking. Strongly
			// recommended in production.
			//
			// ABSENT KEY READS AS FALSE. Where no client-side script
			// reads this cookie — the browser attaches it by itself —
			// true costs nothing.
			CookieHTTPOnly: v.GetBool("fiber.session.cookie_http_only"),

			// Path attribute of the Set-Cookie header. Empty is served
			// as "/", because fasthttp gives every cookie path a leading
			// slash, which makes the cookie available on every path of
			// the domain.
			CookiePath: v.GetString("fiber.session.cookie_path"),

			// SameSite attribute of the session cookie. Controls when
			// the browser includes the cookie in cross-site requests:
			//   Strict — never sent cross-site (most secure, may break
			//            sign-in flows that arrive from another site).
			//   Lax    — sent on top-level navigations
			//            (recommended default).
			//   None   — always sent cross-site; forces Secure, see
			//            below.
			//
			// Matching in setCookieAttributes is utils.EqualFold against
			// "Strict" and "None", and EVERYTHING ELSE FALLS THROUGH TO
			// LAX. So "strict" works, and "Strcit" is silently Lax — a
			// downgrade with no error anywhere. Validate carries the
			// allowlist for that reason; the middleware has no way to
			// report it.
			CookieSameSite: v.GetString("fiber.session.cookie_same_site"),

			// Secure flag on the session cookie. When true, the browser
			// only sends the cookie over HTTPS connections. Must be true
			// in any production or public-facing environment.
			//
			// NOT the last word on the wire value. setCookieAttributes
			// forces Secure whenever SameSite resolves to None,
			// regardless of what is set here — so the pairing (None,
			// false) serves a Secure cookie while config.yaml and String
			// both report false. Validate rejects that pairing rather
			// than let the two disagree.
			CookieSecure: v.GetBool("fiber.session.cookie_secure"),

			// When true, the cookie carries no Max-Age or Expires
			// attribute and is deleted when the browser session ends.
			// The server-side session still expires through
			// AbsoluteTimeout and IdleTimeout — this flag affects only
			// the client-side cookie, and deleting the cookie does not
			// delete the session from the store.
			//
			// Practical effect: a user who closes and reopens the
			// browser loses the cookie, and with it the session, even if
			// the server-side session has not yet expired. That is the
			// safer choice where users share devices, though a browser
			// that restores its previous session on start keeps such
			// cookies anyway, so it is a convenience rather than a
			// guarantee. Leave it false for "remember me" behaviour,
			// where the cookie outlives the browser for IdleTimeout.
			//
			// ABSENT KEY READS AS FALSE.
			CookieSessionOnly: v.GetBool(
				"fiber.session.cookie_session_only"),

			// Inactivity timeout. If no request touches the session
			// within this window the session is destroyed and the user
			// has to log in again. It also sets the cookie's Max-Age,
			// unless CookieSessionOnly is true.
			//
			// Must be at least one second AND must not exceed
			// AbsoluteTimeout. The second half is not a style rule:
			// configDefault PANICS on AbsoluteTimeout > 0 &&
			// AbsoluteTimeout < IdleTimeout, and the application calls
			// session.New during startup, so the pairing takes the
			// process down with a runtime panic instead of a config
			// error. Validate catches it first.
			IdleTimeout: v.GetDuration("fiber.session.idle_timeout"),
		},
	}
}

// Validate returns a joined error for every invalid or missing
// SessionConfig field.
//
// A service is expected to call it only when fiber.auth.mode is "session"
// — AuthConfig.IsSession is the branch.
//
// Both timeouts are required and must be at least one second. The floor
// catches the two values the middleware does not refuse but reinterprets:
// a negative AbsoluteTimeout means no ceiling at all, and a negative
// IdleTimeout is replaced by the middleware's 30m default. It also catches
// a bare number, which Viper reads as nanoseconds.
//
// Deliberately unchecked:
//
//   - Whether the store is shared across processes. That is the prefork
//     pairing on the struct doc; it needs ListenConfig, which this method
//     never sees, so the application's cross-section validation holds it.
//   - Storage, Store, Next, ErrorHandler, KeyGenerator, and Extractor. All
//     are left unset here by design, none can be expressed in YAML, and the
//     middleware fills each in; NewSessionConfig lists what with.
//   - CookieDomain and CookiePath. Empty is the correct default for both —
//     the request host and "/" respectively — and any non-empty value is a
//     deployment decision this package cannot second-guess. A domain that
//     does not match the request host makes the browser drop the cookie,
//     but only the deployment knows which host that is.
//   - CookieHTTPOnly and CookieSessionOnly. Both settings are meaningful
//     and an absent key reads as false, which is the documented default.
//     What false costs is on the fields in NewSessionConfig, where it can
//     be read by someone about to change it.
//
// Every check appends rather than returning early, so one restart surfaces
// every fiber.session.* problem at once.
func (c *SessionConfig) Validate() error {
	// errs is declared before the nil check only so the two statements
	// read in the same order in all Validate implementations; the nil
	// check is what must come first, since every line after it
	// dereferences c.
	var errs []error
	if c == nil || c.Config == nil {
		return errors.New("fiber.session config was not initialised")
	}

	// Each timeout meets exactly one of its cases: zero is the absent
	// value and reported as required, a negative value is reported with
	// what the middleware would make of it, and a positive one under a
	// second as a probable missing unit.
	switch {
	case c.AbsoluteTimeout == 0:
		errs = append(errs, errors.New("fiber.session.absolute_timeout is "+
			"required: zero is legal to the middleware and means the "+
			"session expires on idle alone, so a continuously active "+
			"client holds one credential forever"))
	case c.AbsoluteTimeout < 0:
		errs = append(errs, fmt.Errorf("fiber.session.absolute_timeout "+
			"must be positive (got %s): the middleware treats a negative "+
			"value like zero, as no ceiling at all",
			c.AbsoluteTimeout))
	case c.AbsoluteTimeout < time.Second:
		errs = append(errs, fmt.Errorf("fiber.session.absolute_timeout "+
			"must be at least 1s (got %s): if this was meant as seconds, "+
			"write the unit — a bare number is nanoseconds",
			c.AbsoluteTimeout))
	}
	switch {
	case c.IdleTimeout == 0:
		errs = append(errs, errors.New("fiber.session.idle_timeout is "+
			"required: zero is replaced by the middleware's own 30m "+
			"default, which appears in neither config.yaml nor this "+
			"config's log line"))
	case c.IdleTimeout < 0:
		errs = append(errs, fmt.Errorf("fiber.session.idle_timeout must "+
			"be positive (got %s): the middleware replaces a negative "+
			"value with its own 30m default, which panics at startup "+
			"against any shorter absolute_timeout",
			c.IdleTimeout))
	case c.IdleTimeout < time.Second:
		errs = append(errs, fmt.Errorf("fiber.session.idle_timeout must "+
			"be at least 1s (got %s): if this was meant as seconds, write "+
			"the unit — a bare number is nanoseconds",
			c.IdleTimeout))
	}

	// Checked here rather than left to the middleware because the
	// middleware's answer is a panic. session.New runs configDefault on
	// the value the application passes it, and an AbsoluteTimeout below
	// IdleTimeout takes the process down at startup with "[session]
	// AbsoluteTimeout must be greater than or equal to IdleTimeout" — a
	// message that names neither config key and no section.
	//
	// The comparison runs only once both halves have passed their own
	// cases above, so an absent or out-of-range value reports itself
	// rather than a pairing it caused. That gate is also what makes this
	// check sufficient. configDefault replaces an IdleTimeout of zero or
	// less with its 30m default BEFORE it compares, so the pairing it
	// judges can differ from the one written here — but only for an
	// IdleTimeout that has already failed above, and a failed Validate
	// means session.New never runs.
	if c.AbsoluteTimeout >= time.Second && c.IdleTimeout >= time.Second &&
		c.AbsoluteTimeout < c.IdleTimeout {
		errs = append(errs, fmt.Errorf("fiber.session.absolute_timeout "+
			"(%s) must be greater than or equal to "+
			"fiber.session.idle_timeout (%s): the session middleware "+
			"panics on this pairing at startup",
			c.AbsoluteTimeout,
			c.IdleTimeout))
	}

	// Required rather than defaulted, for the same reason as
	// fiber.requestid.header: an empty value still produces a working
	// "Lax" on the wire, so what this prevents is not a broken response
	// but a configuration that does not describe one.
	//
	// The allowlist is the load-bearing half. setCookieAttributes
	// recognises only "Strict" and "None" and sends everything else as
	// Lax, so a typo is a silent security downgrade — the one failure in
	// this section that produces no symptom at all until someone reads a
	// Set-Cookie header.
	switch {
	case c.CookieSameSite == "":
		errs = append(errs, errors.New("fiber.session.cookie_same_site is "+
			"required: an empty value is served as Lax by the middleware's "+
			"own fallback, where neither config.yaml nor a startup log "+
			"line will show it"))
	case !containsFold(validSameSite, c.CookieSameSite):
		errs = append(errs, fmt.Errorf("fiber.session.cookie_same_site "+
			"must be one of Strict, Lax, or None, case-insensitive (got "+
			"%q): the middleware matches only Strict and None and sends "+
			"everything else as Lax, so this value is a silent downgrade "+
			"rather than an error",
			c.CookieSameSite))
	case strings.EqualFold(c.CookieSameSite, "None") && !c.CookieSecure:
		// setCookieAttributes forces Secure for SameSite=None before
		// consulting CookieSecure at all, so this pairing does not break
		// the cookie — it makes config.yaml and String report a false
		// that is true on the wire. Same shape as fiber.trust_proxy
		// without an allowlist: a setting that reads one way in the file
		// and another where it counts.
		errs = append(errs, errors.New("fiber.session.cookie_secure must "+
			"be true when fiber.session.cookie_same_site is \"None\": the "+
			"middleware sets Secure unconditionally for None, so false "+
			"here is reported by config.yaml and by the startup log but "+
			"is not what is served"))
	}

	return errors.Join(errs...)
}

// String returns a loggable representation of SessionConfig.
//
// The pointer receiver means fmt only picks this up for a *SessionConfig.
// Printing a value copy (%v on SessionConfig, not &SessionConfig) bypasses
// it and dumps the struct fields directly.
//
// The nil check is TWO-PART, mirroring Validate's. Both a nil
// *SessionConfig and a wrapper around a nil *session.Config reach this
// method: a logger's own guard, such as zerolog's `if val == nil` before
// it calls a Stringer, compares an INTERFACE with nil, which neither
// satisfies. Without the second half, the half-built one would panic
// inside the startup log line instead of rendering a placeholder.
//
// # CookieName is DERIVED, not asserted
//
// It is not read from config — see sessionCookieName — and a session
// cookie whose name appears nowhere in the startup log is the first thing
// someone debugging a login loop goes looking for, so it is reported. But
// it is reported from the Extractor field rather than as a bare literal,
// on the same grounds RequestIDConfig.String derives its Generator line —
// that method's comment carries the argument in full.
//
// The exposure here is identical: the application hands
// *cfg.Session.Config to session.New by value, so an Extractor assigned
// onto that copy — the shape RecoverConfig.WithStackTraceHandler and
// ZerologConfig.WithLogger both use deliberately — would leave this line
// naming "session_id" while the cookie is served under something else.
// Deriving costs one branch and means the line cannot describe a cookie
// that is not the one being set.
//
// The zero-Extractor branch borrows ZerologConfig.String's "(middleware
// default)" convention for the same reason it exists there: a zero value
// is not absence, it is the documented way to select the default, and the
// line should say which of the two it is. A non-zero Extractor names its
// Key when it reads a COOKIE and says "custom extractor" otherwise,
// because extractors.Extractor also covers headers, params and chains. A
// header or param extractor sets no cookie at all, which "session_id"
// would have asserted. A chain carries the Source and Key of its first
// link, so one that opens with a cookie is named by that cookie's Key and
// any other is reported as custom rather than guessed at.
//
// Detecting the zero value on Extract mirrors session.configDefault's own
// `cfg.Extractor.Extract == nil`, so the two agree on what "unset" means.
func (c *SessionConfig) String() string {
	if c == nil {
		return "<nil SessionConfig>"
	}
	if c.Config == nil {
		return "<uninitialised SessionConfig>"
	}

	cookieName := sessionCookieName + " (middleware default)"
	if c.Extractor.Extract != nil {
		cookieName = "custom extractor"
		if c.Extractor.Source == extractors.SourceCookie &&
			c.Extractor.Key != "" {
			cookieName = c.Extractor.Key
		}
	}

	return fmt.Sprintf("AbsoluteTimeout=%s "+
		"CookieName=%s "+
		"CookieDomain=%s "+
		"CookieHTTPOnly=%t "+
		"CookiePath=%s "+
		"CookieSameSite=%s "+
		"CookieSecure=%t "+
		"CookieSessionOnly=%t "+
		"IdleTimeout=%s",
		c.AbsoluteTimeout,
		cookieName,
		c.CookieDomain,
		c.CookieHTTPOnly,
		c.CookiePath,
		c.CookieSameSite,
		c.CookieSecure,
		c.CookieSessionOnly,
		c.IdleTimeout,
	)
}
