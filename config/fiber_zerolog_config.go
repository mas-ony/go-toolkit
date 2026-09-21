// fiber_zerolog_config.go covers the fiber.zerolog.* section of config.yaml.
//
// Keys in this section, with their environment-variable spellings:
//
//	fiber.zerolog.fields
//		FIBER_ZEROLOG_FIELDS
//	fiber.zerolog.fields_snake_case
//		FIBER_ZEROLOG_FIELDS_SNAKE_CASE
//	fiber.zerolog.wrap_headers
//		FIBER_ZEROLOG_WRAP_HEADERS
//	fiber.zerolog.messages
//		FIBER_ZEROLOG_MESSAGES
//	fiber.zerolog.levels
//		FIBER_ZEROLOG_LEVELS
//
// Five keys, and that is the whole section — NewZerologConfig below reads
// exactly these. See doc.go for how the environment spelling is derived and
// which tests hold it up.

package config

import (
	"errors"
	"fmt"
	"strings"

	fiberzerolog "github.com/gofiber/contrib/v3/zerolog"
	"github.com/rs/zerolog"
	"github.com/spf13/viper"
)

// zerologClassCount is how many response classes the middleware buckets by
// status code: >= 500, >= 400, and everything else. Both
// fiber.zerolog.messages and fiber.zerolog.levels are indexed by that bucket,
// so both are required to have exactly this many entries when they are set at
// all.
//
// The middleware itself accepts fewer and clamps the index to the last
// element, which is why this file requires the full three rather than leaving
// it alone. A one-element list reads as "only this" and means "this for
// everything": levels: [error] logs every 200 at Error, and nothing about the
// running service would suggest the list was ever meant to be longer.
const zerologClassCount = 3

// ZerologConfig wraps fiberzerolog.Config so it participates in the standard
// Validate/String lifecycle used by all other sub-configs.
//
// The middleware is registered OUTERMOST in router.New, so it wraps requestid,
// recover, and limiter middleware inside those. That is so every request is
// logged, including ones that later panic: fiberrecover sits inside it and
// converts a panic into a returned error, which surfaces as the value of this
// middleware's c.Next() and so still gets a log line with a status attached.
//
// Only the OUTERMOST position matters to this file. Which of the others
// follow, and whether there are three of them or four, is router.New's
// argument to make.
//
// Reversing the two is not a crash and that is the hazard. This middleware
// does `chainErr := c.Next()` with no defer, so a panic recovered OUTSIDE it
// unwinds straight past that call and the request is missing from the log
// entirely — no error, no warning, just a request that was served and never
// recorded. router.New carries the full argument; the point to preserve here
// is that anything registered BEFORE this one produces requests that never
// appear in the log, and nothing anywhere reports that.
//
// It calls the app's ErrorHandler itself, which is what puts a status on the
// line.
//
// On a non-nil chainErr the middleware calls c.App().ErrorHandler(c, chainErr)
// directly, falls back to a bare 500 if that returns an error of its own, and
// then returns nil. Three consequences, none of them visible from config.yaml:
//
//   - The status the log line reports is read AFTER that call, so a failed
//     or panicking request is logged as the 500 the client actually received.
//     Were the error merely returned and handled a layer out, this middleware
//     would read the status first and log a 200 for a request that failed.
//   - router.New's errorHandler therefore runs INSIDE this middleware, not
//     above it, so its "unhandled error" line is written BEFORE the request
//     line — and, for a panic, after stackTraceHandler's. Three lines,
//     innermost first, tied together by the request id.
//   - Because nil is returned, the error does not reach Fiber's own handling
//     and errorHandler is not called twice. Registering this middleware
//     elsewhere does not change that either: Fiber would then make the one
//     call instead.
type ZerologConfig struct {
	// Embedded as a POINTER, so both the wrapper and the embedded struct can
	// be nil independently — hence the two-part nil check in Validate.
	// Embedding also promotes the fields (c.Fields, not
	// c.ZerologConfig.Config.Fields), which is why a nil embedded pointer
	// panics on field access rather than at the method call.
	*fiberzerolog.Config

	// levelErrs holds one error per unusable entry in fiber.zerolog.levels,
	// recorded by NewZerologConfig and reported by Validate.
	//
	// It exists because the level list is the only key in this package whose
	// text has to be CONVERTED rather than cast, and the target type has no
	// spare value to signal failure with: zerolog.Level is an int8 whose zero
	// value is DebugLevel, a perfectly valid setting. Dropping a bad entry
	// silently would therefore not leave a detectable hole — it would shorten
	// the list, and a shortened list is not an error to the middleware, it is
	// an instruction to clamp. levels: [error, warn, typo] would become
	// [error, warn] and log every successful request at Warn forever.
	//
	// Constructors in this package never return an error, so the errors are
	// carried here instead of returned. Nothing outside Validate reads them.
	levelErrs []error
}

