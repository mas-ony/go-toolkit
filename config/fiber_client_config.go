// fiber_client_config.go covers the fiber.client.* section of config.yaml.
//
// Keys in this section, with their environment-variable spellings:
//
//	fiber.client.base_url
//		FIBER_CLIENT_BASE_URL
//	fiber.client.token
//		FIBER_CLIENT_TOKEN
//	fiber.client.retries
//		FIBER_CLIENT_RETRIES
//	fiber.client.no_retry_methods
//		FIBER_CLIENT_NO_RETRY_METHODS
//	fiber.client.insecure
//		FIBER_CLIENT_INSECURE
//
// Five keys, and that is the whole section — NewClientConfig below reads
// exactly these. See doc.go for how the environment spelling is derived and
// which tests hold it up.

package config

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/gofiber/fiber/v3"
	"github.com/spf13/viper"
)

// validNoRetryMethods is the allowlist checked by ClientConfig.Validate,
// spelled with Fiber's own method constants so a rename upstream is a
// compile error here rather than a silent gap in the list.
//
// Checked because a typo in this key is INVISIBLE at runtime. The transport
// looks an entry up as strings.ToUpper(method), so "PSOT" matches no request
// it will ever make: the method it was meant to exclude keeps its retries,
// and the only symptom is a create replayed after a timeout — the one
// failure the key exists to prevent, in a deployment whose config.yaml says
// it cannot happen.
//
// The empty-struct value is the idiomatic set, the same shape validEnvs in
// app_config.go uses. Membership is tested on the UPPERCASED entry, because
// that is the form the transport compares, so "post" and "POST" are the same
// setting here and there.
var validNoRetryMethods = map[string]struct{}{
	fiber.MethodGet:     {},
	fiber.MethodHead:    {},
	fiber.MethodPost:    {},
	fiber.MethodPut:     {},
	fiber.MethodPatch:   {},
	fiber.MethodDelete:  {},
	fiber.MethodConnect: {},
	fiber.MethodOptions: {},
	fiber.MethodTrace:   {},
	fiber.MethodQuery:   {},
}

// ClientConfig carries the settings for an OUTBOUND HTTP client.
type ClientConfig struct {
	// BaseURL is the service root: scheme, host, and optionally a path
	// prefix, e.g. "https://api.example.com" or
	// "https://api.example.com/api/v1". A trailing slash is tolerated, and so
	// is the "/api/v1" a URL pasted out of a browser carries, provided the
	// client is built with httpclient.Options.TrimPathSuffix set to it.
	//
	// Empty means no client is configured. See the type comment: it is a
	// legitimate setting for a deployment that only serves, which is why
	// Validate lets it through. httpclient.New refuses it by name, at the
	// one point where something is definitely trying to build a client.
	BaseURL string

	// Token is the bearer credential, sent as "Authorization: Bearer
	// <token>" on every request. Blank in config.yaml and supplied as
	// FIBER_CLIENT_TOKEN, the same way the database pair is.
	//
	// Not required, and that is the difference from those two. A service that
	// authenticates by network position rather than by header is a real
	// deployment, so an absent token is a working configuration and cannot be
	// rejected here. The failure it leaves is a 401 from the far end, which
	// names itself.
	Token string

	// Retries is how many ADDITIONAL attempts a retryable failure gets. Zero
	// means one attempt and no retry, which is the value an absent key reads
	// as.
	//
	// What counts as retryable is the transport's decision, not this
	// package's, and the usual rule is two kinds of failure: a transport
	// error (connection refused, reset, timeout) and the statuses that mean
	// "try again later" — 429, 500, 502, 503, 504. Every other 4xx is a
	// deterministic answer about this request and gets one attempt however
	// high this is set.
	//
	// It buys time as well as attempts. Under the common exponential backoff
	// — doubling from 500ms, capped at 8s — 2 spends up to 1.5s before giving
	// up and 5 spends up to 15.5s, with each further retry adding the cap to
	// a call that is already failing. That is per REQUEST, so a batch of them
	// multiplies it: the reason to keep this small is the unattended run that
	// now takes an hour to report the outage it hit in the first minute.
	Retries int

	// NoRetryMethods are the methods that get exactly one attempt whatever
	// the failure, and this key has THREE settings rather than two.
	//
	// Absent, it is nil, and the transport is expected to substitute its own
	// default: POST alone. Stated, it is exactly what is stated. And stated
	// EMPTY — `no_retry_methods: []` — every method is replayed, which is the
	// opposite of the default and has to be asked for. splitList preserves
	// that distinction (nil for a key no source supplies, non-nil for a
	// stated empty list), which is what makes the third setting expressible
	// at all. A transport that treats nil and empty alike collapses two of
	// the three back into one, and this is the field to check when it does.
	//
	// From the ENVIRONMENT the empty list is not expressible: Viper's
	// AllowEmptyEnv is off, so FIBER_CLIENT_NO_RETRY_METHODS= reads as unset
	// and lands on the default instead. The file is the only place it can be
	// written.
	//
	// POST is the usual default because replaying a create is only safe when
	// the server can recognise the duplicate. Where the request body carries
	// nothing unique, a replayed POST whose first attempt had in fact landed
	// writes a SECOND row, and no later response can tell the two cases
	// apart. Widen this only for endpoints that address a specific id or
	// carry a natural key the server rejects a second copy on.
	NoRetryMethods []string

	// Insecure disables TLS certificate verification for every request the
	// client makes. The real case is a private CA on an internal deployment;
	// anything reachable from outside should fix its certificate instead.
	//
	// Inert under an http:// base_url, which negotiates no TLS to skip the
	// checks of — so true there is not a setting that does nothing dangerous,
	// it is a setting that will do something the day the URL gains its "s"
	// and nobody revisits this line.
	Insecure bool
}

