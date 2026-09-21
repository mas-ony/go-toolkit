// fiber_session_config.go covers the fiber.session.* section of config.yaml.
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
// exactly these. See doc.go for how the environment spelling is derived and
// which tests hold it up.

package config

import (
	"errors"
	"fmt"
	"strings"

	"github.com/gofiber/fiber/v3/extractors"
	"github.com/gofiber/fiber/v3/middleware/session"
	"github.com/spf13/viper"
)

// sessionCookieName is the name the session cookie is actually served under,
// and it is NOT one of the eight keys above.
//
// The name lives on session.Config.Extractor, whose default is
// extractors.FromCookie("session_id"), and that field is a struct carrying a
// func — nothing YAML can express, so NewSessionConfig leaves it nil and the
// middleware's configDefault substitutes the default. Every cookie ATTRIBUTE
// is configurable here; the cookie NAME is not.
//
// Declared rather than inlined so that the fallback String reports and the
// middleware's own default have one spelling between them. String does not
// print it unconditionally — it derives the name from the Extractor and
// reaches for this only when that field is unset, which is the case where the
// middleware will substitute extractors.FromCookie(sessionCookieName) itself.
// See the derivation note on String.
const sessionCookieName = "session_id"

// validSameSite is the allowlist checked by SessionConfig.Validate.
//
// Matched case-INSENSITIVELY, mirroring the utils.EqualFold comparison in
// session.setCookieAttributes: rejecting "strict" here would reject a value
// the middleware honours exactly as written.
var validSameSite = []string{"Strict", "Lax", "None"}

// SessionConfig wraps session.Config so it participates in the standard
// Validate/String lifecycle used by all other sub-configs.
//
// This config is only read and validated when fiber.auth.mode is "session".
// In "jwt" mode the session middleware is not registered and this struct is
// never consulted.
//
// In-memory store note: NewStore substitutes memory.New() for a nil Storage,
// so the store is per-PROCESS and shared with nothing. When EnablePrefork is
// true (fiber_listen_config.go), each worker has its own isolated store and
// requests landing on a different worker than the one that issued the cookie
// appear unauthenticated — intermittently, and only under load, which is the
// worst shape of bug to diagnose. Either disable prefork or install a shared
// Storage (e.g. Redis) before enabling both prefork and session auth at once.
// Nothing here can check that pairing — it needs ListenConfig — so
// Cross-section validation rejects it instead.
type SessionConfig struct {
	// Embedded as a POINTER, so both the wrapper and the embedded struct can
	// be nil independently — hence the two-part nil check in Validate.
	// Embedding also promotes the fields (c.IdleTimeout, not
	// c.SessionConfig.Config.IdleTimeout), which is why a nil embedded pointer
	// panics on field access rather than at the method call.
	*session.Config
}

