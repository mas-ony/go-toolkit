package httpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/client"
	"github.com/rs/zerolog"

	"github.com/mas-ony/go-toolkit/config"
)

// Backoff between attempts: doubling from the first value, never past
// the second. The cap matters more than the curve, since waiting
// minutes between attempts turns a transient blip into an apparent
// hang. No jitter is applied, so callers that fail together retry
// together.
//
// maxBackoffShift bounds the doubling itself rather than only its
// result. firstBackoff << n overflows int64 for a large enough n, and a
// negative or zero duration makes time.After fire immediately, so a
// caller that set Retries high would get a hot retry loop instead of the
// cap above. Five doublings already carry firstBackoff past maxBackoff,
// so clamping there shortens no wait the cap would have left alone.
const (
	firstBackoff    = 500 * time.Millisecond
	maxBackoff      = 8 * time.Second
	maxBackoffShift = 5
)

// maxErrorBody bounds how much of a non-2xx body is copied out of the
// response, not how much is read. The transport buffers the whole thing
// before send sees any of it, so nothing here keeps a pathological body
// from arriving; what the cap rules out is carrying it past resp.Close()
// and holding it for the length of the error path. One mebibyte is far
// more than an envelope or a proxy's error page needs, and the message a
// caller sees is shorter still: see maxMessageRunes.
const maxErrorBody = 1 << 20

// maxMessageRunes bounds the message carried on an Error, whether it came
// from the envelope or from a body that is not one. A message cut here
// gains an ellipsis, so the cap is on the text and not on the length of
// the result. See messageFrom.
const maxMessageRunes = 200

// Sentinel errors for the three statuses a caller is likely to branch
// on rather than merely report. Error.Unwrap reaches them, so errors.Is
// works on anything Do returns.
//
// What each one means is the caller's to document: a 404 from one
// endpoint is a stale id and from another a missing row, and only the
// caller knows which endpoint it just addressed.
var (
	ErrNotFound      = errors.New("not found")
	ErrConflict      = errors.New("conflict")
	ErrUnprocessable = errors.New("unprocessable")
)

// Envelope is the success/message/data shape the service answers in.
//
// Data is a type parameter rather than an any, which is the whole reason
// this type exists: a response decodes straight into the caller's type
// instead of into a map the caller then has to re-marshal. That is also
// why this is not response.Response, whose Data is an any because it is
// the type a handler ENCODES from — the same three fields read in the
// other direction.
//
// A list endpoint answers with a page, and the page IS
// response.Paginated. One definition for that shape, whichever way it is
// travelling:
//
//	var env Envelope[response.Paginated[Item]]
//
// What does not travel with the type is NewPaginated's normalisation. It
// replaces a nil slice with an empty one on the way OUT, so a service
// built on that package never sends "items":null. Decoding takes what
// arrived, and a service that does send it leaves Items nil — which
// ranges zero times, so this matters only to a caller that tests the
// slice rather than walking it.
type Envelope[T any] struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	Data    T      `json:"data"`
}

// Error carries a non-2xx response. Msg is the message the server put in
// the envelope, when it sent one, so a client has something to report
// besides a status code.
//
// Path holds whatever URL was passed to Do, which is normally the
// absolute URL that URL built rather than a bare path. It is rendered
// verbatim in Error, and Do's retry log line carries the same value at
// debug level, so a caller that puts a credential in a query parameter
// puts it in both places.
type Error struct {
	Status int
	Method string
	Path   string
	Msg    string
}

// Options carries the three settings a client needs that
// config.ClientConfig cannot hold. The zero value is usable: no suffix
// trimmed, no headers beyond the default Accept, and nothing logged.
//
// Each is absent from the fiber.client.* section for its own reason, and
// NewClientConfig carries the argument in full. In short: a header map is
// a shape the key-per-leaf config tests cannot express and the
// environment cannot carry at all; a route prefix is a fact about the far
// service's routes rather than a deployment setting, so it belongs beside
// the constant the paths are built from; and a logger is not a value any
// YAML could hold.
type Options struct {
	// TrimPathSuffix is stripped from the end of the configured base
	// URL's path when present, e.g. "/api/v1". Pasting a URL out of a
	// browser is the obvious thing to do, and silently doubling the
	// prefix produces 404s that look like a missing route rather than a
	// bad setting. A trailing slash on it is tolerated.
	TrimPathSuffix string

	// Headers are sent on every request. Accept is defaulted to
	// application/json before this map is merged, so an entry for it
	// here replaces the default rather than being ignored.
	//
	// Authorization is better set through the section's token key, which
	// is applied after this map and therefore wins over an entry here.
	Headers map[string]string

	// Log receives one debug line per retry, carrying the URL and the
	// error that provoked it. The zero value is usable and writes
	// nowhere, so a caller with nothing to log leaves it unset rather
	// than wiring a discard writer.
	Log zerolog.Logger
}

