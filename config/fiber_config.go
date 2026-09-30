package config

// The fiber.* section of config.yaml.
//
// Keys in this section, with their environment-variable spellings:
//
//	fiber.body_limit
//		FIBER_BODY_LIMIT
//	fiber.case_sensitive
//		FIBER_CASE_SENSITIVE
//	fiber.concurrency
//		FIBER_CONCURRENCY
//	fiber.disable_default_content_type
//		FIBER_DISABLE_DEFAULT_CONTENT_TYPE
//	fiber.disable_default_date
//		FIBER_DISABLE_DEFAULT_DATE
//	fiber.disable_head_auto_register
//		FIBER_DISABLE_HEAD_AUTO_REGISTER
//	fiber.disable_header_normalizing
//		FIBER_DISABLE_HEADER_NORMALIZING
//	fiber.disable_keepalive
//		FIBER_DISABLE_KEEPALIVE
//	fiber.disable_pre_parse_multipart_form
//		FIBER_DISABLE_PRE_PARSE_MULTIPART_FORM
//	fiber.enable_ip_validation
//		FIBER_ENABLE_IP_VALIDATION
//	fiber.enable_splitting_on_parsers
//		FIBER_ENABLE_SPLITTING_ON_PARSERS
//	fiber.get_only
//		FIBER_GET_ONLY
//	fiber.idle_timeout
//		FIBER_IDLE_TIMEOUT
//	fiber.immutable
//		FIBER_IMMUTABLE
//	fiber.max_ranges
//		FIBER_MAX_RANGES
//	fiber.pass_locals_to_context
//		FIBER_PASS_LOCALS_TO_CONTEXT
//	fiber.pass_locals_to_views
//		FIBER_PASS_LOCALS_TO_VIEWS
//	fiber.proxy_header
//		FIBER_PROXY_HEADER
//	fiber.read_buffer_size
//		FIBER_READ_BUFFER_SIZE
//	fiber.read_timeout
//		FIBER_READ_TIMEOUT
//	fiber.reduce_memory_usage
//		FIBER_REDUCE_MEMORY_USAGE
//	fiber.request_methods
//		FIBER_REQUEST_METHODS
//	fiber.server_header
//		FIBER_SERVER_HEADER
//	fiber.skip_unmatched_routes
//		FIBER_SKIP_UNMATCHED_ROUTES
//	fiber.stream_request_body
//		FIBER_STREAM_REQUEST_BODY
//	fiber.strict_routing
//		FIBER_STRICT_ROUTING
//	fiber.trust_proxy
//		FIBER_TRUST_PROXY
//	fiber.trust_proxy_config.proxies
//		FIBER_TRUST_PROXY_CONFIG_PROXIES
//	fiber.trust_proxy_config.loopback
//		FIBER_TRUST_PROXY_CONFIG_LOOPBACK
//	fiber.trust_proxy_config.link_local
//		FIBER_TRUST_PROXY_CONFIG_LINK_LOCAL
//	fiber.trust_proxy_config.private
//		FIBER_TRUST_PROXY_CONFIG_PRIVATE
//	fiber.trust_proxy_config.unix_socket
//		FIBER_TRUST_PROXY_CONFIG_UNIX_SOCKET
//	fiber.unescape_path
//		FIBER_UNESCAPE_PATH
//	fiber.views_layout
//		FIBER_VIEWS_LAYOUT
//	fiber.write_buffer_size
//		FIBER_WRITE_BUFFER_SIZE
//	fiber.write_timeout
//		FIBER_WRITE_TIMEOUT
//
// Thirty-six keys, and that is the whole section — NewFiberConfig below reads
// exactly these. The environment spelling holds only for a Viper built by
// NewViper; see the package documentation.
//
// NewFiberConfig also reads app.name and app.version, which belong to another
// section and are listed in app_config.go. They are the two halves of AppName
// and are read here rather than passed in because every New*Config takes the
// same one argument. Nothing else in this package reads outside its own
// section; if a second case appears, passing *AppConfig is the change to make
// rather than adding a third exception to this note.

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/bytedance/sonic"
	"github.com/gofiber/fiber/v3"
	"github.com/spf13/viper"
)

// FiberConfig wraps fiber.Config so it participates in the standard
// Validate/String lifecycle used by all other sub-configs.
type FiberConfig struct {
	// Embedded as a POINTER, so both the wrapper and the embedded struct can
	// be nil independently — hence the two-part nil check in Validate.
	// Embedding also promotes the fields (c.AppName, not
	// c.FiberConfig.AppName), which is why a nil embedded pointer panics on
	// field access rather than at the method call.
	*fiber.Config
}