// NewClientConfig reads ClientConfig fields from the provided Viper instance.
//
// Never returns an error: absent keys and uncastable values both come back as
// zero values, which Validate rejects afterwards.
//
// This function is the authoritative list of keys the fiber.client.* section
// supports. A key present in config.yaml but missing here is dead weight —
// Viper never looks it up, so neither the file nor an environment variable
// can supply it.
//
// # Three things a client needs that are not here
//
// None of them is absent because it does not matter; each is a shape no key
// in this section could carry, and each belongs to the application that
// builds the client.
//
// All three are carried by httpclient.Options instead, which httpclient.New
// takes alongside a *ClientConfig. That is also why this section has no
// WithLogger: a logger travels in that struct rather than being copied onto a
// foreign config type the way ZerologConfig copies one.
//
// A HEADER MAP is the one shape this file cannot express. Every test that
// pairs config.yaml against these constructors compares LEAF keys — a mapping
// is recursed into, so "fiber.client.headers" would never appear as a key
// anything reads, while each name an operator wrote under it would appear as
// a key nothing reads. The environment cannot express it at all.
// Authorization, the header this would mostly be used for, has its own key
// above.
//
// A ROUTE PREFIX to trim is a fact about the far service's own routes rather
// than a deployment setting: the call site names it once as the constant it
// also builds its paths from, so the two cannot drift. A key here would let
// them, and the symptom would be a doubled prefix answering 404 — which reads
// as a missing route rather than as a bad setting.
//
// A LOGGER is not a value any YAML could hold. The application installs its
// own on the client it constructs.
func NewClientConfig(v *viper.Viper) *ClientConfig {
	return &ClientConfig{
		BaseURL: v.GetString("fiber.client.base_url"),
		Token:   v.GetString("fiber.client.token"),
		Retries: v.GetInt("fiber.client.retries"),

		// Read through splitList, so the environment accepts commas or
		// spaces; see splitList in splitlist.go for why the comma spelling
		// would otherwise arrive as one element.
		NoRetryMethods: splitList(v, "fiber.client.no_retry_methods"),

		Insecure: v.GetBool("fiber.client.insecure"),
	}
}