// containsFold reports whether s equals any element of list under Unicode case
// folding. Kept unexported and local because validSameSite is the only
// allowlist in this package matched case-insensitively; every other one
// (validEnvs, the driver set) is deliberately exact.
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
// Never returns an error: absent keys and uncastable values both come back as
// zero values, which Validate rejects afterwards.
//
// This function is the authoritative list of keys the fiber.session.* section
// supports. A key present in config.yaml but missing here is dead weight —
// Viper never looks it up, so neither the file nor an environment variable can
// supply it.
//
// # The six fields NOT read here
//
// session.Config has fourteen exported fields and this constructor populates
// eight. The remaining six are absent for the same reason: none can be
// expressed in YAML at any spelling. Storage and Store are interfaces/pointers
// to live objects; Next, ErrorHandler, and KeyGenerator are funcs; Extractor
// is a struct carrying one. No scalar, sequence, or mapping could carry any of
// them.
//
// The middleware supplies a working default for all six, and two of those
// defaults are worth knowing rather than just noting:
//
//   - Storage nil means NewStore substitutes memory.New() — the per-process
//     store described on the struct above.
//   - Extractor's zero value means configDefault substitutes
//     extractors.FromCookie("session_id"), which is where the cookie NAME
//     comes from. See sessionCookieName.
//
// The other four are unremarkable: a nil Store is filled by NewStore, a nil
// Next is never consulted so the middleware never skips, a nil ErrorHandler
// makes handleSessionError fall back to logging and a 500, and a nil
// KeyGenerator becomes utils.SecureToken. Changing any of the six means
// editing this file or installing the value on a copy at router.New's call
// site, the way RecoverConfig.WithStackTraceHandler does.
func NewSessionConfig(v *viper.Viper) *SessionConfig {
	return &SessionConfig{
		Config: &session.Config{
			// Hard upper bound on session lifetime regardless of activity.
			// After this duration the session is destroyed even if the user
			// has been continuously active.
			//
			// Zero is legal to the MIDDLEWARE — it means "no absolute timeout,
			// expire on idle alone" — and Validate rejects it anyway. That is
			// a policy stricter than Fiber's, not a repair of it: an e-Office
			// session that renews forever on activity is a credential with no
			// expiry, and this deployment wants a hard ceiling. Relaxing the
			// check is a one-line change in Validate if that ever stops being
			// true.
			AbsoluteTimeout: v.GetDuration("fiber.session.absolute_timeout"),

			// Domain attribute of the Set-Cookie header. Empty string uses the
			// request host (same-host cookies only). Set to a parent domain
			// (e.g. ".example.com") to share sessions across subdomains.
			CookieDomain: v.GetString("fiber.session.cookie_domain"),

			// HttpOnly flag on the session cookie. When true, JavaScript
			// cannot read the cookie via document.cookie, which mitigates
			// XSS-based session hijacking. Strongly recommended in production.
			//
			// ABSENT KEY READS AS FALSE. Where no client-side script reads
			// this cookie — the browser attaches it automatically — true costs
			// nothing.
			CookieHTTPOnly: v.GetBool("fiber.session.cookie_http_only"),

			// Path attribute of the Set-Cookie header. Empty string defaults
			// to "/", making the cookie available for all paths on the domain.
			CookiePath: v.GetString("fiber.session.cookie_path"),

			// SameSite attribute of the session cookie. Controls when the
			// browser includes the cookie in cross-site requests:
			//   Strict — never sent cross-site (most secure, may break
			//            OAuth flows).
			//   Lax    — sent on top-level same-site navigations
			//            (recommended default).
			//   None   — always sent cross-site; forces Secure, see below.
			//
			// Matching in setCookieAttributes is utils.EqualFold against
			// "Strict" and "None", and EVERYTHING ELSE FALLS THROUGH TO LAX.
			// So "strict" works, and "Strcit" is silently Lax — a downgrade
			// with no error anywhere. Validate carries the allowlist for that
			// reason; the middleware has no way to report it.
			CookieSameSite: v.GetString("fiber.session.cookie_same_site"),

			// Secure flag on the session cookie. When true, the browser only
			// sends the cookie over HTTPS connections. Must be true in any
			// production or public-facing environment.
			//
			// NOT the last word on the wire value. setCookieAttributes forces
			// Secure(true) whenever SameSite resolves to None, regardless of
			// what is set here — so the pairing (None, false) serves a Secure
			// cookie while config.yaml and String both report false. Validate
			// rejects that pairing rather than let the two disagree.
			CookieSecure: v.GetBool("fiber.session.cookie_secure"),

			// When true, the cookie carries no Max-Age or Expires attribute
			// and is deleted when the browser session ends (tab/window close).
			// The server-side session still expires via AbsoluteTimeout and
			// IdleTimeout — this flag affects only the client-side cookie
			// lifetime.
			//
			// Practical effect: a user who closes and reopens the browser
			// loses the cookie (and thus the session) even if the server-side
			// session has not yet expired. This is the more secure default for
			// apps where users share devices. Set false when "remember me"
			// behaviour is desired and CookieSecure is true.
			CookieSessionOnly: v.GetBool("fiber.session.cookie_session_only"),

			// Inactivity timeout. If no request touches the session within
			// this window the session is destroyed and the user must log in
			// again.
			//
			// Must be non-zero AND must not exceed AbsoluteTimeout. The second
			// half is not a style rule: configDefault PANICS on
			// AbsoluteTimeout > 0 && AbsoluteTimeout < IdleTimeout, and
			// router.New calls session.New during startup, so the pairing
			// takes the process down with a runtime panic instead of a config
			// error. Validate catches it first.
			IdleTimeout: v.GetDuration("fiber.session.idle_timeout"),
		},
	}
}