// Client is an HTTP client bound to one service.
//
// It is safe for concurrent use. Nothing on it is written after New
// returns, and each request is taken fresh off the transport, so one
// client is meant to be shared rather than built per call. Building one
// per call would also throw away the connection pool behind it.
type Client struct {
	base *url.URL
	hc   *client.Client

	retries int
	noRetry map[string]bool

	log zerolog.Logger
}

// contextErr reports a cancelled or expired context as an error that
// names the call, and that errors.Is can match against context.Canceled
// and context.DeadlineExceeded.
func contextErr(ctx context.Context, method, rawURL string) error {
	return fmt.Errorf("%s %s: %w", method, rawURL, ctx.Err())
}

// isSuccess reports whether a status is a 2xx. Do and send both ask,
// once each way round, so the boundary is written in one place.
func isSuccess(code int) bool {
	return code >= 200 && code < 300
}

// attempts names a count of tries, singular for one, since a method in
// NoRetryMethods gets exactly that.
func attempts(n int) string {
	if n == 1 {
		return "1 attempt"
	}
	return fmt.Sprintf("%d attempts", n)
}

// retryableStatus reports whether a status is worth another attempt.
func retryableStatus(code int) bool {
	switch code {
	case fiber.StatusTooManyRequests,
		fiber.StatusInternalServerError,
		fiber.StatusBadGateway,
		fiber.StatusServiceUnavailable,
		fiber.StatusGatewayTimeout:
		return true
	}
	return false
}

// condense folds a message onto one line and caps its length.
//
// The cut lands on a rune boundary, not a byte offset. A string sliced
// at a fixed offset splits whatever multi-byte character straddles it,
// and the message goes somewhere a replacement character reads as
// corruption rather than as truncation. Ranging over the string finds
// that boundary and stops there, which is the point: converting to
// []rune first would spend four bytes per character across a whole
// capped error page to keep the first two hundred of them.
//
// strings.Fields both splits and trims, so no separate TrimSpace is
// needed: leading and trailing whitespace is not carried into the join.
//
// ToValidUTF8 runs last, on the cut string, so that part is bounded by
// the cap. The fold before it is not: strings.Fields walks the whole
// body and allocates a slice header per token, so maxErrorBody is what
// sets the peak here. A body that is not text at all still has to leave
// here as something a JSON log line can carry, and it returns
// the string untouched when there is nothing to repair. It replaces a
// run of invalid bytes with one character rather than one apiece, so a
// binary body reads as a mark where the text stopped making sense
// instead of as a wall of them.
func condense(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	n := 0
	for i := range s {
		n++
		if n > maxMessageRunes {
			s = s[:i] + "..."
			break
		}
	}
	return strings.ToValidUTF8(s, "\uFFFD")
}

// messageFrom pulls the envelope's message out of an error body.
//
// A gateway or proxy in front of the service answers with HTML or plain
// text rather than the envelope, so a decode failure is expected rather
// than exceptional. The raw body is then used instead, since an
// unparsed error page is still the most informative thing available,
// but a whole page of HTML in a log line is not. A body that decodes as
// an envelope carrying no message takes the same path, and so reads as
// its own JSON.
//
// Both paths are condensed, not only the fallback. The envelope's
// message is as unbounded as the page is: nothing between the wire and
// here caps it except maxErrorBody, so a service answering with a
// mebibyte of JSON puts a mebibyte on Error.Msg, which Error renders
// into everything that touches it. Folding whitespace applies to both
// for the same reason, since a server message carrying newlines breaks
// a log line as thoroughly as an HTML page does.
func messageFrom(payload []byte) string {
	var env Envelope[json.RawMessage]
	if err := json.Unmarshal(payload, &env); err == nil {
		if env.Message != "" {
			return condense(env.Message)
		}
	}
	return condense(string(payload))
}