// parseLevel converts one entry of fiber.zerolog.levels.
//
// Written as an explicit switch rather than as a call to zerolog.ParseLevel,
// which would accept five spellings this switch does not: fatal, panic,
// disabled, the empty string, and the numeric fallback. Every one of them is
// refused because it is unsafe here, and each is named below with its reason —
// so this is an allowlist that is SHORTER than upstream's on purpose, not one
// that has fallen behind it.
//
// Those five are the whole difference. ParseLevel's own list is trace, debug,
// info, warn, error, fatal, panic, disabled, and NoLevel (which marshals to
// ""), then Atoi — so the five accepted here are spelled identically on both
// sides and no alternative spelling of any of them exists upstream. In
// particular there is no "warning": ParseLevel matches against
// LevelFieldMarshalFunc(WarnLevel), which is LevelWarnValue, which is "warn".
// A config that writes "warning" gets the default branch below, which is the
// correct answer rather than a gap.
//
// The numeric fallback: ParseLevel takes any string that strconv.Atoi accepts
// and returns Level(n) for it, so "9" parses cleanly, survives the
// middleware's NoLevel and Disabled check, reaches its switch on the level,
// and matches no case in it. The result is a response class that produces no
// log line at all, from a value that looked like a deliberate setting. An
// allowlist cannot express that failure, which is the argument for using one.
//
// The empty string is refused for the same shape of reason: it is the
// marshalled form of zerolog.NoLevel, so ParseLevel returns NoLevel with a NIL
// error for it, and the middleware treats NoLevel as "do not log". A YAML
// sequence preserves an empty entry — levels: ["", warn, info] is three
// entries, not two — so this is reachable from the file and not only from a
// hand-built slice.
//
// Matching is case-insensitive, as zerolog's own parser is. Lowercase is
// canonical and is what config.yaml and every error message here use.
func parseLevel(name string) (zerolog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "trace":
		return zerolog.TraceLevel, nil
	case "debug":
		return zerolog.DebugLevel, nil
	case "info":
		return zerolog.InfoLevel, nil
	case "warn":
		return zerolog.WarnLevel, nil
	case "error":
		return zerolog.ErrorLevel, nil

	case "fatal":
		return zerolog.NoLevel, errors.New(`"fatal" is not a usable level ` +
			`for request logging: zerolog's Fatal() calls os.Exit(1) after ` +
			`writing the line, so the first response in that class ` +
			`terminates the process`)
	case "panic":
		return zerolog.NoLevel, errors.New(`"panic" is not a usable level ` +
			`for request logging: zerolog's Panic() panics after writing ` +
			`the line, from inside the request-logging middleware itself`)
	case "disabled":
		return zerolog.NoLevel, errors.New(`"disabled" is not accepted: the ` +
			`middleware returns before writing anything, so every response ` +
			`in that class disappears from the log with nothing reporting ` +
			`it. Drop the field list or add a Skip func in code if that is ` +
			`really what is wanted`)
	case "":
		return zerolog.NoLevel, errors.New(`an empty entry reads as ` +
			`zerolog's NoLevel, which the middleware treats as "do not log" ` +
			`— that response class would disappear from the log with ` +
			`nothing reporting it`)

	default:
		return zerolog.NoLevel, fmt.Errorf("%q is not a level name: use one "+
			"of trace, debug, info, warn, error (fatal and panic end the "+
			"process, disabled silently drops the class, and a bare number "+
			"logs nothing at all)",
			name)
	}
}