// buildTime is injected at link time via -ldflags and appended to AppName so
// the running binary version is visible in the Fiber startup banner —
// and, wherever fiber.listen.disable_startup_message suppresses that
// banner, in the "configuration loaded" startup line instead, which
// logs FiberConfig.String and so carries AppName on every start. It does
// NOT appear in the Server response header — Fiber sets that header from
// fiber.Config.ServerHeader verbatim, which is a separate string carried from
// fiber.server_header, and omits the header entirely when that value is empty.
//
// Typical injection in a Makefile or CI script. The path is the FULL import
// path of THIS package, not the consuming application's — a bare
// "config.buildTime", or the application's own module path, matches no
// symbol here:
//
//	MOD=github.com/mas-ony/go-toolkit/config
//	go build -ldflags "-X $MOD.buildTime=$(date -u +%Y%m%d%H%M%S)" .
//
// It fails quietly: go build accepts -X for a symbol that does not exist
// without complaining, so a wrong or stale path produces a binary with no
// build stamp and no error.
//
// Built without -ldflags (e.g. during local development with "go run"),
// buildTime is the empty string and the suffix is simply omitted — AppName
// becomes "<name> 1.0" rather than "<name> 1.0.<timestamp>".
var buildTime string

// NewFiberConfig reads FiberConfig fields from the provided Viper instance.
//
// Never returns an error: absent keys and uncastable values both come back as
// zero values, which Validate rejects afterwards.
//
// This function is the authoritative list of keys the fiber.* section
// supports. A key present in config.yaml but missing here is dead weight —
// Viper never looks it up, so neither the file nor an environment variable can
// supply it.
//
// AppName is assembled from app.name + app.version + buildTime so the startup
// banner and the configuration line logged at startup identify the exact
// build that is running.
//
// sonic is used as the JSON encoder/decoder in place of the standard library
// for improved throughput on large request/response payloads. JSON only — see
// the codec note below for what that does NOT cover.
//
// # The seventeen fields NOT read here
//
// fiber.Config has fifty-two fields and this constructor populates thirty-five
// (the thirty-six keys above collapse to thirty-two fields, since
// fiber.trust_proxy_config.* is one struct, plus AppName, JSONDecoder, and
// JSONEncoder). The remaining seventeen split four ways, and only the first
// two BULLETS — three fields — change what a request sees:
//
//   - Views and StructValidator are interfaces with real consequences when
//     nil. A nil Views means there is no template engine, which is what makes
//     fiber.views_layout above a live key feeding a field that does nothing —
//     it is kept for completeness of the section, not because it acts. A nil
//     StructValidator means c.Bind() performs NO validation beyond decoding,
//     so every field rule lives in the handler and service layers rather than
//     in a tag.
//   - ErrorHandler is supplied LATER, not skipped. It is a func closing over a
//     logger that does not exist at config time, so the application assigns it
//     on the copy it takes of this struct — the same shape as
//     ZerologConfig.WithLogger and RecoverConfig.WithStackTraceHandler, and
//     the same hazard if the assignment is ever dropped: not a crash, but
//     Fiber's own DefaultErrorHandler, which answers in plain text rather than
//     in the application's own error envelope. Unlike those two, there is no
//     With* method forcing the call — the assignment is a bare line in the
//     application and nothing here can see it.
//   - Six are the OTHER codecs: XMLEncoder, XMLDecoder, CBOREncoder,
//     CBORDecoder, MsgPackEncoder, and MsgPackDecoder. All funcs, cannot be
//     expressed in YAML at any spelling. Fiber supplies a working default for
//     each. Worth naming because "sonic is the codec" is easy to read as
//     covering every content type: it covers c.JSON and c.Bind().JSON, and
//     nothing else. A service that serves only JSON leaves the others
//     unreachable rather than slow.
//   - The remaining eight are structural or unused: Services,
//     ServicesStartupContextProvider, and ServicesShutdownContextProvider
//     are Fiber v3's service-dependency feature, and SharedStorage and
//     SharedStatePrefix its storage-backed shared state (app.SharedState),
//     neither of which anything here wires; RegexHandler is a func;
//     ColorScheme is a struct of ANSI escape strings for the startup banner,
//     which fiber.listen.disable_startup_message can turn off entirely; and
//     CompressedFileSuffixes is a map, meaningful only for static file
//     serving.
//
// Adding any of them means Go code at a call site rather than a key here.
func NewFiberConfig(v *viper.Viper) *FiberConfig {
	appName := v.GetString("app.name") + " " + v.GetString("app.version")
	if buildTime != "" {
		appName += "." + buildTime
	}

	return &FiberConfig{
		Config: &fiber.Config{
			// AppName is shown in the Fiber startup banner, unless
			// fiber.listen.disable_startup_message turns that off, and in
			// the "configuration loaded" startup line, which logs String
			// below. It is not the Server response header; see ServerHeader
			// for that.
			AppName: appName,

			// BodyLimit is the maximum allowed request body size in bytes.
			// Requests exceeding this receive 413 Request Entity Too Large.
			//
			// It bounds the WHOLE request, not the file inside it, so on any
			// upload route it has to cover fileutil.MaxFileBytes plus the
			// multipart envelope. The application's cross-section
			// validation enforces that floor, since it is the only place
			// both numbers are in scope.
			//
			// Fiber's own default of 4194304 (4 MiB) is therefore NOT usable
			// under any per-file limit near it or above — it falls below the
			// floor and fails startup validation. An absent key reads as 0,
			// which Fiber would replace with that same 4 MiB, so the missing
			// key fails for the same reason rather than needing a separate "is
			// required" check. See the note in Validate.
			BodyLimit: v.GetInt("fiber.body_limit"),

			// CaseSensitive distinguishes "/Foo" from "/foo" when true.
			// Most REST APIs leave this false for predictable behaviour.
			CaseSensitive: v.GetBool("fiber.case_sensitive"),

			// Concurrency is the maximum number of simultaneous connections.
			// 0 selects Fiber's default, 262144 (256 × 1024). Tune it to the
			// file descriptors available (ulimit -n).
			Concurrency: v.GetInt("fiber.concurrency"),

			// DisableDefaultContentType suppresses the automatic Content-Type
			// header on responses that omit it. Leave false unless every
			// handler sets its own Content-Type explicitly.
			DisableDefaultContentType: v.GetBool(
				"fiber.disable_default_content_type"),

			// DisableDefaultDate suppresses the automatic Date header.
			// Saves a small amount of CPU; most clients and proxies expect
			// Date.
			DisableDefaultDate: v.GetBool("fiber.disable_default_date"),

			// DisableHeadAutoRegister prevents GET routes from being mirrored
			// to HEAD automatically. HTTP requires HEAD wherever GET is
			// supported (RFC 9110); keep false unless HEAD handlers are
			// registered manually.
			DisableHeadAutoRegister: v.GetBool(
				"fiber.disable_head_auto_register"),

			// DisableHeaderNormalizing skips canonical header-name formatting
			// (e.g. "content-type" stays as-is instead of "Content-Type").
			// Leave false unless integrating with a case-sensitive upstream.
			DisableHeaderNormalizing: v.GetBool(
				"fiber.disable_header_normalizing"),

			// DisableKeepalive forces a new TCP connection for every request.
			// Only useful for debugging or extremely constrained environments.
			DisableKeepalive: v.GetBool("fiber.disable_keepalive"),

			// DisablePreParseMultipartForm skips automatic multipart parsing.
			// Set true when multipart bodies are parsed manually (e.g. for
			// streaming uploads) to avoid the double-parse overhead.
			DisablePreParseMultipartForm: v.GetBool(
				"fiber.disable_pre_parse_multipart_form"),

			// EnableIPValidation makes c.IP() and c.IPs() validate addresses
			// parsed from proxy headers before returning them; c.IP() then
			// returns the first valid IP rather than the raw header value.
			//
			// The two are gated differently. c.IP() reads ProxyHeader only
			// on the trusted path — TrustProxy AND a matching
			// TrustProxyConfig — and otherwise returns the socket address,
			// which needs no validating, so there this setting changes
			// nothing. c.IPs() is not gated at all: it parses
			// X-Forwarded-For from ANY client, trusted or not, and this
			// setting filters what it returns either way. Validated or not,
			// what c.IPs() returns off the trusted path is whatever the
			// client chose to write.
			EnableIPValidation: v.GetBool("fiber.enable_ip_validation"),

			// EnableSplittingOnParsers splits comma-separated query, body, and
			// header parameters into slices automatically when parsing
			// (e.g. "?foo=bar,baz" → foo = ["bar","baz"]).
			EnableSplittingOnParsers: v.GetBool(
				"fiber.enable_splitting_on_parsers"),

			// GETOnly rejects every request that is not GET or HEAD, and
			// Validate rejects the value true outright — the one setting in
			// this section refused whatever the rest of the file says, rather
			// than for its shape or for contradicting another key.
			//
			// The rejection is fasthttp's, not the router's, and that is what
			// makes it worth a startup error. Fiber assigns this straight to
			// fasthttp.Server.GetOnly, which is read while the request is
			// being READ: readLimitBody returns ErrGetOnly for anything
			// failing !IsGet() && !IsHead(), before the router, before every
			// middleware, and before any handler. HEAD survives, which matters
			// here only because fiber.disable_head_auto_register is false and
			// every GET route is mirrored to one.
			//
			// What the client gets is not the naked connection error it looks
			// like: Fiber sets fasthttp's ErrorHandler to its own
			// serverErrorHandler, which maps ErrGetOnly to
			// ErrMethodNotAllowed, walks the Use-registered middleware with
			// non-Use routes skipped, and then calls this app's ErrorHandler.
			// So a POST comes back as a 405 in the house envelope — a
			// well-formed answer to a request no handler ever saw.
			//
			// That is the whole problem with leaving it settable. Every
			// non-GET route a service registers answers 405, and nothing in
			// the log names this key on the run where they do. Turn
			// fiber.listen.enable_print_routes on to find out what is
			// registered and the route table prints them all and says the
			// opposite of what is happening.
			//
			// There is no value of any other key that makes true coherent.
			// Narrowing fiber.request_methods to GET and HEAD to match would
			// panic at the first non-GET registration, since Fiber's Add
			// panics outright on a method outside RequestMethods, so the
			// setting is refused rather than paired with anything.
			//
			// The second effect is smaller and points the same way: fasthttp
			// documents that with GetOnly set the request is limited by
			// fiber.read_buffer_size rather than by fiber.body_limit, which is
			// typically orders of magnitude smaller.
			//
			// A deployment serving reads alone — a mirror, a cache — is the
			// one shape the value fits, and nothing in this section can tell
			// it apart from a deployment that has just lost its write half,
			// which is why the value is refused rather than reported.
			GETOnly: v.GetBool("fiber.get_only"),

			// IdleTimeout is the keep-alive idle-connection timeout. When 0,
			// fasthttp falls back to ReadTimeout; only if ReadTimeout is also
			// 0 is the idle time unlimited. Set a positive duration
			// (e.g. "75s") to reclaim idle sockets sooner and align with
			// common load-balancer idle-connection timeouts.
			IdleTimeout: v.GetDuration("fiber.idle_timeout"),

			// Immutable makes values obtained from the context (route params,
			// query values, body, etc.) immutable copies, so they remain valid
			// after the handler returns — at the cost of Fiber's
			// zero-allocation fast path. Leave false unless such values are
			// retained by goroutines that outlive the handler.
			Immutable: v.GetBool("fiber.immutable"),

			// JSONDecoder / JSONEncoder use sonic in place of encoding/json
			// for faster JSON processing (JIT-compiled codecs on amd64/arm64;
			// sonic transparently falls back to an encoding/json-compatible
			// path on other architectures).
			//
			// Trade-off: sonic.Marshal/Unmarshal use sonic's ConfigDefault,
			// which differs from encoding/json in three ways a client can
			// see. On output, HTML characters (<, >, &) are NOT escaped and
			// map keys are NOT sorted; on input, a control character left
			// unescaped inside a JSON string is ACCEPTED, where encoding/json
			// rejects the whole body. The first two do not matter to a JSON
			// API whose responses are never embedded into HTML and whose
			// clients do not depend on key order; the third makes a request
			// the standard library would refuse reach the handler. Where
			// standard-library behaviour is needed, switch to
			// sonic.ConfigStd.Marshal and sonic.ConfigStd.Unmarshal.
			//
			// sonic.Unmarshal copies its input before decoding, so decoded
			// strings never alias fasthttp's request buffer, which is
			// recycled once the handler returns.
			JSONDecoder: sonic.Unmarshal,
			JSONEncoder: sonic.Marshal,

			// MaxRanges caps the number of sub-ranges parsed from a Range
			// header; ranges beyond the cap are rejected. Zero or negative
			// values fall back to Fiber's default of 16 — there is no
			// "disable Range support" setting via this field.
			MaxRanges: v.GetInt("fiber.max_ranges"),

			// PassLocalsToContext forwards c.Locals values to the standard
			// context.Context. Only needed when middleware or libraries read
			// from context.Context instead of fiber.Ctx.
			PassLocalsToContext: v.GetBool("fiber.pass_locals_to_context"),

			// PassLocalsToViews forwards c.Locals values to the template
			// engine as variables. Irrelevant for JSON APIs that use no view
			// engine.
			PassLocalsToViews: v.GetBool("fiber.pass_locals_to_views"),

			// ProxyHeader is the header used to determine the real client IP
			// when TrustProxy is true.
			// Common values: "X-Real-IP", "X-Forwarded-For",
			// "CF-Connecting-IP"
			ProxyHeader: v.GetString("fiber.proxy_header"),

			// ReadBufferSize is the per-connection read buffer in bytes.
			// Increase if "small read buffer" errors occur due to large
			// request headers (many cookies, long Authorization header, etc.).
			ReadBufferSize: v.GetInt("fiber.read_buffer_size"),

			// ReadTimeout is the maximum time to read the full request.
			// 0 means no limit. Set a positive duration (e.g. "30s") to
			// defend against slow-read attacks.
			ReadTimeout: v.GetDuration("fiber.read_timeout"),

			// ReduceMemoryUsage trades CPU for memory: fasthttp hands a
			// connection's read and write buffers back to its pools between
			// requests, and keeps no request or response body buffer for the
			// next one, instead of holding all of them for the life of the
			// connection. fasthttp recommends it for a server whose memory
			// goes on many mostly idle keep-alive connections.
			ReduceMemoryUsage: v.GetBool("fiber.reduce_memory_usage"),

			// RequestMethods is the allowlist of HTTP methods the router
			// accepts. A request with any other method is answered 501 Not
			// Implemented before routing. A LISTED method is routed as usual:
			// 404 where no route matches the path at all, and 405 where the
			// path has routes for other methods only. Removing unused methods
			// reduces the attack surface.
			//
			// When empty, Fiber uses fiber.DefaultMethods: GET, HEAD, POST,
			// PUT, DELETE, CONNECT, OPTIONS, TRACE, PATCH, and QUERY.
			// Explicitly setting this field overrides the entire default —
			// omitting HEAD would disable it even though RFC 9110 requires
			// it. A list that names seven drops CONNECT, TRACE, and QUERY by
			// omission rather than by decision; the count is worth knowing
			// before deleting the key on the assumption that the default
			// matches what the file lists.
			//
			// Registering a route for a method the list does not name
			// PANICS at startup — "add: invalid http method GET" — naming
			// the method but neither this key nor its source. So a list that
			// misspells GET, or leaves out a method the service registers,
			// does not start. Fiber upper-cases the method of every route it
			// registers before looking it up here, so an entry that is not
			// upper-case can never match one; Validate refuses it by name
			// rather than leaving it to that panic.
			//
			// Viper note: v.GetStringSlice returns a NIL slice when the key is
			// absent — cast's toSliceEOk returns (nil, true, err) for a nil
			// input and GetStringSlice discards the error — while a stated
			// empty sequence takes cast's reflect branch and is built with
			// make, so it comes back non-nil at length zero. Fiber's check is
			// len(app.config.RequestMethods) == 0, which cannot tell the two
			// apart and falls back to DefaultMethods for both, so omitting the
			// key from config.yaml is safe and has the same effect as not
			// setting this field at all. The distinction is not idle
			// elsewhere: fiber.zerolog.levels and fiber.zerolog.fields both
			// turn on it, because the zerolog middleware restores its own
			// defaults for nil ONLY. See the nil-versus-made-but-empty note in
			// ZerologConfig.Validate, which
			// TestSplitListKeepsAbsentAndEmptyApart pins.
			//
			// Read through splitList for the same reason fiber.zerolog.fields
			// is, and the consequence here is worse. From the ENVIRONMENT a
			// bare string is cast with strings.Fields, so
			//
			//	FIBER_REQUEST_METHODS=GET,POST
			//
			// would arrive as ONE element, "GET,POST". len() is 1, so Fiber
			// does not fall back to DefaultMethods; its only method is a
			// string no client sends, and the first route the service
			// registers panics with "add: invalid http method GET" — a
			// message about a method, from a key that names two correctly.
			// splitList turns the spelling into the two methods the operator
			// meant, and Validate refuses any entry that still contains a
			// comma or a space, which is how the same mistake arrives from a
			// YAML sequence.
			RequestMethods: splitList(v, "fiber.request_methods"),

			// ServerHeader is the value of the Server response header.
			// Empty string omits the header entirely — the recommended default
			// to avoid server fingerprinting.
			ServerHeader: v.GetString("fiber.server_header"),

			// SkipUnmatchedRoutes answers a request matching no registered
			// route with 404 — or 405, when the path exists for other
			// methods — before the middleware chain runs, so bots, scanners,
			// and bad URLs cost nothing past the router lookahead.
			//
			// It is the one bool in this section whose true is a real trade
			// rather than a preference. The recommended stack (see doc.go)
			// registers FOUR middlewares with app.Use — zerolog, requestid,
			// recover, limiter — and a skipped request reaches none of them:
			// no request log line, no request id, and nothing counted
			// against the limiter's quota. What comes back is still the
			// application's envelope, because a 404 is a *fiber.Error and
			// the error handler answers those without logging, so the
			// response is indistinguishable and the log line is simply
			// absent.
			//
			// The same goes for anything that ANSWERS through app.Use on a
			// path with no route of its own — static files, a catch-all 404
			// page, a proxy. With this true, none of them runs for such a
			// path; Fiber's own documentation lists them. Check the service
			// registers nothing of the kind before turning this on.
			//
			// Preflight is exempt — Fiber checks IsPreflight before the
			// lookahead — and the fast path only arms when at least one Use
			// route exists, which is the same four it then bypasses.
			//
			// Validate rejects exactly one pairing, the silent one: Fiber
			// builds the lookahead from 64-bit method masks and abandons it
			// wholesale when len(RequestMethods) > 64, leaving this true and
			// inert. Everything else about true is documented, deliberate, and
			// the operator's call.
			//
			// That early return costs a second thing, which is not this key's
			// to lose: buildSkipIndexes sets methodMaskValid and returns
			// before it builds the 405-fallback prune mask, and that mask is
			// maintained even when this key is FALSE. So above 64 methods the
			// router loses both, and only one of them is named by the error.
			// Nothing here can reject that on its own, since a long
			// fiber.request_methods with this key false is a legal
			// configuration — it is the reason the check is worded as "trim
			// request_methods OR turn this off" rather than only the latter.
			SkipUnmatchedRoutes: v.GetBool("fiber.skip_unmatched_routes"),

			// StreamRequestBody reads the body lazily instead of buffering it
			// fully before the handler is called. Required for large file
			// uploads that would exceed BodyLimit when fully buffered.
			StreamRequestBody: v.GetBool("fiber.stream_request_body"),

			// StrictRouting treats "/foo" and "/foo/" as distinct routes when
			// true. false is the common default — trailing slashes are
			// interchangeable.
			StrictRouting: v.GetBool("fiber.strict_routing"),

			// TrustProxy allows Fiber to read X-Forwarded-For and related
			// headers when — and only when — the request arrives from an
			// address in TrustProxyConfig, a rule that is easy to configure
			// straight past:
			//
			//	TrustProxy=false
			//	    proxy headers ignored; c.IP() is the socket IP
			//	TrustProxy=true, allowlist EMPTY
			//	    proxy headers ignored; c.IP() is the socket IP
			//	TrustProxy=true, request IP in the allowlist
			//	    c.IP() reads ProxyHeader, c.Scheme() reads
			//	    X-Forwarded-Proto, c.Host() reads X-Forwarded-Host
			//
			// The middle row is the trap. Fiber's own doc for this field: "If
			// you enable TrustProxy and do not provide a TrustProxyConfig,
			// Fiber will skip all headers that could be spoofed." Setting
			// trust_proxy: true and proxy_header: X-Forwarded-For without the
			// allowlist below is therefore not a partial configuration — it is
			// identical in behaviour to leaving trust_proxy false, so every
			// c.IP() and every "ip" in the request log is the GATEWAY's
			// address, for every client, with no error anywhere.
			//
			// That matters concretely for any service behind an API gateway,
			// where the socket IP is always the gateway's. Validate rejects
			// the middle row rather than letting it ship.
			//
			// See also: ProxyHeader, EnableIPValidation.
			TrustProxy: v.GetBool("fiber.trust_proxy"),

			// TrustProxyConfig is the allowlist that makes TrustProxy do
			// anything. Ignored entirely when TrustProxy is false.
			//
			// Proxies takes IP addresses or CIDR ranges — the gateway's egress
			// address(es), not the clients'. Three of the booleans are
			// shorthands for whole classes: Loopback (127.0.0.0/8, ::1/128),
			// LinkLocal (169.254.0.0/16, fe80::/10), Private (10/8,
			// 172.16/12, 192.168/16, fc00::/7). The fourth, UnixSocket,
			// trusts every connection that arrives over a unix socket, which
			// has no peer IP to match against a list.
			//
			// Private is the pragmatic setting for a gateway on the same
			// cluster network and the one to reach for first. It is also the
			// widest: it trusts X-Forwarded-For from ANY RFC 1918 source,
			// which is right only if nothing else on that network can reach
			// the service directly. Where the gateway has a stable address,
			// list it in Proxies and leave all three false.
			//
			// Proxies goes through splitList so
			// FIBER_TRUST_PROXY_CONFIG_PROXIES=10.0.0.1,10.0.0.2 works; a
			// space-separated value works too.
			TrustProxyConfig: fiber.TrustProxyConfig{
				Proxies:    splitList(v, "fiber.trust_proxy_config.proxies"),
				Loopback:   v.GetBool("fiber.trust_proxy_config.loopback"),
				LinkLocal:  v.GetBool("fiber.trust_proxy_config.link_local"),
				Private:    v.GetBool("fiber.trust_proxy_config.private"),
				UnixSocket: v.GetBool("fiber.trust_proxy_config.unix_socket"),
			},

			// UnescapePath URL-decodes percent-encoded characters in the path
			// before route matching, so encoded special characters still hit
			// the intended route (e.g. "/caf%C3%A9" matches "/café").
			UnescapePath: v.GetBool("fiber.unescape_path"),

			// ViewsLayout is the default layout template name passed to the
			// view engine. Irrelevant for JSON APIs that use no view engine.
			ViewsLayout: v.GetString("fiber.views_layout"),

			// WriteBufferSize is the per-connection write buffer in bytes. It
			// is a buffer, not a limit: a larger response is written through
			// it in pieces, and raising it only means fewer, larger writes.
			// Increase for APIs that serve large responses or use SSE
			// streams.
			WriteBufferSize: v.GetInt("fiber.write_buffer_size"),

			// WriteTimeout is the maximum time to write the full response.
			// 0 means no limit. Set a positive duration (e.g. "30s") to
			// release sockets held open by slow or stalled clients.
			WriteTimeout: v.GetDuration("fiber.write_timeout"),
		},
	}
}

