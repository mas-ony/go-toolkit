package config

// The fiber.requestid.* section of config.yaml.
//
// Keys in this section, with their environment-variable spellings:
//
//	fiber.requestid.header
//		FIBER_REQUESTID_HEADER
//
// One key, and that is the whole section — NewRequestIDConfig below reads
// exactly that one. The environment spelling holds only for a Viper built by
// NewViper; see the package documentation.

import (
	"errors"
	"fmt"
	"strings"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/requestid"
	"github.com/spf13/viper"
)

// tokenSpecials is the non-alphanumeric half of the RFC 9110 "token" character
// set, which is what an HTTP field name is allowed to contain.
//
// Checked because the value is written verbatim into every response header. A
// name with a space or a colon in it is not a header Fiber will reject on the
// way out — it is a malformed response line, and the failure surfaces at a
// client or an intermediary rather than at startup, which is the wrong place
// for a typo in config.yaml to be discovered.
const tokenSpecials = "!#$%&'*+-.^_`|~"

// DefaultRequestIDHeader is "X-Request-ID", and it is the only value of
// fiber.requestid.header that the request-log field agrees with.
//
// The name has TWO consumers and only one of them reads the configured value:
//
//   - The requestid middleware, which reads fiber.requestid.header and both
//     looks for an inbound id under that name and writes the outbound one
//     there. Fully configurable.
//   - The zerolog middleware's "requestId" field, which does
//     fc.GetRespHeader(fiber.HeaderXRequestID) — a compile-time constant in
//     gofiber/contrib/v3/zerolog. It cannot be pointed anywhere else.
//
// So renaming the header while "requestId" is in fiber.zerolog.fields does not
// rename the log field's source; it removes it, and the field is emitted as an
// empty string on every request with no error anywhere.
// Cross-section validation rejects that pairing, which is the only reason
// this constant is exported rather than inlined — it is what the error message
// tells the operator to type.
const DefaultRequestIDHeader = fiber.HeaderXRequestID

// RequestIDConfig wraps requestid.Config so it participates in the standard
// Validate/String lifecycle used by all other sub-configs.
//
// The middleware belongs SECOND in the stack — inside zerolog, outside
// recover — and every one of those two is load-bearing:
//
//   - Inside zerolog, because the "requestId" log field is read from the
//     RESPONSE header after c.Next() returns. Registered outside, the header
//     would still be set in time, but the documented invariant that zerolog is
//     outermost (so a request that panics is still logged) would be broken for
//     nothing.
//   - Outside recover, so a recovered panic still has an id to log. Both
//     the stack-trace handler and the error handler read it back with
//     requestid.FromContext(c), and that id is what ties the 500 a client saw
//     to the two log lines that explain it.
//
// the application carries the full argument; the point to preserve here is
// that the id has to exist before anything that might fail runs.
//
// # Where the id is readable, which is narrower than it looks
//
// The middleware publishes it with fiber.StoreInContext, and that helper does
// two things conditionally: it ALWAYS writes c.Locals, and it copies the value
// into the request context ONLY when fiber.pass_locals_to_context is true —
// and false is the setting that keeps it out. So requestid.FromContext(c)
// resolves anywhere a fiber.Ctx is in hand, which covers both of the
// application's handlers, and a service or repository holding a plain
// context.Context sees nothing at all. Threading the id into that layer means
// flipping that key or passing it explicitly; there is no fiber.requestid.*
// setting for it.
type RequestIDConfig struct {
	// Embedded as a POINTER, so both the wrapper and the embedded struct can
	// be nil independently — hence the two-part nil check in Validate.
	// Embedding also promotes the fields (c.Header, not
	// c.RequestIDConfig.Config.Header), which is why a nil embedded pointer
	// panics on field access rather than at the method call.
	*requestid.Config
}

// isHTTPFieldName reports whether s is a valid HTTP field name: a non-empty
// RFC 9110 token, i.e. letters, digits, and the characters in tokenSpecials.
//
// Bytes are compared rather than runes because a field name is ASCII by
// definition — indexing a string yields bytes, and any multi-byte rune fails
// the check on its first byte, which is the correct answer.
func isHTTPFieldName(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.IndexByte(tokenSpecials, c) >= 0:
		default:
			return false
		}
	}
	return true
}