// parseLevels reads a level list and converts each entry, returning the levels
// it could convert and one error per entry it could not.
//
// The nil return is load-bearing and is why this is not a loop inlined into
// the constructor. An ABSENT key must leave Levels nil, because nil is the
// only value configDefault replaces with the middleware's default; a
// made-but-empty slice is passed through untouched and panics on the first
// request. splitList already distinguishes the two — nil for a key no source
// supplies, non-nil for a stated empty list — and this function has to
// preserve that distinction rather than flatten it by unconditionally calling
// make.
//
// Bad entries are SKIPPED rather than substituted. Nothing sensible could be
// substituted: zerolog.Level is an int8 with no invalid value, and its zero
// value is DebugLevel. Skipping shortens the list, which on its own would be
// silent — the middleware reads a short list as an instruction to clamp — so
// the error accompanying each skip is the whole mechanism, and Validate must
// report it. A ZerologConfig with a non-empty levelErrs never reaches
// router.New.
func parseLevels(v *viper.Viper, key string) ([]zerolog.Level, []error) {
	raw := splitList(v, key)
	if raw == nil {
		return nil, nil
	}

	levels := make([]zerolog.Level, 0, len(raw))
	var errs []error
	for i, name := range raw {
		level, err := parseLevel(name)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s[%d]: %w", key, i, err))
			continue
		}
		levels = append(levels, level)
	}

	return levels, errs
}

// classListErrors returns the length errors for a status-class list —
// fiber.zerolog.messages or fiber.zerolog.levels — given the length it was
// read at. Both are indexed identically by the middleware and fail
// identically, so the two messages live in one place rather than being written
// twice.
//
// Returns a slice rather than a single error only to keep the call sites'
// append line uniform with the rest of Validate; at most one error is ever
// produced.
//
// n is the length of a NON-NIL list. A nil list is the documented way to
// inherit the middleware's default and never reaches here, which is why zero
// is treated as a crash rather than as absence.
func classListErrors(key string, n int) []error {
	switch {
	case n == 0:
		return []error{fmt.Errorf("%s is an empty list, which is not a way "+
			"to switch it off: the middleware clamps its class index to "+
			"len-1 and would read the slice at -1, so the first request "+
			"panics. Omit the key entirely to inherit the default",
			key)}
	case n != zerologClassCount:
		return []error{fmt.Errorf("%s must have exactly %d entries — one "+
			"for status >= 500, one for status >= 400, one for everything "+
			"else — but has %d. The middleware accepts a shorter list and "+
			"clamps to the last entry, so a single entry silently applies "+
			"to every response class. Omit the key entirely to inherit the "+
			"default",
			key,
			zerologClassCount,
			n)}
	default:
		return nil
	}
}