// send makes one attempt and hands back the status and a copy of the
// body.
//
// The copy is what the rest of Do stands on: the transport pools its
// response buffers, and Close returns the one behind Body for reuse, so
// anything still pointing at it reads whatever the next request lands
// there instead. Closing also returns the pooled request, which is why
// nothing releases it by hand on the path below that succeeds.
//
// A transport failure drops that request rather than returning it to
// the pool. Send releases it on some of its own failure paths and not
// others, and the error it hands back does not say which happened, so
// releasing here would sometimes put one object in the pool twice and
// hand it to two goroutines at once. The cost of not releasing is a
// missed recycle on exactly the path retries take; the cost of
// releasing is a data race, so the object is left to the collector.
func (c *Client) send(
	ctx context.Context,
	method, rawURL string,
	body []byte,
	contentType string,
) (int, []byte, error) {
	req := c.hc.R().SetContext(ctx).SetURL(rawURL).SetMethod(method)
	// The transport labels any body it is handed
	// application/octet-stream when nothing else set a content type, so
	// an empty contentType would mislabel a JSON body as binary rather
	// than say nothing at all. The flag survives the copy the transport
	// takes of this request.
	req.RawRequest.Header.SetNoDefaultContentType(true)
	if body != nil {
		req.SetRawBody(body)
	}
	if contentType != "" {
		req.SetHeader(fiber.HeaderContentType, contentType)
	}

	resp, err := req.Send()
	if err != nil {
		return 0, nil, err
	}
	defer resp.Close()

	status := resp.StatusCode()
	payload := resp.Body()
	if !isSuccess(status) && len(payload) > maxErrorBody {
		payload = payload[:maxErrorBody]
	}
	return status, bytes.Clone(payload), nil
}