// NewRequestIDConfig reads RequestIDConfig fields from the provided Viper
// instance.
//
// Never returns an error: absent keys and uncastable values both come back as
// zero values, which Validate rejects afterwards.
//
// This function is the authoritative list of keys the fiber.requestid.*
// section supports. A key present in config.yaml but missing here is dead
// weight — Viper never looks it up, so neither the file nor an environment
// variable can supply it.
//
// # The two fields NOT read here
//
// requestid.Config has three exported fields and this constructor populates
// one. The remaining two are absent for the same reason: none can be expressed
// in YAML at any spelling. Next and Generator are funcs. No scalar, sequence,
// or mapping could carry either of them.
//
// The middleware supplies a working default for each. A nil Next is never
// consulted, so the middleware never skips; a nil Generator is replaced by
// utils.SecureToken in the middleware's own configDefault. Changing any of
// them means editing this file.
func NewRequestIDConfig(v *viper.Viper) *RequestIDConfig {
	return &RequestIDConfig{
		Config: &requestid.Config{
			// Header is the field name the id is read FROM on the way in and
			// written TO on the way out. Both directions, one key — which is
			// the part worth pausing on, because it means the id is not always
			// generated here.
			//
			// The middleware reuses an inbound value when the request already
			// carries this header and the value is valid, and only generates
			// one otherwise. Behind an API gateway that is exactly right: the
			// gateway stamps an id, the service adopts it, and one identifier
			// spans both hops. Exposed directly it means the id in the log is
			// CLIENT-SUPPLIED, so a caller can send the same value on every
			// request and collapse the correlation, or reuse an id it saw
			// elsewhere. Nothing here can tell the two deployments apart, and
			// neither can the middleware — the only guard is that Fiber v3
			// validates the inbound value as visible ASCII (0x20–0x7E) and
			// regenerates it otherwise, which rules out CRLF injection into
			// the response but not a chosen or repeated id.
			//
			// The generated form, when it is generated, is utils.SecureToken:
			// 32 crypto/rand bytes in base64url, 43 characters of
			// [A-Za-z0-9_-] with no padding. Not a UUID — do not write a
			// parser expecting one. Fiber v3 arrives at it two ways — it is
			// the default Generator, and it is also the unconditional fallback
			// sanitizeRequestID takes when a custom Generator returns an
			// invalid value three times running — so it is the shape to expect
			// whatever a future Generator does.
			//
			// Length of an ADOPTED id is bounded only by
			// fiber.read_buffer_size, since the header has to fit the read
			// buffer to arrive at all. A client that fills it puts ~4 KiB in
			// every response header and every log line for its own requests.
			// Lower that key, or put the gateway in front, if that matters.
			//
			// Changing this value away from "X-Request-ID" is a startup error
			// while "requestId" is in fiber.zerolog.fields — see
			// DefaultRequestIDHeader for why the log field cannot follow it.
			//
			// That comparison is EXACT, so a merely re-cased "x-request-id" is
			// rejected too, and on the wire it would have worked: fasthttp
			// normalises header names, so the response would still carry
			// X-Request-ID and the log field would still find it. The check is
			// deliberately stricter than the wire because that normalisation
			// is itself a key — set fiber.disable_header_normalizing to true
			// and the header is written and matched as spelled, which breaks
			// adoption of an inbound X-Request-ID and empties the log field,
			// silently, in a change that never touches this section.
			Header: v.GetString("fiber.requestid.header"),
		},
	}
}