// NewZerologConfig reads ZerologConfig fields from the provided Viper
// instance.
//
// Never returns an error: absent keys and uncastable values both come back as
// zero values, which Validate rejects afterwards.
//
// This function is the authoritative list of keys the fiber.zerolog.* section
// supports. A key present in config.yaml but missing here is dead weight —
// Viper never looks it up, so neither the file nor an environment variable can
// supply it.
//
// # The eight fields NOT read here
//
// fiberzerolog.Config has thirteen exported fields and this constructor
// populates five. Of the remaining eight, one is supplied later and seven are
// absent:
//
//   - Seven cannot be expressed in YAML at any spelling. Next, Skip,
//     SkipField, GetResBody, GetLogger, SkipHeader, and RedactHeader are
//     funcs. No scalar, sequence, or mapping could carry any of them.
//   - Logger is not read from config, and cannot be: the application logger
//     does not exist yet. It is built in the application FROM this config (its
//     level and format depend on app.env), so the ordering is fixed —
//     config.New, then the logger, then the HTTP layer. WithLogger closes that
//     gap at the point the middleware is constructed.
//
// Six of those seven are inert when nil: Next and Skip are what would make the
// middleware skip a request, SkipField and SkipHeader what would drop a field
// or a header, GetResBody and GetLogger what would override the response body
// and the logger — every one of them is guarded by a nil check at its call
// site, so nil means the behaviour simply does not happen.
//
// REDACTHEADER IS THE EXCEPTION and it matters. Nil there is not "no
// redaction": configDefault replaces it with redactSensitiveHeader on the copy
// the middleware keeps, which masks nine credential-bearing names —
// authorization, proxy-authorization, cookie, set-cookie, location, x-api-key,
// x-auth-token, x-csrf-token, x-xsrf-token — with "[REDACTED]" wherever
// reqHeaders or resHeaders is in the Fields list. Leaving it nil is therefore
// the SAFE setting, and supplying one at the registration site REPLACES the
// default rather than extending it: a func written to cover one house-specific
// header silently stops redacting those nine. Changing any of the seven means
// editing this file; changing this one means re-listing the nine as well.
//
// # Messages and Levels
//
// Both could be left out of this constructor entirely and inherited from the
// middleware. What reading them adds is the ability to STATE the message text
// and the per-class levels in config.yaml rather than inherit them, and to
// deviate deliberately — a service fronted by a gateway that already alerts on
// 5xx may want its own message text, and a noisy environment may want 4xx at
// Info.
//
// The hazard in making them settable is real and is answered by Validate
// rather than by leaving them out. Every shape that would quietly break
// request logging is a startup error: a list of the wrong length, an
// unrecognised level name, and the three level names — fatal, panic, disabled
// — that zerolog parses happily and that would end the process or delete a
// whole response class from the log. What is left settable cannot silently
// stop the service logging.
//
// One hazard is NOT closed and cannot be from here. The middleware writes
// through the application logger, and logger.New floors that logger at Info
// for every env except development. A level below the floor is dropped by
// zerolog itself, so levels: [error, warn, debug] in production logs 5xx and
// 4xx and nothing else — no error, no warning, and a String line at startup
// that shows the setting as configured. ZerologConfig.Validate never sees
// app.env, so the check would have to live in validateCrossSection, and it
// would be pinned to logger.New's hard-coded floor rather than to anything
// either file states. Until it exists, the rule is: nothing below info outside
// development.
//
// fiber.zerolog.levels is the one key that cannot fully use that route — its
// text has to be converted, and zerolog.Level has no value meaning "not a
// level" — so its conversion failures are stashed in levelErrs and reported by
// Validate along with everything else.
func NewZerologConfig(v *viper.Viper) *ZerologConfig {
	levels, levelErrs := parseLevels(v, "fiber.zerolog.levels")

	return &ZerologConfig{
		levelErrs: levelErrs,
		Config: &fiberzerolog.Config{
			// Fields is the ordered list of request/response attributes
			// written to the structured log entry for every HTTP request. The
			// order here determines the order of keys in the log output.
			//
			// Available field names (constants in contrib zerolog v1.1.3 —
			// unknown names are silently ignored, so a typo produces no field
			// and no error):
			//   error         — handler error message (only added when
			//                   non-nil, and written through zerolog's own
			//                   Err(), so it is the one field FieldsSnakeCase
			//                   cannot rename)
			//   status        — HTTP response status code
			//   url           — original request URL including query string
			//   path          — request path without the query string
			//   route         — matched route pattern (e.g. "/api/users/:id")
			//   method        — HTTP method (GET, POST, ...)
			//   ip            — client IP (respects TrustProxy / ProxyHeader)
			//   ips           — raw X-Forwarded-For header, unparsed and NOT
			//                   gated on TrustProxy, unlike ip above
			//   host          — Host header with any port stripped
			//                   (Hostname(), not the raw header)
			//   port          — the CLIENT's port, read off the connection's
			//                   remote address. Not the port the server is
			//                   listening on, despite how the name reads; "0"
			//                   when there is no remote address and "" on a
			//                   unix socket
			//   protocol      — "http" or "https"
			//   referer       — Referer request header
			//   ua            — User-Agent request header
			//   latency       — total request processing duration, formatted
			//   requestId     — value of the X-Request-ID RESPONSE header, set
			//                   by the requestid middleware, which router.New
			//                   registers between this one and recover. The
			//                   header name is read from a CONSTANT here, not
			//                   from fiber.requestid.header, so renaming that
			//                   key empties this field; the pairing is
			//                   rejected at startup in
			//                   Cross-section validation. The field is
			//                   ALWAYS emitted — as an empty string when no
			//                   header is present — so log queries can filter
			//                   on requestId != "" to find traced requests
			//   queryParams   — query-string parameters
			//   bytesReceived — request body size in bytes
			//   bytesSent     — response body size in bytes
			//   body          — request body may contain credentials/PII
			//   resBody       — response body doubles peak memory on large
			//                   payloads (note the name: NOT "responseBody")
			//   reqHeaders    — every request header, one log key each,
			//                   EXCEPT the values RedactHeader masks — see the
			//                   constructor comment for the nine names it
			//                   covers by default, Authorization and Cookie
			//                   among them
			//   resHeaders    — every response header, same treatment;
			//                   Set-Cookie is masked by that same default
			//   pid           — server process ID
			//
			// Never enable "body" or "resBody" in production without scrubbing
			// sensitive fields from the log pipeline first (e.g. a Loki
			// pipeline_stage.replace rule for "password", "token"). Neither
			// goes through RedactHeader — that default covers header VALUES
			// only, so a credential in a form post or an error envelope is
			// logged in full.
			//
			// The two header fields are partly covered and not safe. The
			// default list is nine well-known names, so a bearer token under a
			// house-specific header (X-Internal-Token, and so on) is logged
			// verbatim, and covering it means a RedactHeader at the
			// registration site — which REPLACES the default rather than
			// extending it.
			//
			// The template's list — error, status, url, method, ip, latency,
			// requestId — is the useful minimum: what was asked for, what
			// happened, how long it took, who asked, and which request it was.
			// requestId is the last of those and the only one a client can
			// quote back, since the same value went out in the response
			// header.
			//
			// Read through splitList, which is why COMMAS OR SPACES both work
			// from the environment:
			//
			//	FIBER_ZEROLOG_FIELDS=status,method,latency     three fields
			//	FIBER_ZEROLOG_FIELDS="status method latency"   three fields
			//
			// That helper exists precisely because the comma spelling is the
			// one an operator reaches for and the one Viper alone gets wrong:
			// GetStringSlice casts a bare environment string with
			// strings.Fields, so "status,method,latency" would arrive as a
			// single element. The failure would be silent twice over — the
			// middleware ignores names it does not recognise, and Validate
			// only rejects an empty list — so the result would be a live
			// service logging every request with none of these attributes and
			// no error anywhere. TestZerologFieldsFromEnvironment pins the
			// comma form at three entries; see splitList in splitlist.go for
			// the full argument. From a YAML sequence it is a non-issue either
			// way: a real list arrives as []interface{} and casts correctly.
			Fields: splitList(v, "fiber.zerolog.fields"),

			// FieldsSnakeCase converts multi-word field names to snake_case in
			// the log output when true. The middleware renames SEVEN fields
			// under this flag — resBody, queryParams, bytesReceived,
			// bytesSent, requestId, reqHeaders, resHeaders — so what it
			// actually costs depends on which of those the Fields list above
			// enables.
			//
			// A Fields list carrying requestId and none of the other six
			// therefore sees this key rename requestId alone. Add bytesSent or
			// resHeaders to that list and it silently starts renaming those
			// too.
			//
			// It is a single switch, and only because two sites outside the
			// middleware cooperate: router.New's errorHandler and
			// stackTraceHandler write their own request-id field and take its
			// name from RequestIDField below. A literal at either site makes
			// this a half-switch — a query for request_id finds the
			// middleware's line for a failed request but not the error or
			// panic line explaining it.
			FieldsSnakeCase: v.GetBool("fiber.zerolog.fields_snake_case"),

			// WrapHeaders nests headers into a sub-object in the log entry
			// when true, instead of splatting one top-level key per header.
			//
			// The sub-object takes the FIELD's name, not a shared "headers"
			// key: reqHeaders becomes {"reqHeaders": {"header-key": "value"}}
			// and resHeaders its own {"resHeaders": {...}} beside it. With
			// FieldsSnakeCase also on, those become req_headers and
			// res_headers. There is no combined object and no level at which
			// the two merge.
			//
			// Only relevant when "reqHeaders" and/or "resHeaders" are in the
			// Fields list. With neither there, this key does nothing either
			// way.
			WrapHeaders: v.GetBool("fiber.zerolog.wrap_headers"),

			// Messages is the text of the log line, chosen by response class:
			// index 0 for status >= 500, index 1 for status >= 400, index 2
			// for everything else. It is the "message" field of the entry, not
			// a field in the Fields list above, and it is the only part of a
			// request log line that is not derived from the request.
			//
			// OPTIONAL, unlike Fields. Omitting the key leaves this nil, and a
			// nil Messages is the one shape configDefault replaces — with
			// {"Server error", "Client error", "Success"}, which is what
			// the same three a file would normally state. Inheriting and
			// stating therefore agree here, which is exactly why this key is
			// allowed to be absent and Fields is not: the middleware's default
			// field list DIFFERS from any list a file states, so inheriting it
			// there means running a configuration no file describes.
			//
			// A PRESENT key must have exactly three entries; Validate rejects
			// every other length. The middleware accepts fewer and clamps, and
			// clamping is the trap — one entry means one message for all three
			// classes, which reads like a filter and is not one. Zero entries
			// is worse: len(Messages)-1 is -1, the middleware indexes
			// Messages[-1] without a guard, and the FIRST request panics.
			// Whether that ends the process or returns a 500 depends only on
			// where router.New puts this middleware relative to recover, which
			// is not a distinction worth relying on. Validate makes it
			// unreachable.
			//
			// From the ENVIRONMENT this key needs COMMAS. Its values contain
			// spaces, and the space-separated form is split on whitespace:
			//
			//	FIBER_ZEROLOG_MESSAGES="Server error,Client error,Success"
			//		three
			//	FIBER_ZEROLOG_MESSAGES="Server error Client error Success"
			//		six
			//
			// The six-entry form fails Validate rather than starting, so the
			// mistake costs a restart and not a silent misconfiguration. The
			// cost of the comma spelling is that a message may not itself
			// contain a comma when set from the environment; from config.yaml
			// it may, because splitList only reaches its comma path for a
			// value GetString could read as a string, and a YAML sequence is
			// not one.
			Messages: splitList(v, "fiber.zerolog.messages"),

			// Levels is the zerolog level each class is logged at, indexed the
			// same way as Messages: index 0 for status >= 500, index 1 for
			// status >= 400, index 2 for everything else.
			//
			// OPTIONAL and length-checked on the same terms as Messages, for
			// the same reasons — including the empty-list panic, which is the
			// identical unguarded Levels[-1] read, and which comes FIRST in
			// the middleware: the level is resolved and indexed before the
			// NoLevel/Disabled early return and before Messages is touched at
			// all, so an empty levels list panics even when messages is fine.
			// Omitted, it inherits Error/Warn/Info.
			//
			// Accepted spellings are trace, debug, info, warn, and error,
			// matched case-insensitively; lowercase is canonical. That list is
			// deliberately SHORTER than what zerolog.ParseLevel accepts, and
			// parseLevel below is a switch rather than a call to it for that
			// reason. Five spellings ParseLevel would take are refused here:
			//
			//	fatal        writes the line, then calls os.Exit(1)
			//	panic        writes the line, then panics
			//	disabled     writes nothing, silently, for that whole class
			//	""           NoLevel, which the middleware reads as
			//               "do not log"
			//	a bare number  parses, then matches nothing — see parseLevel
			//
			// None of those is a logging level in any useful sense at this
			// call site, and none is distinguishable from a working
			// configuration until a response in that class arrives — at which
			// point two of them end the process and three simply stop logging.
			//
			// Read as text and converted, so failures are collected in
			// levelErrs rather than returned. Single words either way, so
			// commas and spaces both work from the environment:
			//
			//	FIBER_ZEROLOG_LEVELS=error,warn,info
			//	FIBER_ZEROLOG_LEVELS="error warn info"
			Levels: levels,
		},
	}
}