// Validate returns a joined error for every invalid or missing SessionConfig
// field. Only called when fiber.auth.mode is "session".
//
// Deliberately unchecked:
//
//   - Whether the store is shared across processes. That is the prefork
//     pairing on the struct doc; it needs ListenConfig, which this method
//     never sees, so cross-section validation holds it.
//   - Storage, Store, Next, ErrorHandler, KeyGenerator, and Extractor. All are
//     nil here by design and stay nil for the life of the process; the
//     middleware's configDefault substitutes memory.New() for the nil Storage
//     and never consults a nil Next; utils.SecureToken for the nil
//     KeyGenerator; and extractors.FromCookie("session_id") for the nil
//     Extractor. None can be expressed in YAML.
//   - CookieDomain and CookiePath. Empty is the correct default for both — the
//     request host and "/" respectively — and any non-empty value is a
//     deployment decision this package cannot second-guess. A domain that does
//     not match the request host makes the browser drop the cookie, but only
//     the deployment knows which host that is.
//   - CookieHTTPOnly and CookieSessionOnly. Both settings are meaningful and
//     an absent key reads as false, which is the documented default. What
//     false costs is on the fields in NewSessionConfig, where it can be read
//     by someone about to change it.
//
// Every check appends rather than returning early, so one restart surfaces
// every fiber.session.* problem at once.
func (c *SessionConfig) Validate() error {
	// errs is declared before the nil check only so the two statements read in
	// the same order in all Validate implementations; the nil check is what
	// must come first, since every line after it dereferences c.
	var errs []error
	if c == nil || c.Config == nil {
		return errors.New("fiber.session config was not initialised")
	}

	if c.AbsoluteTimeout == 0 {
		errs = append(errs, errors.New("fiber.session.absolute_timeout is "+
			"required: zero is legal to the middleware and means the "+
			"session expires on idle alone, so a continuously active client "+
			"holds one credential forever"))
	}
	if c.IdleTimeout == 0 {
		errs = append(errs, errors.New("fiber.session.idle_timeout is "+
			"required: zero is replaced by the middleware's own 30m default, "+
			"which appears in neither config.yaml nor this config's log line"))
	}

	// Checked here rather than left to the middleware because the middleware's
	// answer is a panic. session.configDefault runs on the value router.New
	// passes to session.New, and an AbsoluteTimeout below IdleTimeout takes
	// the process down at startup with "[session] AbsoluteTimeout must be
	// greater than or equal to IdleTimeout" — a message that names neither
	// config key and no section.
	//
	// The two conjuncts are safe for DIFFERENT reasons, which is worth
	// spelling out because only one of them mirrors the middleware.
	// AbsoluteTimeout > 0 mirrors it exactly: configDefault gates its own
	// guard on the same condition, so a zero there cannot panic and is already
	// reported above. IdleTimeout > 0 does NOT mirror it — configDefault
	// substitutes its 30m default for a zero IdleTimeout BEFORE comparing, so
	// absolute_timeout: 10m with idle_timeout absent would panic on 10m < 30m
	// and this check would not see it. What makes that unreachable is the
	// separate "idle_timeout is required" error above: validate fails, New
	// returns an error, and router.New never runs. The conjunct is there so
	// this error does not fire alongside that one describing a 30m the
	// operator never wrote — not because the pairing is harmless.
	if c.AbsoluteTimeout > 0 && c.IdleTimeout > 0 &&
		c.AbsoluteTimeout < c.IdleTimeout {
		errs = append(errs, fmt.Errorf("fiber.session.absolute_timeout (%s) "+
			"must be greater than or equal to fiber.session.idle_timeout "+
			"(%s): the session middleware panics on this pairing at startup",
			c.AbsoluteTimeout,
			c.IdleTimeout))
	}

	// Required rather than defaulted, for the same reason as
	// fiber.requestid.header: an empty value still produces a working "Lax" on
	// the wire, so what this prevents is not a broken response but a
	// configuration that does not describe one.
	//
	// The allowlist is the load-bearing half. setCookieAttributes recognises
	// only "Strict" and "None" and sends everything else as Lax, so a typo is
	// a silent security downgrade — the one failure in this section that
	// produces no symptom at all until someone reads a Set-Cookie header.
	switch {
	case c.CookieSameSite == "":
		errs = append(errs, errors.New("fiber.session.cookie_same_site is "+
			"required: an empty value is served as Lax by the middleware's "+
			"own fallback, where neither config.yaml nor a startup log line "+
			"will show it"))
	case !containsFold(validSameSite, c.CookieSameSite):
		errs = append(errs, fmt.Errorf("fiber.session.cookie_same_site must "+
			"be one of Strict, Lax, or None, case-insensitive (got %q): the "+
			"middleware matches only Strict and None and sends everything "+
			"else as Lax, so this value is a silent downgrade rather than "+
			"an error",
			c.CookieSameSite))
	case strings.EqualFold(c.CookieSameSite, "None") && !c.CookieSecure:
		// setCookieAttributes forces Secure(true) for SameSite=None before
		// consulting CookieSecure at all, so this pairing does not break the
		// cookie — it makes config.yaml and String report a false that is true
		// on the wire. Same shape as fiber.trust_proxy without an allowlist: a
		// setting that reads one way in the file and another where it counts.
		errs = append(errs, errors.New("fiber.session.cookie_secure must be "+
			"true when fiber.session.cookie_same_site is \"None\": the "+
			"middleware sets Secure unconditionally for None, so false here "+
			"is reported by config.yaml and by the startup log but is not "+
			"what is served"))
	}

	return errors.Join(errs...)
}