// Validate returns a joined error for every invalid or missing FiberConfig
// field.
//
// Deliberately unchecked:
//
//   - BodyLimit, here. Fiber replaces any value <= 0 with DefaultBodyLimit
//     (4 MiB), so an absent key is not a broken server — it is a 4 MiB cap
//     silently below fileutil.MaxFileBytes, which is a DIFFERENT and more
//     specific problem. The application's cross-section validation is the
//     only place both numbers are in scope, and it rejects every value a
//     check here would, with a message naming the floor and the reason. A
//     plain "fiber.body_limit is required" beside it would report one cause
//     twice.
//   - Concurrency. Same shape: Fiber replaces <= 0 with DefaultConcurrency
//     (262144), so an absent key changes nothing observable for a deployment
//     that wanted the default. Requiring it would buy a startup error for
//     a deployment that behaves identically either way.
//   - Whether each RequestMethods entry names a REAL method. Each is checked
//     to be an upper-case HTTP token, which is the grammar every method
//     shares, but "GTE" is one: it passes here, and the service then panics
//     at its first GET registration. Checking more would mean mirroring
//     fiber.DefaultMethods and refusing the custom methods this field exists
//     to add.
//   - Buffer sizes, timeouts, and every bool EXCEPT GETOnly and the
//     TrustProxy and SkipUnmatchedRoutes pairings below. Fiber has a working
//     default or a meaningful zero for each, and none of them can be invalid —
//     only unwise.
//
// What IS checked is five things. Two stand on their own: GETOnly, refused
// outright, and the shape of each RequestMethods entry, an upper-case HTTP
// token. Three are pairings — TrustProxy without an allowlist,
// SkipUnmatchedRoutes above the 64-method mask, and ProxyHeader without
// TrustProxy — and each has the same shape: a key that reads as ON in
// config.yaml, reads as ON in String below, and does nothing where it
// matters.
//
// GETOnly is the one VALUE refused outright: true is rejected whatever the
// rest of the file says, where every other check objects to a shape or to
// a contradiction between two keys. fasthttp rejects every non-GET while it
// reads the request, so every write route a service registers answers 405
// without a handler ever running — and if fiber.listen.enable_print_routes
// is on, the startup route table prints every one of them. See the field
// comment in NewFiberConfig for the mechanics and for why no other key can
// make it coherent.
//
// Every check appends rather than returning early, so one restart surfaces
// every fiber.* problem at once.
func (c *FiberConfig) Validate() error {
	// errs is declared before the nil check only so the two statements read in
	// the same order in all Validate implementations; the nil check is what
	// must come first, since every line after it dereferences c.
	var errs []error
	if c == nil || c.Config == nil {
		return errors.New("fiber config was not initialised")
	}

	// GETOnly is refused rather than paired, because there is nothing here to
	// pair it with: fasthttp rejects the request while reading it, so no key
	// in any section can make a POST reach a handler while this is true. The
	// failure is a well-formed 405 in the house envelope — see the field
	// comment — which is exactly why it needs catching at startup: nothing
	// about the response, the log line, or the printed route table says that a
	// configuration key turned the write half of the API off.
	if c.GETOnly {
		errs = append(errs, errors.New("fiber.get_only is true: fasthttp "+
			"rejects every request that is not GET or HEAD while it reads "+
			"it, before the router and before any middleware, so POST, PUT, "+
			"and DELETE would answer 405 with no handler ever running — and "+
			"fiber.listen.enable_print_routes would still list every one of "+
			"them at startup. It is coherent only where every registered "+
			"route is a read, and nothing here can tell that deployment "+
			"from one that has lost its write half, so it is refused "+
			"rather than reported: set it to false"))
	}

	// TrustProxy without an allowlist is not a partial configuration — Fiber
	// skips every spoofable header, which is exactly what TrustProxy=false
	// does. Left unchecked it is a security setting that reads as ON in
	// config.yaml, reads as ON in this struct's String output, and is OFF in
	// the only place it matters. See the field comment for the three-row
	// table.
	if c.TrustProxy &&
		len(c.TrustProxyConfig.Proxies) == 0 &&
		!c.TrustProxyConfig.Loopback &&
		!c.TrustProxyConfig.LinkLocal &&
		!c.TrustProxyConfig.Private &&
		!c.TrustProxyConfig.UnixSocket {
		errs = append(errs, errors.New("fiber.trust_proxy is true but no "+
			"trusted proxies are configured: set "+
			"fiber.trust_proxy_config.proxies to the gateway address(es), "+
			"or one of fiber.trust_proxy_config.{loopback,link_local,"+
			"private,unix_socket} — without one of these Fiber ignores "+
			"fiber.proxy_header entirely and c.IP() returns the socket "+
			"address"))
	}

	// SkipUnmatchedRoutes is not refused for what it costs — that trade is
	// documented in config.yaml and is the operator's to make. It is refused
	// for the one combination that makes it a no-op without saying so: Fiber
	// builds the unmatched-route lookahead from 64-bit method masks, and
	// buildSkipIndexes returns early when len(RequestMethods) > 64, so the key
	// reads as ON here, ON in String below, and does nothing at the router.
	// Same shape as the TrustProxy check above, and reachable the same way —
	// through a key this file exposes.
	if c.SkipUnmatchedRoutes && len(c.RequestMethods) > 64 {
		errs = append(errs, errors.New("fiber.skip_unmatched_routes is true "+
			"but fiber.request_methods lists "+
			strconv.Itoa(len(c.RequestMethods))+
			" methods: Fiber's lookahead is built from 64-bit method "+
			"masks and is abandoned entirely above 64, so every request "+
			"would go back through the full middleware chain while this "+
			"key still reads true — trim fiber.request_methods to 64 or "+
			"fewer, or set fiber.skip_unmatched_routes to false"))
	}

	// A method is an RFC 9110 token, the same grammar as a header name, so
	// isHTTPFieldName answers for both. An entry that is not one — "GET,POST"
	// or "GET POST" from a YAML sequence, where splitList never splits, or an
	// empty string — can never match a request, and the first route
	// registered for the method it was meant to hold panics at startup.
	//
	// A token that is not upper-case fails the same way by another route.
	// Fiber upper-cases the method of every route before looking it up in
	// this list, so "get" is never found, and registering a GET route panics
	// exactly as if the entry were missing.
	for i, m := range c.RequestMethods {
		switch {
		case !isHTTPFieldName(m):
			errs = append(errs, fmt.Errorf("fiber.request_methods[%d] is not "+
				"a method name (got %q): a method is a single token — "+
				"letters, digits, and %s — so write one per entry, and a "+
				"route registered for a method missing from the list "+
				"panics at startup",
				i,
				m,
				tokenSpecials))
		case m != strings.ToUpper(m):
			errs = append(errs, fmt.Errorf("fiber.request_methods[%d] must "+
				"be upper-case (got %q): Fiber upper-cases the method of "+
				"every route it registers before looking it up here, so "+
				"this entry can never match one, and registering a route "+
				"for %s panics at startup",
				i,
				m,
				strings.ToUpper(m)))
		}
	}

	// ProxyHeader is only read on the trusted path. Set without TrustProxy it
	// is inert, and the mistake is worth a startup error rather than a support
	// ticket about client IPs that never appear in the log.
	if c.ProxyHeader != "" && !c.TrustProxy {
		errs = append(errs, errors.New("fiber.proxy_header is set but "+
			"fiber.trust_proxy is false: the header is never read — enable "+
			"fiber.trust_proxy and configure fiber.trust_proxy_config, or "+
			"clear fiber.proxy_header"))
	}

	return errors.Join(errs...)
}