// Validate returns a joined error for every invalid or missing ClientConfig
// field.
//
// Deliberately unchecked:
//
//   - Whether anything builds a client from this section. Validate sees a
//     *ClientConfig and nothing else, so it cannot tell a configured client
//     from five keys nobody reads.
//   - BaseURL's absence. Empty is the correct setting for a deployment that
//     calls nothing, and the client constructor is where it should be refused
//     by name for one that does. The type comment carries the argument.
//   - Token. A service that authenticates by network position rather than by
//     header needs none, so an absent one is a working configuration. See the
//     field comment in NewClientConfig.
//   - Insecure. A bool has no invalid value: both settings are meaningful and
//     an absent key reads as false, which is the safe one. What true COSTS is
//     real — every certificate accepted, expired or forged alike — but
//     that is an operator's decision about a private CA, not a malformed
//     value.
//   - Retries above any particular number. The cost of a high value is wall
//     clock rather than correctness, and no number here is wrong for every
//     deployment; the arithmetic is on the field so it can be read by someone
//     about to raise it.
//
// Every check appends rather than returning early, so one restart surfaces
// every fiber.client.* problem at once.
func (c *ClientConfig) Validate() error {
	// errs is declared before the nil check only so the two statements read in
	// the same order in all Validate implementations; the nil check is what
	// must come first, since every line after it dereferences c.
	var errs []error
	if c == nil {
		return errors.New("fiber.client config was not initialised")
	}

	// A STATED base URL is checked, an absent one is not. The two are
	// different findings: absence says this deployment calls nothing, while a
	// value that is not a URL says it meant to call something and cannot.
	//
	// Trimmed and re-parsed the way a client constructor would, so what is
	// checked is what that constructor will see. The host check is the one a
	// constructor typically does NOT make: "https://" parses, carries the
	// right scheme, and fails every request afterwards with no host to dial.
	if c.BaseURL != "" {
		raw := strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
		u, err := url.Parse(raw)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("fiber.client.base_url is not a "+
				"valid URL: %w",
				err))
		case u.Scheme != "http" && u.Scheme != "https":
			errs = append(errs, fmt.Errorf("fiber.client.base_url must "+
				"start with http:// or https:// (got %q)",
				c.BaseURL))
		case u.Host == "":
			errs = append(errs, fmt.Errorf("fiber.client.base_url names no "+
				"host (got %q): every request would be built against an "+
				"address with nothing to dial",
				c.BaseURL))
		}
	}

	// Negative is not "fewer retries", it is NO REQUEST. The transport loops
	// `for attempt := 0; attempt <= retries; attempt++`, so a negative bound
	// skips the body entirely and every call returns "giving up after 0
	// attempts" without a packet leaving the process — a failure that reads
	// like an unreachable service and is a number in this file.
	if c.Retries < 0 {
		errs = append(errs, fmt.Errorf("fiber.client.retries cannot be "+
			"negative (got %d): 0 already means one attempt and no retry",
			c.Retries))
	}

	// Entries are checked, the LIST is not: an empty one is the documented
	// third setting of this key rather than a mistake, and rejecting it would
	// leave no way to say "replay everything".
	for i, m := range c.NoRetryMethods {
		if _, ok := validNoRetryMethods[strings.ToUpper(m)]; !ok {
			errs = append(errs, fmt.Errorf("fiber.client.no_retry_"+
				"methods[%d] is not an HTTP method (got %q): it would match "+
				"no request the client makes, so the method it was meant to "+
				"exclude keeps its retries and nothing reports it",
				i,
				m))
		}
	}

	return errors.Join(errs...)
}

// String returns a loggable representation of ClientConfig.
//
// The pointer receiver means fmt only picks this up for a *ClientConfig.
// Printing a value copy (%v on ClientConfig, not &ClientConfig) bypasses it
// and dumps the struct fields directly — the token included.
//
// Token is reported as PRESENT OR NOT rather than printed. This struct holds
// a credential and the omission is deliberate, as in DatabaseConfig.String —
// but a bearer token differs from a database password in that its absence is
// legal, so a line that showed nothing at all could not distinguish an
// unauthenticated client from one whose FIBER_CLIENT_TOKEN never reached the
// process. Two words carry that; the value itself carries nothing else worth
// having in a log.
//
// NoRetryMethods DERIVES its three states rather than printing the slice,
// because the two that matter are both length zero and %v renders them
// identically as []. Nil leaves the transport its default and a stated empty
// list replays every method including POST — the one setting in this section
// that can duplicate a row — so a startup line that could not tell them
// apart would be silent about exactly the setting worth reading it for.
func (c *ClientConfig) String() string {
	if c == nil {
		return "<nil ClientConfig>"
	}

	token := "(none)"
	if c.Token != "" {
		token = "(set)"
	}

	methods := strings.Join(c.NoRetryMethods, ",")
	switch {
	case c.NoRetryMethods == nil:
		methods = "POST (transport default)"
	case len(c.NoRetryMethods) == 0:
		methods = "none (every method is replayed)"
	}

	return fmt.Sprintf("BaseURL=%s "+
		"Token=%s "+
		"Retries=%d "+
		"NoRetryMethods=%s "+
		"Insecure=%t",
		c.BaseURL,
		token,
		c.Retries,
		methods,
		c.Insecure,
	)
}