// String returns a loggable representation of SessionConfig.
//
// The pointer receiver means fmt only picks this up for a *SessionConfig.
// Printing a value copy (%v on SessionConfig, not &SessionConfig) bypasses it
// and dumps the struct fields directly.
//
// The nil check is TWO-PART, mirroring Validate's. zerolog reaches this method
// through fmt.Stringer, and its own guard — `if val == nil` in
// internal/json.AppendStringer — is an INTERFACE nil, which neither a typed
// nil pointer nor a wrapper around a nil embedded pointer satisfies. So it
// calls String on both, and without the second half the half-built one panics
// inside the startup log line rather than rendering a placeholder.
// The section list carries the argument.
//
// # CookieName is DERIVED, not asserted
//
// It is not read from config — see sessionCookieName — and a session cookie
// whose name appears nowhere in the startup log is the first thing someone
// debugging a login loop goes looking for, so it is reported. But it is
// reported from the Extractor field rather than as a bare literal, on the same
// grounds RequestIDConfig.String derives its Generator line — that method's
// comment carries the argument in full.
//
// The exposure here is identical: router.New hands *cfg.Session.Config to
// session.New by value, so an Extractor assigned onto that copy — the shape
// RecoverConfig.WithStackTraceHandler and ZerologConfig.WithLogger both use
// deliberately — would leave this line naming "session_id" while the cookie is
// served under something else. Nothing does that today. Deriving costs one
// branch and means the line cannot describe a cookie that is not the one being
// set.
//
// The zero-Extractor branch borrows ZerologConfig.String's "(middleware
// default)" convention for the same reason it exists there: a zero value is
// not absence, it is the documented way to select the default, and the line
// should say which of the two it is. A non-zero Extractor names its Key when
// it reads a COOKIE and says "custom extractor" otherwise, because
// extractors.Extractor also covers headers, params, and chains — and for those
// the cookie name is not merely unknown, there is no cookie, which
// "session_id" would have asserted.
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