// WithLogger returns the middleware configuration with the application logger
// installed. It is the only supported way to build the value handed to
// fiberzerolog.New:
//
//	app.Use(fiberzerolog.New(cfg.Zerolog.WithLogger(log)))
//
// # Mechanics
//
// The returned value is a shallow copy: Fields still shares its backing array
// with the stored config. Safe only because nothing writes to it — the same
// assumption router.New makes when it copies cfg.Fiber.Config.
//
// log is taken BY VALUE and the address of that local copy is stored, so the
// middleware holds a snapshot. A caller that later reassigns its own logger
// variable cannot retroactively change what the middleware writes through.
//
// Bypassing this call is not a crash and that is the hazard: the middleware
// substitutes its own package-level zerolog.New(os.Stderr).With().Timestamp().
// Logger() for a nil Logger, so the service ends up with two loggers and two
// formats and no error anywhere.
//
// Panics if c or c.Config is nil, like every other promoted-field access on
// this type. Validate rejects both at startup, well before router.New runs.
func (c *ZerologConfig) WithLogger(log zerolog.Logger) fiberzerolog.Config {
	out := *c.Config
	out.Logger = &log
	return out
}

// RequestIDField returns the key the request id is logged under, given the
// current FieldsSnakeCase setting: "requestId" when false, "request_id" when
// true.
//
// It exists so that FieldsSnakeCase has exactly one meaning across the
// service. The middleware renames its own field from a table of constants when
// the flag is set, but router.New writes the same id on two more lines —
// errorHandler's and stackTraceHandler's — and those are ordinary zerolog
// calls that no middleware setting can reach. Spelling the name as a literal
// at those two sites makes the flag a half-switch: flipping it renames the
// request line and leaves the two lines EXPLAINING a failed request under the
// other key, so a query for request_id finds the 500 and not the stack trace
// behind it. Both handlers take their key from this method, so the three move
// together or not at all.
//
// The two spellings are the middleware's own, not a general camelCase-to-snake
// conversion — fiberzerolog switches on a fixed set of seven fields and
// substitutes a constant for each. Nothing here would survive that table being
// replaced by an algorithm, which is why the false branch reads
// fiberzerolog.FieldRequestID rather than repeating the string: half of this
// pair is pinned to upstream, and the compiler notices if it moves. The true
// branch cannot be — fieldRequestID_ is unexported — so
// TestSnakeCaseRenamesEveryRequestIDField covers it instead.
//
// Panics if c or c.Config is nil, like every other promoted-field access on
// this type. Validate rejects both at startup, well before router.New runs.
func (c *ZerologConfig) RequestIDField() string {
	if c.FieldsSnakeCase {
		return "request_id"
	}
	return fiberzerolog.FieldRequestID
}