// Validate returns a joined error for every invalid or missing RequestIDConfig
// field.
//
// Deliberately unchecked:
//
//   - Whether Header equals DefaultRequestIDHeader. On its own a rename is a
//     perfectly good configuration — "X-Correlation-Id" is a real convention
//     and the middleware honours it end to end. It only breaks in combination
//     with "requestId" in fiber.zerolog.fields, and this method cannot see
//     that section. Cross-section validation holds both and rejects the
//     pairing there.
//   - Generator and Next. Both are nil here by design; the middleware's
//     configDefault substitutes utils.SecureToken for the nil Generator, and
//     never consults a nil Next. Neither can be expressed in YAML. "Nil here"
//     is all this method can speak to, and the distinction matters for
//     Generator. the application hands *cfg.RequestID.Config to requestid.New
//     by value, so a Generator assigned onto that copy — the shape
//     RecoverConfig.WithStackTraceHandler and ZerologConfig.WithLogger both
//     use deliberately — would be invisible from here, exactly as a
//     LimiterMiddleware assigned at the registration site is invisible to
//     LimiterConfig.Validate. Nothing does that today. String below therefore
//     DERIVES what it reports about this field rather than asserting it, which
//     is the only defence available: a check here could not see the
//     assignment, but a log line that reads the field cannot describe a
//     generator that is not the one running.
//
// Every check appends rather than returning early, so one restart surfaces
// every fiber.requestid.* problem at once.
func (c *RequestIDConfig) Validate() error {
	// errs is declared before the nil check only so the two statements read in
	// the same order in all Validate implementations; the nil check is what
	// must come first, since every line after it dereferences c.
	var errs []error
	if c == nil || c.Config == nil {
		return errors.New("fiber.requestid config was not initialised")
	}

	// Required rather than defaulted, because an empty value here still
	// produces "X-Request-ID" on the wire, so the failure this prevents is not
	// a broken response but a configuration that does not describe one. See
	// the field comment in NewRequestIDConfig.
	if c.Header == "" {
		errs = append(errs, fmt.Errorf("fiber.requestid.header is required: "+
			"it names a header written to every response, and leaving it "+
			"empty serves %q anyway — from the middleware's own fallback, "+
			"where neither config.yaml nor a startup log line will show it",
			DefaultRequestIDHeader))
	} else if !isHTTPFieldName(c.Header) {
		errs = append(errs, fmt.Errorf("fiber.requestid.header must be a "+
			"valid HTTP field name — letters, digits, or any of %s, with no "+
			"spaces, colons, quotes, or non-ASCII characters (got %q)",
			tokenSpecials,
			c.Header))
	}

	return errors.Join(errs...)
}

// String returns a loggable representation of RequestIDConfig.
//
// The pointer receiver means fmt only picks this up for a *RequestIDConfig.
// Printing a value copy (%v on RequestIDConfig, not &RequestIDConfig) bypasses
// it and dumps the struct fields directly.
//
// The nil check is TWO-PART, mirroring Validate's. zerolog reaches this method
// through fmt.Stringer, and its own guard — `if val == nil` in
// internal/json.AppendStringer — is an INTERFACE nil, which neither a typed
// nil pointer nor a wrapper around a nil embedded pointer satisfies. So it
// calls String on both, and without the second half the half-built one panics
// inside the startup log line rather than rendering a placeholder.
// The section list carries the argument.
//
// # Generator is DERIVED, not asserted
//
// A literal "Generator=utils.SecureToken" would be true on the current wiring:
// the field is nil here, the application passes *cfg.RequestID.Config through
// unmodified, and configDefault substitutes utils.SecureToken for the nil. But
// it would be true by coincidence of a call site this file cannot see, not by
// construction — the same shape as RecoverConfig.WithStackTraceHandler and
// ZerologConfig.WithLogger, both of which assign a func onto a COPY long after
// this struct is built. Assign a Generator that way and the startup line goes
// on naming utils.SecureToken while something else mints every id, which is
// exactly the failure this package rejects everywhere else: a setting that
// reads one way in config.yaml, reads the same way in String output, and is
// something else in the only place it acts.
//
// What deriving buys is specific rather than cosmetic. The Header comment in
// NewRequestIDConfig documents the generated form as 43 characters of
// [A-Za-z0-9_-] and warns against writing a parser that expects a UUID; this
// field is the only thing in the log that says whether that shape still holds.
//
// The nil branch borrows ZerologConfig.String's "(middleware default)"
// convention, for the same reason it exists there: nil is not absence, it is
// the documented way to select the default, and the line should say which of
// the two it is. The non-nil branch cannot say more than "custom" — %T on any
// of them prints func() string, which distinguishes nothing.
func (c *RequestIDConfig) String() string {
	if c == nil {
		return "<nil RequestIDConfig>"
	}
	if c.Config == nil {
		return "<uninitialised RequestIDConfig>"
	}

	generator := "custom"
	if c.Generator == nil {
		generator = "utils.SecureToken (middleware default)"
	}

	return fmt.Sprintf("Header=%s "+
		"Generator=%s",
		c.Header,
		generator,
	)
}