// String returns a loggable representation of FiberConfig.
//
// The pointer receiver means fmt only picks this up for a *FiberConfig.
// Printing a value copy (%v on FiberConfig, not &FiberConfig) bypasses it and
// dumps the struct fields directly.
//
// The nil check is TWO-PART, mirroring Validate's. A logger's own guard, such
// as zerolog's `if val == nil` before it calls a Stringer, compares an
// INTERFACE with nil, which neither a typed nil pointer nor a wrapper around
// a nil embedded pointer satisfies. Without the second half, the half-built
// one would panic inside the startup log line instead of rendering a
// placeholder.
//
// RequestMethods prints "(fiber.DefaultMethods)" when empty rather than
// "[]", because an empty list is the setting that allows every default
// method and "[]" reads as allowing none.
func (c *FiberConfig) String() string {
	if c == nil {
		return "<nil FiberConfig>"
	}
	if c.Config == nil {
		return "<uninitialised FiberConfig>"
	}
	methods := fmt.Sprintf("%v", c.RequestMethods)
	if len(c.RequestMethods) == 0 {
		methods = "(fiber.DefaultMethods)"
	}
	return fmt.Sprintf("AppName=%s "+
		"BodyLimit=%d "+
		"CaseSensitive=%t "+
		"Concurrency=%d "+
		"DisableDefaultContentType=%t "+
		"DisableDefaultDate=%t "+
		"DisableHeadAutoRegister=%t "+
		"DisableHeaderNormalizing=%t "+
		"DisableKeepalive=%t "+
		"DisablePreParseMultipartForm=%t "+
		"EnableIPValidation=%t "+
		"EnableSplittingOnParsers=%t "+
		"GETOnly=%t "+
		"IdleTimeout=%s "+
		"Immutable=%t "+
		"MaxRanges=%d "+
		"PassLocalsToContext=%t "+
		"PassLocalsToViews=%t "+
		"ProxyHeader=%s "+
		"ReadBufferSize=%d "+
		"ReadTimeout=%s "+
		"ReduceMemoryUsage=%t "+
		"RequestMethods=%s "+
		"ServerHeader=%s "+
		"SkipUnmatchedRoutes=%t "+
		"StreamRequestBody=%t "+
		"StrictRouting=%t "+
		"TrustProxy=%t "+
		"TrustProxyProxies=%v "+
		"TrustProxyLoopback=%t "+
		"TrustProxyLinkLocal=%t "+
		"TrustProxyPrivate=%t "+
		"TrustProxyUnixSocket=%t "+
		"UnescapePath=%t "+
		"ViewsLayout=%s "+
		"WriteBufferSize=%d "+
		"WriteTimeout=%s",
		c.AppName,
		c.BodyLimit,
		c.CaseSensitive,
		c.Concurrency,
		c.DisableDefaultContentType,
		c.DisableDefaultDate,
		c.DisableHeadAutoRegister,
		c.DisableHeaderNormalizing,
		c.DisableKeepalive,
		c.DisablePreParseMultipartForm,
		c.EnableIPValidation,
		c.EnableSplittingOnParsers,
		c.GETOnly,
		c.IdleTimeout,
		c.Immutable,
		c.MaxRanges,
		c.PassLocalsToContext,
		c.PassLocalsToViews,
		c.ProxyHeader,
		c.ReadBufferSize,
		c.ReadTimeout,
		c.ReduceMemoryUsage,
		methods,
		c.ServerHeader,
		c.SkipUnmatchedRoutes,
		c.StreamRequestBody,
		c.StrictRouting,
		c.TrustProxy,
		c.TrustProxyConfig.Proxies,
		c.TrustProxyConfig.Loopback,
		c.TrustProxyConfig.LinkLocal,
		c.TrustProxyConfig.Private,
		c.TrustProxyConfig.UnixSocket,
		c.UnescapePath,
		c.ViewsLayout,
		c.WriteBufferSize,
		c.WriteTimeout,
	)
}