// Validate returns a joined error for every invalid or missing ZerologConfig
// field.
//
// Deliberately unchecked:
//
//   - Next, Skip, SkipField, GetResBody, GetLogger, and SkipHeader. All are
//     nil here by design and stay nil for the life of the process, and every
//     one is guarded by a nil check at its call site, so nil means the
//     behaviour does not happen. None can be expressed in YAML.
//   - RedactHeader. Nil here like the six above, and unchecked for the same
//     reason but NOT the same default. The middleware's configDefault replaces
//     nil with redactSensitiveHeader on the copy it keeps, so the nil this
//     method sees is the setting that redacts, not the one that does not.
//     There is nothing here to reject either way.
//   - FieldsSnakeCase and WrapHeaders. A bool has no invalid value: both
//     settings are meaningful and an absent key reads as false, which is the
//     documented default. There is nothing a check could reject.
//   - The CONTENTS of Fields. Only the length is checked. The middleware
//     silently ignores names it does not recognise.
//   - Logger. It is nil here by design and stays nil for the life of the
//     process; WithLogger supplies it to a copy at the point of use. Whether
//     router.New actually makes that call is equally unchecked and equally
//     invisible from here — skipping it is not a crash, it substitutes the
//     middleware's own package-level stderr logger and splits the service's
//     output across two formats.
//   - The ABSENCE of Messages or Levels. Either may be nil, and nil is the one
//     value configDefault replaces — with {"Server error", "Client error",
//     "Success"} at Error/Warn/Info, which are also the values a file
//     would normally state. Requiring them would be asserting a constant
//     against itself. Their CONTENTS, when present, are checked below and
//     heavily.
//
// # Fields
//
// The length check on Fields is load-bearing, though not by the route the
// middleware's own defaulting suggests. configDefault restores the default
// field list only when Fields is NIL, and the two failure shapes differ on
// exactly that:
//
//   - An ABSENT key produces nil. v.Get returns nil for a key no source
//     supplies, cast.ToStringSlice fails that cast and discards its error, and
//     GetStringSlice hands back a nil slice. Left to the middleware, that
//     would quietly become {ip, latency, status, method, url, error} — a
//     working request log, but not the one config.yaml describes and not the
//     one String reports.
//   - An EMPTY key produces a non-nil empty slice, which the middleware does
//     NOT replace. A YAML `fields: []` casts to []string{}, and splitList's
//     comma path returns a made-but-empty slice for a value like ",". Either
//     configures the middleware with zero fields: a bare message per request,
//     a service that is up and has quietly stopped recording what it served.
//
// One length check covers both, since len() cannot tell them apart and neither
// is acceptable. What it enforces is that the field list is STATED rather than
// inherited — the same choice fiber.requestid.header makes, for the same
// reason.
//
// The nil-versus-made-but-empty distinction the two bullets rest on is a
// property of spf13/cast, not an assumption: toSliceEOk returns (nil, true,
// err) for a nil input, and make([]string, 0) — non-nil — for an empty YAML
// sequence. TestSplitList pins both, including that the absent-key result is
// nil rather than merely length zero, which is what parseLevels below depends
// on.
//
// # Messages and Levels
//
// Both are optional and both are strict once present. The three checks each
// one gets are not interchangeable and none of them is about taste:
//
//   - LENGTH ZERO is a crash. The middleware computes its index from the
//     status class and clamps it with `if i >= len(list) { i = len(list) - 1
//     }`, which for an empty list yields -1 and indexes the slice at -1 with
//     no guard. That is a panic on the first request served, from a value —
//     `messages: []` — that looks like a way to turn something off. Nothing
//     else in this section fails that quickly or that loudly, and it is the
//     only check here preventing a crash rather than a silence.
//   - LENGTH OTHER THAN THREE is accepted by the middleware and clamped,
//     which is the same trap wearing a working configuration. levels: [error]
//     does not mean "log errors only"; it means every response, 200s included,
//     is logged at Error. Nothing in the log would look wrong, because Error
//     lines are what a log is expected to contain.
//   - LEVEL SPELLINGS are converted in the constructor and their failures
//     arrive here in levelErrs. When there are any, the length check for
//     levels is SKIPPED rather than merely reported after them: parseLevels
//     drops what it cannot convert, so one typo in a three-entry list produces
//     a list of length two, and a length error on top would be describing a
//     symptom of the typo while naming a count the operator never wrote.
//
// Messages entries are additionally required to be non-blank. An empty message
// is not a crash and not a silence — the line is still written, with every
// field intact and `"message":""` where the class belongs. It is cheap to
// reject and there is no reading under which it was intended.
//
// Every check appends rather than returning early, so one restart surfaces
// every fiber.zerolog.* problem at once.
func (c *ZerologConfig) Validate() error {
	// errs is declared before the nil check only so the two statements read in
	// the same order in all Validate implementations; the nil check is what
	// must come first, since every line after it dereferences c.
	var errs []error
	if c == nil || c.Config == nil {
		return errors.New("fiber.zerolog config was not initialised")
	}
	if len(c.Fields) == 0 {
		errs = append(errs, errors.New("fiber.zerolog.fields is required: an "+
			"empty list is not the middleware's default list, it is a "+
			"request log with no fields at all"))
	}

	errs = append(errs, c.levelErrs...)

	if c.Messages != nil {
		errs = append(errs, classListErrors(
			"fiber.zerolog.messages", len(c.Messages))...)
		for i, m := range c.Messages {
			if strings.TrimSpace(m) == "" {
				errs = append(errs, fmt.Errorf("fiber.zerolog.messages[%d] "+
					"is blank: the line is still written, with every field "+
					"intact and an empty message where the response class "+
					"belongs",
					i))
			}
		}
	}

	switch {
	case len(c.levelErrs) > 0:
		// Length check suppressed on purpose. parseLevels DROPS what it cannot
		// convert, so every rejected entry also shortens the list: a
		// three-entry list with one typo in it arrives here at length two, and
		// a second error announcing that it must have three would be
		// describing a symptom of the first one while pointing at a count the
		// operator did not write.
	case c.Levels != nil:
		errs = append(errs, classListErrors(
			"fiber.zerolog.levels", len(c.Levels))...)
	}

	return errors.Join(errs...)
}