// New builds a client for the configured base URL.
//
// cfg is the fiber.client.* section; opt carries the three settings that
// section cannot hold, and its zero value is usable.
//
// The errors below name no key and no flag, because the same cfg can
// arrive from config.yaml, from the environment, or from a struct literal
// a command filled out of its own flags. A caller that knows which should
// wrap them.
//
// ClientConfig.Validate deliberately lets an EMPTY base URL through: a
// deployment that calls nothing is a correct configuration, and refusing
// it belongs here, at the one point where something is definitely trying
// to build a client.
func New(cfg *config.ClientConfig, opt Options) (*Client, error) {
	if cfg == nil {
		return nil, errors.New("client config was not initialised")
	}

	raw := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if raw == "" {
		return nil, errors.New("base URL is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf(
			"invalid base URL %q: %w", cfg.BaseURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf(
			"base URL must start with http:// or https:// (got %q)",
			cfg.BaseURL)
	}
	// A scheme on its own clears the check above. TrimRight takes both
	// slashes off "https://", leaving "https:", which parses as a
	// scheme with an empty host and an empty path. URL then builds
	// "https:///items", and every request fails in the transport with
	// a message that names neither the setting nor the reason.
	if u.Host == "" {
		return nil, fmt.Errorf(
			"base URL names no host (got %q)", cfg.BaseURL)
	}

	// The suffix comes off the parsed path rather than the raw string,
	// so at worst it shortens the path: trimming the string would let a
	// suffix that also matches the tail of the host eat part of the
	// host. Trailing slashes come off both sides because "/api/v1/" and
	// "/api/v1" name the same prefix, but only the second one matches a
	// path that has already lost its slash, so leaving them on would
	// strip nothing and produce exactly the doubled prefix this field
	// exists to prevent.
	suffix := strings.TrimRight(
		strings.TrimSpace(opt.TrimPathSuffix), "/")
	if suffix != "" {
		u.Path = strings.TrimRight(
			strings.TrimSuffix(u.Path, suffix), "/")
		u.RawPath = ""
	}

	hc := client.New()
	hc.SetHeader(fiber.HeaderAccept, fiber.MIMEApplicationJSON)

	// User-Agent and Referer are written by the client itself after it
	// merges the header map, so those two have to go through their own
	// setters or the merged value is overwritten.
	for k, v := range opt.Headers {
		switch {
		case strings.EqualFold(k, fiber.HeaderUserAgent):
			hc.SetUserAgent(v)
		case strings.EqualFold(k, fiber.HeaderReferer):
			hc.SetReferer(v)
		default:
			hc.SetHeader(k, v)
		}
	}
	if cfg.Token != "" {
		hc.SetHeader(fiber.HeaderAuthorization, "Bearer "+cfg.Token)
	}
	if cfg.Insecure {
		// TLSConfig returns the live config, minimum version and all,
		// so this loosens one field rather than replacing the lot.
		//nolint:gosec // guarded by the caller's own opt-in
		hc.TLSConfig().InsecureSkipVerify = true
	}

	methods := cfg.NoRetryMethods
	if methods == nil {
		methods = []string{fiber.MethodPost}
	}
	noRetry := make(map[string]bool, len(methods))
	for _, m := range methods {
		noRetry[strings.ToUpper(m)] = true
	}

	// Clamped to the documented floor of one attempt. Do's loop runs
	// while attempt <= retries, so a negative value skips the body
	// entirely: nothing is sent, lastErr stays nil, and the caller
	// gets a give-up message wrapping no cause at all. Rejecting it
	// instead would turn a typo into a startup failure for a field
	// whose meaning at zero is already "do not retry".
	retries := cfg.Retries
	if retries < 0 {
		retries = 0
	}

	return &Client{
		base:    u,
		hc:      hc,
		retries: retries,
		noRetry: noRetry,
		log:     opt.Log,
	}, nil
}

// URL builds an absolute URL for a path under the base.
//
// path is concatenated onto the base path rather than joined, so it
// carries its own leading separator. Against a base that has a path,
// "/items" gives "<base>/items" and "items" gives "<base>items", which
// are two different URLs. Against a bare origin, whose path is empty,
// they are the same URL: url.URL.String inserts the separator a host
// requires. So a missing leading separator is invisible until somebody
// configures a base with a prefix, and then every route moves at once.
// Pass it.
//
// It is decoded text rather than an escaped path segment: it is assigned
// to url.URL.Path and escaped once on the way out. So a value
// interpolated into it goes in as itself, a pre-encoded "a%20b" comes
// back out as "a%2520b", and a value containing a separator cannot be
// carried at all, since the escape step leaves separators alone and the
// segment splits in two. The transport normalises the path besides,
// resolving dot segments and collapsing repeated slashes, so an
// interpolated ".." addresses something other than what it spells.
// Query values go through url.Values.Encode and need no such care.
//
// # A query on the base is merged, not replaced
//
// The base's own parameters survive, and query overrides them key by
// key, so a base carrying a static parameter keeps it on every call.
// Assigning RawQuery outright would drop them for any caller that
// passes a query of its own.
func (c *Client) URL(path string, query url.Values) string {
	u := *c.base
	// The base's RawPath describes the base's path, not the one being
	// assigned below. String already ignores a RawPath that does not
	// decode to Path, so clearing it changes no URL; it removes a stale
	// field rather than leaving it to be reasoned about.
	u.RawPath = ""
	u.Path = strings.TrimRight(u.Path, "/") + path
	if len(query) > 0 {
		merged := u.Query()
		for k, v := range query {
			merged[k] = v
		}
		u.RawQuery = merged.Encode()
	}
	return u.String()
}

func (e *Error) Error() string {
	if e.Msg == "" {
		return fmt.Sprintf("%s %s: HTTP %d",
			e.Method, e.Path, e.Status)
	}
	return fmt.Sprintf("%s %s: HTTP %d: %s",
		e.Method, e.Path, e.Status, e.Msg)
}

// Unwrap lets errors.Is reach the sentinel for the statuses a caller
// treats as control flow rather than failure. Every other status
// unwraps to nil, so errors.As is the only way to inspect one.
func (e *Error) Unwrap() error {
	switch e.Status {
	case fiber.StatusNotFound:
		return ErrNotFound
	case fiber.StatusConflict:
		return ErrConflict
	case fiber.StatusUnprocessableEntity:
		return ErrUnprocessable
	}
	return nil
}

// Do performs a request with bounded retries and decodes the response
// into out, which may be nil when the body is not wanted.
//
// rawURL is absolute, and URL is what builds one. A bare path reaches
// the transport as its own URL, with no scheme and no host to resolve
// it against, so it fails before a request is made and the base this
// client was built for is never consulted. Nothing here joins a path
// onto the base, because doing it silently would hide the one case
// where the two spellings differ: see URL on the leading separator.
//
// out receives the whole body, envelope and all; nothing here unwraps
// it. A caller that wants the payload passes *Envelope[T] and reads
// Success and Message off that. Only the status decides success, so a
// 2xx carrying success:false returns nil from here and is the caller's
// to notice.
//
// A 2xx with an empty body and a non-nil out is a decode error, not a
// success. An endpoint that answers 204, or 200 with nothing, has to be
// called with a nil out.
//
// body is a byte slice rather than an io.Reader precisely so a retry
// can replay it. A Reader is consumed by the first attempt, and the
// second would silently send an empty body, which for a POST means the
// server rejects a request that looks, from the log, like it was sent
// correctly.
//
// contentType becomes the request's Content-Type when it is non-empty
// and is left off when it is not; see send for what the transport would
// otherwise put there. Nothing defaults it to JSON, despite the rest of
// this package assuming JSON: a body whose encoding is unstated leaves
// the server free to guess, so a caller sending one names it.
//
// # What is retried, and what is not
//
// Retried: transport failures (connection refused, reset, timeout) and
// the statuses that mean "try again later", 429, 500, 502, 503, and
// 504. The backoff is the one above in every case; a Retry-After header
// is not read, so a server asking for a longer pause than the cap does
// not get one.
//
// Every transport failure counts, including the ones that cannot
// succeed on a second try: an unknown host, a certificate this client
// will not accept. What comes back from the transport does not separate
// those from a reset, and guessing at the text of the message would put
// a string comparison between a caller and its retries. So a
// misconfiguration costs the full backoff before it is reported, which
// is bounded, whereas failing to retry a reset is not.
//
// Not retried: every other 4xx. A 400, 404, 409, 413, or 415 is a
// deterministic answer about this request, and repeating it just delays
// the same result. A 408 is the exception that stays on this side of
// the line anyway: the server gave up reading and may already hold part
// of the body, so replaying it is a decision for a caller that knows
// its endpoints, not for a loop that does not. Nor is any method in
// Config.NoRetryMethods retried, whatever the failure. A 3xx is not
// retried either, and reaches the caller as an *Error like any other
// status that is not a 2xx.
//
// The retry lives here rather than in the transport's own retry hook
// because the decision turns on the status and the method, and that
// hook only ever sees a transport failure.
//
// # What comes back
//
// A status this client will not retry gives an *Error directly. An
// exhausted budget gives an error wrapping the last failure, which is
// an *Error when the last attempt reached the server and a transport
// error when it did not, so errors.As is how a caller inspects either
// and errors.Is is what reaches the sentinels.
func (c *Client) Do(
	ctx context.Context,
	method, rawURL string,
	body []byte,
	contentType string,
	out any,
) error {
	var lastErr error

	// The retry budget, not the attempt count: the loop below runs
	// retries+1 times, which is what the give-up message reports.
	retries := c.retries
	if c.noRetry[strings.ToUpper(method)] {
		retries = 0
	}

	for attempt := 0; attempt <= retries; attempt++ {
		if attempt > 0 {
			shift := attempt - 1
			if shift > maxBackoffShift {
				shift = maxBackoffShift
			}
			wait := firstBackoff << shift
			if wait > maxBackoff {
				wait = maxBackoff
			}
			c.log.Debug().Str("url", rawURL).Int("attempt", attempt).
				Dur("wait", wait).Err(lastErr).Msg("retrying")
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return contextErr(ctx, method, rawURL)
			}
		}

		status, payload, err := c.send(
			ctx, method, rawURL, body, contentType)
		if err != nil {
			if ctx.Err() != nil {
				// The caller's deadline, not a transient failure.
				return contextErr(ctx, method, rawURL)
			}
			lastErr = err
			continue
		}

		if isSuccess(status) {
			if out == nil {
				return nil
			}
			if err := json.Unmarshal(payload, out); err != nil {
				return fmt.Errorf("%s %s: decoding response: %w",
					method, rawURL, err)
			}
			return nil
		}

		apiErr := &Error{
			Status: status,
			Method: method,
			Path:   rawURL,
			Msg:    messageFrom(payload),
		}
		if !retryableStatus(status) {
			return apiErr
		}
		lastErr = apiErr
	}

	return fmt.Errorf("%s %s: giving up after %s: %w",
		method, rawURL, attempts(retries+1), lastErr)
}