// String returns a loggable representation of ZerologConfig.
//
// The pointer receiver means fmt only picks this up for a *ZerologConfig.
// Printing a value copy (%v on ZerologConfig, not &ZerologConfig) bypasses it
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
// Messages and Levels print "(middleware default)" when nil rather than "[]".
// The distinction is the whole meaning of those two keys — nil inherits
// ({"Server error", "Client error", "Success"} and Error/Warn/Info
// respectively), while an empty slice is a startup error — and "[]" would
// report the two identically, in the one line an operator reads to confirm
// what the process actually loaded. Fields keeps %v because it has no such
// split: it is required, so it is never nil in a config that started.
//
// Levels renders as names rather than numbers because zerolog.Level has a
// String method on a value receiver, so %v resolves it per element: [error
// warn info], not [3 2 1]. Messages uses %q because its entries contain spaces
// and an unquoted [Server error Client error Success] cannot be read back as
// three.
func (c *ZerologConfig) String() string {
	if c == nil {
		return "<nil ZerologConfig>"
	}
	if c.Config == nil {
		return "<uninitialised ZerologConfig>"
	}
	messages := fmt.Sprintf("%q", c.Messages)
	if c.Messages == nil {
		messages = "(middleware default)"
	}

	levels := fmt.Sprintf("%v", c.Levels)
	if c.Levels == nil {
		levels = "(middleware default)"
	}

	return fmt.Sprintf("Fields=%v "+
		"FieldsSnakeCase=%t "+
		"WrapHeaders=%t "+
		"Messages=%s "+
		"Levels=%s",
		c.Fields,
		c.FieldsSnakeCase,
		c.WrapHeaders,
		messages,
		levels,
	)
}
