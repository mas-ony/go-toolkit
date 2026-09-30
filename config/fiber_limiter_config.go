package config

// The fiber.limiter.* section of config.yaml.
//
// Keys in this section, with their environment-variable spellings:
//
//	fiber.limiter.disable_headers
//		FIBER_LIMITER_DISABLE_HEADERS
//	fiber.limiter.disable_value_redaction
//		FIBER_LIMITER_DISABLE_VALUE_REDACTION
//	fiber.limiter.expiration
//		FIBER_LIMITER_EXPIRATION
//	fiber.limiter.max
//		FIBER_LIMITER_MAX
//	fiber.limiter.skip_failed_requests
//		FIBER_LIMITER_SKIP_FAILED_REQUESTS
//	fiber.limiter.skip_successful_requests
//		FIBER_LIMITER_SKIP_SUCCESSFUL_REQUESTS
//	fiber.limiter.strategy
//		FIBER_LIMITER_STRATEGY
//
// Seven keys, and that is the whole section — NewLimiterConfig below reads
// exactly these. The environment spelling holds only for a Viper built by
// NewViper; see the package documentation.

import (
	"errors"
	"fmt"
	"time"

	"github.com/gofiber/fiber/v3/middleware/limiter"
	"github.com/spf13/viper"
)

// LimiterConfig wraps limiter.Config so it participates in the standard
// Validate/String lifecycle used by all other sub-configs.
//
// The middleware belongs FOURTH in the stack — inside zerolog, inside
// requestid, inside recover — and every one of those three is load-bearing:
//
//   - Inside zerolog, so a rejected request still produces a log line. A 429
//     is the one response an operator most wants counted, and registered
//     outside the logger the limiter would answer it without the request ever
//     appearing in the log.
//   - Inside requestid, so the 429 a client saw carries an id that ties it to
//     that line.
//   - Inside recover, so the three-deep sandwich documented on RecoverConfig
//     stays intact. The limiter does not panic itself, but it sits upstream of
//     every handler that might, and that has a consequence worth knowing: a
//     panic unwinds through the limiter's own `err = c.Next()`, so none of its
//     post-Next code runs. The hit is never decremented and no X-RateLimit-*
//     headers are set, which means a panicking request counts against the
//     quota even under skip_failed_requests. Nothing deadlocks — the handler
//     releases its mutex before calling c.Next().
//
// Fourth still puts it OUTSIDE every route: nothing is dispatched to a handler
// until the quota check passes.
//
// # Strategy, and what the two choices actually cost
//
// fiber.limiter.strategy selects LimiterMiddleware. It is the ONE of the
// fields that cannot be spelled directly in YAML which a key can nonetheless
// reach — see limiterMiddlewareFor for why it is the only one.
//
// FIXED is one entry per key, discarded wholesale at the boundary. The
// rotation is two lines: on ts >= e.exp the handler sets e.currHits = 0 and
// e.exp = ts + expiration. The count is thrown away, not decayed, and that is
// the whole of the cost. At a Max of 10000 per minute: 10000 requests at
// 11:00:59.9, the window rolls, 10000 more at 11:01:00.1. Twenty thousand
// requests in two hundred milliseconds and no window ever exceeded Max, so
// nothing was violated. The guarantee is "at most Max per FIXED window", not
// "at most Max in any sixty seconds", and the distance between those two
// readings is the factor of two.
//
// SLIDING charges the previous window's count against the current one on a
// weight that decays across it:
//
//	resetInSec := rotateWindow(e, ts, expiration)  // left in this window
//	weight     := float64(resetInSec) / float64(expiration)
//	rate       := int(math.Ceil(float64(e.prevHits)*weight)) + e.currHits
//
// weight is the fraction of the current window still ahead of the request:
// about 1.0 the instant it rolls, decaying to 0 at its end. Replay the burst
// above and the second half is refused — immediately after the roll prevHits
// is 10000 and weight is ~1.0, so rate is already ~10000 before the client's
// first new request and remaining is 0. The budget returns at 10000 per minute
// of elapsed time instead of all at once. math.Ceil rounds the carried portion
// UP, so the strategy errs strict.
//
// rotateWindow distinguishes two cases that "reset" would flatten: exactly one
// window elapsed sets prevHits = currHits, currHits = 0, exp += expiration —
// the frame slides forward. MORE than one window elapsed zeroes both and
// starts at ts, so a client that went quiet for an hour is not taxed by an
// ancient count on its return.
//
// # Switching is free per key and across a restart, not per request
//
// Both strategies read and write ONE stored type, item{currHits, prevHits,
// exp}, and it already carries the previous window's count; fixed simply never
// assigns prevHits, and manager.release zeroes all three back into the pool.
// So the switch needs no cache flush even with a shared Storage: entries
// written by fixed decode into sliding's reads with prevHits at 0, which just
// means the first window after the restart behaves like fixed and it converges
// from there.
//
// TWO COSTS ARE REAL, and neither is the arithmetic.
//
// The first is entry LIFETIME, not entry size. Both TTLs are measured from the
// LAST WRITE rather than from the window boundary, which is what makes them
// comparable: fixed stores with the window length itself, so a key that goes
// quiet is collected exactly one window after its final request. Sliding
// stores with ttlDuration(resetInSec, expiration) — what remains of this
// window PLUS a whole one — deliberately, so a client cannot wait out the
// sample window and return to a full budget. The same key is therefore
// retained for between one and two windows instead of exactly one. Per key the
// memory is identical; the RESIDENT key population is up to double. Noise
// against the in-memory store; the number that sets the key count on anything
// shared.
//
// The second is store traffic, and it is invisible unless the two handlers are
// read side by side. Their tail blocks are gated differently:
//
//   - fixed re-reads only on a skip:
//     if (SkipSuccessfulRequests && <400) || (SkipFailedRequests && >=400)
//     which is also the ONLY path on which X-RateLimit-Remaining is
//     recomputed; the limit and reset it sends are always the inbound values.
//   - sliding re-reads on `skipHit || !cfg.DisableHeaders`.
//
// DisableHeaders is false here, so sliding adds a second full get-modify-set
// to EVERY request, not just to skipped ones. The reason is sound rather than
// an oversight — the sliding rate is time-dependent, weight has decayed by the
// time the handler returns, and a header computed inbound would be stale — but
// the price is doubled store traffic. Cheap while Storage is nil. With a
// shared Storage it is four network round trips per request instead of two,
// split across the handler's two critical sections and serialised on the one
// mutex either way; see the storage note below.
//
// # Storage is per-process, and prefork multiplies the quota
//
// Storage is left nil, so newManager falls back to Fiber's internal in-memory
// store. Two consequences follow, and neither is visible in config.yaml:
//
//   - Counters are per-process and die with it. A restart, a redeploy, or a
//     crash hands every client a fresh quota, and a second REPLICA has its own
//     counters from the start — so the effective service-wide limit is Max ×
//     replicas, not Max.
//   - fiber.listen.enable_prefork multiplies it again. Each forked worker is
//     a whole process with its own store, so the per-IP quota becomes Max ×
//     workers on one host. database.max_open_conns has the same per-process
//     shape and the same multiplier; the enable_prefork field lists both.
//     Validate cannot check either: the multiplier is the host's core count
//     and this process cannot know it at config time.
//
// A shared backend (Redis, Valkey, or the service's own database) assigned to
// limiter.Config.Storage addresses restarts, replicas and prefork workers
// alike, and it is Go code at the registration site rather than a key, since
// fiber.Storage needs a DSN, a driver, and a Close() that something has to
// own. IT IS NOT A FREE WIN, and
// two properties should be settled before it is reached for:
//
//   - It makes the limiter FAIL CLOSED. On the memory path a store operation
//     has exactly one error return and it is unreachable by construction (see
//     DisableValueRedaction below); on the storage path every get and set can
//     fail and the handler returns that error, so a backend blip becomes a 500
//     through the error handler on every request the middleware sees. Next is
//     nil, so that includes health checks — a dependency outage would not
//     degrade the service, it would take the replica out of rotation. A
//     wrapper that swallows read and write errors is what restores fail-open,
//     and it should exist before the backend does.
//   - It cannot make the count EXACT. fiber.Storage is get/set with no INCR
//     and no compare-and-swap, and the handler's read-modify-write is guarded
//     by a sync.RWMutex created per handler — one lock for all keys, and only
//     within one process. Across replicas increments are lost, so a shared
//     store buys an approximately global limit that overshoots under
//     concurrency. An exact one belongs at the gateway.
//
// # KeyGenerator identifies clients by c.IP(), which fiber.trust_proxy governs
//
// KeyGenerator is left nil, so the middleware keys on c.IP() — and what c.IP()
// returns depends on a key in a different section. With fiber.trust_proxy
// false it is the socket peer address, so every client behind the gateway
// shares one key and the whole deployment is rate-limited as a single client.
// With trust_proxy true AND a matching allowlist in fiber.trust_proxy_config,
// it is the forwarded client address, which is what a per-client quota needs.
//
// The dangerous middle case is trust_proxy true with an EMPTY allowlist: Fiber
// ignores the forwarded headers entirely, so the behaviour collapses back to
// the socket peer while config.yaml reads as though proxies are trusted.
// FiberConfig.Validate rejects that pairing on its own terms, which is why
// LimiterConfig.Validate does not — it cannot see the key, and the check
// already exists one section over.
//
// The key is also passed to the store UNPREFIXED. That is inert while Storage
// is nil and a collision the moment it is not: manager namespaces nothing, so
// a shared backend serving any other Fiber middleware or any second service
// keyed on client IP shares this section's counters. A KeyGenerator returning
// a prefixed key belongs in the same change as the Storage.
type LimiterConfig struct {
	// Embedded as a POINTER, so both the wrapper and the embedded struct can
	// be nil independently — hence the two-part nil check in Validate.
	// Embedding also promotes the fields (c.Max, not
	// c.LimiterConfig.Config.Max), which is why a nil embedded pointer panics
	// on field access rather than at the method call.
	*limiter.Config

	// Strategy is the RAW fiber.limiter.strategy value, retained rather than
	// discarded once LimiterMiddleware has been resolved from it.
	//
	// It lives on the wrapper because limiter.Config has nowhere to put it:
	// that struct holds the resolved Handler, and a Handler cannot say what
	// string produced it, nor whether the key was absent or merely misspelled.
	// Keeping the string is what lets Validate reject "Sliding" by name
	// instead of reporting a nil interface, and what lets String print the
	// configured strategy rather than a %T.
	//
	// EXPORTED on purpose, so that code outside this package — an
	// application's own tests, say — can build a LimiterConfig by hand that
	// passes Validate. Unexported, it could only ever be set through
	// NewLimiterConfig. It is the only field here not promoted from the
	// embedded struct; read it as belonging to the section rather than to
	// the middleware.
	Strategy string
}

// Accepted values for fiber.limiter.strategy.
//
// Matched EXACTLY — not lowercased, not trimmed. "Fixed" and "sliding " are
// rejected by name rather than quietly corrected, which is the same choice
// app.env makes for "Production" and is made for the same reason: a value the
// process silently repairs is a value config.yaml and the running service
// disagree about. Validate prints the offending value with %q, so a stray
// space is visible in the error rather than invisible in the file.
const (
	StrategyFixed   = "fixed"
	StrategySliding = "sliding"
)

// limiterMiddlewareFor maps a fiber.limiter.strategy value to the Handler that
// implements it.
//
// Returns a nil interface for anything else, INCLUDING the empty string, which
// is what an absent key yields. That nil is not a default and must not be
// treated as one — configDefault would silently make it FixedWindow{} — so
// every caller is paired with Validate's strategy check, which is the only
// thing standing between a misspelled key and a limiter counting by a strategy
// nobody chose.
//
// Both returned values are zero-size structs, which is the whole reason this
// field could become a key at all: there is nothing to dial, nothing to
// authenticate, and nothing to close.
func limiterMiddlewareFor(strategy string) limiter.Handler {
	switch strategy {
	case StrategyFixed:
		return limiter.FixedWindow{}
	case StrategySliding:
		return limiter.SlidingWindow{}
	default:
		return nil
	}
}

// strategyName is limiterMiddlewareFor in reverse: the fiber.limiter.strategy
// value that WOULD produce the given Handler, or "" for nil and for any
// Handler this package cannot name.
//
// It exists so Validate can compare what the key says against what is actually
// installed, rather than trusting that the constructor set both from the same
// string.
func strategyName(h limiter.Handler) string {
	switch h.(type) {
	case limiter.FixedWindow:
		return StrategyFixed
	case limiter.SlidingWindow:
		return StrategySliding
	default:
		return ""
	}
}

// NewLimiterConfig reads LimiterConfig fields from the provided Viper
// instance.
//
// Never returns an error: absent keys and uncastable values both come back as
// zero values, which Validate rejects afterwards.
//
// This function is the authoritative list of keys the fiber.limiter.* section
// supports. A key present in config.yaml but missing here is dead weight —
// Viper never looks it up, so neither the file nor an environment variable can
// supply it.
//
// # The six fields NOT read here
//
// limiter.Config has thirteen exported fields and this constructor populates
// seven. The remaining six are absent for the same reason: none can be
// expressed in YAML at any spelling. Storage is an interface; Next, MaxFunc,
// ExpirationFunc, KeyGenerator, and LimitReached are funcs. No scalar,
// sequence, or mapping could carry any of them.
//
// The middleware supplies a working default for each. A nil Storage is
// replaced by Fiber's internal in-memory store; a nil Next is never consulted,
// so the middleware never skips; a nil KeyGenerator is replaced by c.IP(); a
// nil LimitReached is replaced by c.SendStatus(429).
//
// # MaxFunc and ExpirationFunc are the trap, not the absence
//
// Those two default to CLOSURES OVER THIS CONFIG rather than to constants:
// configDefault installs func(fiber.Ctx) int { return cfg.Max } and
// func(fiber.Ctx) time.Duration { return cfg.Expiration }, reading the values
// AFTER its own <= 0 substitutions have run. So leaving both nil is what makes
// fiber.limiter.max and fiber.limiter.expiration mean anything at all.
//
// The trap is what happens when they are not nil. Both handlers read
// cfg.MaxFunc(c) and cfg.ExpirationFunc(c) once per request and never touch
// cfg.Max or cfg.Expiration again — the scalars are consulted exactly once, by
// configDefault, while building the closures, and that includes the storage
// TTL the fixed handler derives from the window. Assign either func at the
// registration site and the matching key is retired on the spot: the file
// still states it, String still prints it, Validate still checks it is
// positive, and nothing on the request path ever reads it. That is the same
// shape as LimiterMiddleware — a key the middleware is free to override with
// something the config no longer describes — except that Validate CAN catch a
// LimiterMiddleware mismatch at construction time (see strategyName) and
// cannot catch this one, because there is nothing on this struct to compare a
// func against.
//
// Changing any of the six means editing this file, and for these two it means
// deleting or rewriting the key they retire in the same change.
func NewLimiterConfig(v *viper.Viper) *LimiterConfig {
	strategy := v.GetString("fiber.limiter.strategy")

	return &LimiterConfig{
		Strategy: strategy,
		Config: &limiter.Config{
			// DisableHeaders omits the rate-limit response headers when true.
			// Two different sets, on two different paths: X-RateLimit-Limit,
			// -Remaining, and -Reset on a request that passes the quota, and
			// Retry-After on the 429 that a request exceeding it receives.
			// Hiding them makes it harder for clients to probe the exact
			// limit, at the cost of well-behaved clients losing the standard
			// back-off signal.
			//
			// It has a SECOND effect under strategy: sliding, which it does
			// not have under fixed. The sliding handler's tail block runs on
			// `skipHit || !DisableHeaders`, so leaving headers on there is
			// what makes every request pay a second get-modify-set. See the
			// type comment; do not read a change to this key as header-only
			// once the strategy is sliding.
			DisableHeaders: v.GetBool("fiber.limiter.disable_headers"),

			// DisableValueRedaction turns off the masking of limiter keys when
			// true. The limiter key is the per-client identity — by default
			// the client IP — so with redaction active (false, the default) it
			// is replaced by the literal "[redacted]".
			//
			// Where it is replaced is narrower than the name suggests, and
			// narrow enough that, with Storage nil as this package leaves
			// it, the key does NOTHING. The flag
			// reaches exactly one place: manager.logKey, called only while
			// formatting the error strings manager.get and manager.set return
			// when an operation fails. Four of its five call sites sit behind
			// a non-nil Storage, and Storage is nil here.
			//
			// The fifth is on the in-memory path — manager.get formats the key
			// when the value it read back is not an *item — and it cannot fire
			// either, because that store is created by newManager, is private
			// to this middleware, and is only ever written by manager.set with
			// an *item. So no output contains a limiter key under either
			// setting.
			//
			// It becomes live the moment a shared Storage is assigned, and
			// those strings are RETURNED as errors, so they reach the error
			// handler and are logged in full. Set it true only then, only
			// when cleartext keys are needed to debug that backend, and only
			// when the log pipeline is allowed to carry client IPs.
			//
			// Note: this has nothing to do with the X-RateLimit-* headers —
			// header visibility is controlled solely by DisableHeaders above.
			DisableValueRedaction: v.GetBool(
				"fiber.limiter.disable_value_redaction"),

			// Expiration is the window size. Each client IP is allowed at most
			// Max requests within one window; under strategy: fixed the
			// counter resets when the window expires, under strategy: sliding
			// the previous window's count is weighted into the current one.
			// Must be at least one second either way.
			//
			// ONE SECOND IS A FLOOR, not a formality, and the reason is a
			// truncation. configDefault tests int(Expiration.Seconds()) <= 0,
			// so every value below 1s truncates to zero seconds and is
			// replaced wholesale by the middleware's own 1 minute. That
			// catches the obvious typo — a bare `expiration: 60` parses as 60
			// NANOSECONDS, not 60 seconds — but it catches it silently, by
			// enforcing a window that appears in neither config.yaml nor this
			// struct's String output. Validate rejects the sub-second range
			// instead. Always write a unit.
			//
			// The middleware also has windowSeconds, which FLOORS a positive
			// sub-second duration to 1s rather than replacing it. That is not
			// a contradiction and does not soften the above: it applies to
			// ExpirationFunc's per-request return value, and configDefault has
			// already normalised the scalar before the closure it installs can
			// return it. From this key, the 1m substitution is the reachable
			// behaviour and the 1s floor is not.
			//
			// Validate refuses a bare number here and on
			// fiber.listen.shutdown_timeout for opposite reasons. There,
			// Fiber would run the nanoseconds exactly as written; here, the
			// middleware would discard them for its own minute. Either way,
			// what runs is not the number the file appears to state.
			//
			// Whole seconds are all that survive either way: the handler
			// computes uint64(d.Seconds()) for the window, so 1500ms is a one
			// second window and 90500ms a ninety second one.
			Expiration: v.GetDuration("fiber.limiter.expiration"),

			// Max is the maximum number of requests a single client IP may
			// make within one Expiration window before receiving 429 Too Many
			// Requests. Must be positive.
			//
			// Zero is NOT "unlimited" — configDefault tests Max <= 0 and
			// substitutes its own default of 5, so an absent key would turn a
			// forgotten line into a service that 429s the sixth request of
			// every minute. A NEGATIVE value lands in the same branch and
			// becomes 5 as well; it is not a disable switch and not a lower
			// bound. Validate requires a positive number instead, so what is
			// configured and what is enforced are the same. That is the same
			// choice fiber.requestid.header and fiber.zerolog.fields make, for
			// the same reason.
			//
			// The handler does treat a max of 0 as "skip this request
			// entirely", but that branch is unreachable from here:
			// configDefault has already replaced the 0 before any request
			// arrives. Disabling the limiter means not registering it.
			Max: v.GetInt("fiber.limiter.max"),

			// SkipFailedRequests excludes requests with HTTP status >= 400
			// from the client's quota. Useful to avoid penalising clients for
			// backend errors they did not cause (e.g. 5xx responses).
			//
			// "Excludes" is after the fact: the hit is counted on the way in
			// and decremented once the status is known, which is why the quota
			// check happens against a count that briefly includes requests
			// that will not end up counting.
			SkipFailedRequests: v.GetBool(
				"fiber.limiter.skip_failed_requests"),

			// SkipSuccessfulRequests excludes requests with HTTP status < 400
			// from the quota. Combine with SkipFailedRequests: false to count
			// only failed requests — effectively rate-limiting
			// error-generating behaviour such as brute-force login attempts.
			//
			// Setting BOTH to true means every request is decremented back out
			// of the quota as soon as it completes, which leaves the
			// middleware registered, allocating a store entry per client, and
			// enforcing nothing. Validate rejects that pairing.
			SkipSuccessfulRequests: v.GetBool(
				"fiber.limiter.skip_successful_requests"),

			// LimiterMiddleware is the counting strategy, resolved from the
			// raw fiber.limiter.strategy string above.
			//
			// An unrecognised string resolves to nil HERE and is caught in
			// Validate — it must be, because a nil reaching the middleware is
			// not an error there: configDefault substitutes FixedWindow{} for
			// it, which would turn a typo into a working limiter counting by a
			// strategy the file does not name. That is the same trap Max and
			// Expiration are guarded against, and the reason Strategy is kept
			// on the wrapper rather than thrown away once resolved.
			LimiterMiddleware: limiterMiddlewareFor(strategy),
		},
	}
}

// Validate returns a joined error for every invalid or missing LimiterConfig
// field.
//
// Deliberately unchecked:
//
//   - Whether Max is SENSIBLE. 1 per hour and 10000000 per second both pass.
//     The right number depends on the client population and the gateway in
//     front, neither of which is visible here.
//   - Whether Expiration is a WHOLE number of seconds. The handler truncates
//     with uint64(d.Seconds()), so 1500ms silently runs as a one second
//     window. Rejecting that would fail configurations that are merely
//     imprecise, and the floor check below already catches the case where
//     truncation reaches zero and changes the window outright.
//   - Whether the chosen STRATEGY is the right one. Fixed admits a 2×Max burst
//     across a boundary and sliding does not; sliding costs a second
//     get-modify-set per request while headers are on. Which trade is correct
//     depends on the gateway in front and on whether Storage is still nil,
//     neither of which is visible here. Both are checked for being NAMED, not
//     for being wise.
//   - DisableHeaders and DisableValueRedaction. A bool has no invalid value:
//     both settings are meaningful and an absent key reads as false, which is
//     the documented default. There is nothing a check could reject. Note that
//     DisableValueRedaction is inert either way while Storage is nil, and that
//     DisableHeaders acquires a second, non-cosmetic effect under
//     strategy: sliding; see the field comments.
//   - The KeyGenerator / fiber.trust_proxy pairing. See the note on the type:
//     this section cannot see that key, and FiberConfig.Validate already
//     rejects the misconfiguration on its own terms.
//   - Storage against fiber.listen.enable_prefork. The interaction is real and
//     documented on the type, but the multiplier is the host's core count,
//     which this process cannot know at config time.
//   - WHERE the application registers the middleware. That it registers it at
//     all is settled — the checks below guard a limiter that is actually
//     running — but the POSITION is not visible from here, and position is
//     what decides whether a 429 reaches the request log and carries an id.
//     Registered outside zerolog it answers rejections that never appear in
//     the log; outside recover it loses the decrement on a panicking request.
//     Both are absences rather than errors, so nothing reports them. The
//     invariant is argued at the registration site and on the type above; this
//     method cannot see either.
//
// Every check appends rather than returning early, so one restart surfaces
// every fiber.limiter.* problem at once.
func (c *LimiterConfig) Validate() error {
	// errs is declared before the nil check only so the two statements read in
	// the same order in all Validate implementations; the nil check is what
	// must come first, since every line after it dereferences c.
	var errs []error
	if c == nil || c.Config == nil {
		return errors.New("fiber.limiter config was not initialised")
	}
	if c.Expiration == 0 {
		errs = append(errs, errors.New("fiber.limiter.expiration is "+
			"required: it names the window every quota is counted against, "+
			"and leaving it empty enforces the middleware's own 1m anyway — "+
			"a number neither config.yaml nor a startup log line will show"))
	} else if c.Expiration < time.Second {
		// Below a second the middleware does not round down, it throws the
		// value away: configDefault tests int(Expiration.Seconds()) <= 0 and
		// substitutes 1 minute. This is also where a bare number lands, since
		// an unsuffixed 60 is 60ns.
		errs = append(errs, fmt.Errorf("fiber.limiter.expiration must be at "+
			"least 1s (got %s): the middleware truncates the window to whole "+
			"seconds and replaces anything that truncates to zero with its "+
			"own 1m — if this was meant as seconds, write the unit, because "+
			"a bare number is nanoseconds",
			c.Expiration))
	}
	if c.Max <= 0 {
		// One branch for both signs on purpose, because the middleware has one
		// branch: configDefault tests Max <= 0. Zero is not "unlimited" and a
		// negative is not a disable switch — each is silently 5.
		errs = append(errs, fmt.Errorf("fiber.limiter.max must be a positive "+
			"number of requests (got %d): the middleware substitutes its own "+
			"default of 5 for anything at or below zero, so neither an "+
			"omitted key nor a negative one disables the limiter — both "+
			"rate-limit every client to five requests per window",
			c.Max))
	}

	// The strategy is checked in two steps because there are two ways to get
	// it wrong and they need different messages: the KEY can be absent or
	// misspelled, and the key can be right while the INSTALLED handler is not
	// the one it names.
	//
	// The second case is not hypothetical paranoia — it is the same shape as
	// the MaxFunc trap on the constructor. What it CANNOT catch is an
	// override applied after this method runs: the application copies the
	// struct long after config load, so a LimiterMiddleware assigned there is
	// invisible here, in exactly the way a MaxFunc assigned there is. This
	// check binds the key to the handler at CONSTRUCTION time — it catches a
	// hand-built config, and an edit that updates one of the two and not the
	// other, and it says nothing about what the registration site does
	// afterwards.
	switch c.Strategy {
	case "":
		errs = append(errs, fmt.Errorf("fiber.limiter.strategy is required: "+
			"it selects how hits are counted, and leaving it empty installs "+
			"the middleware's own FixedWindow anyway — a rule neither "+
			"config.yaml nor a startup log line will show. Write %q to keep "+
			"counting per window and discarding the count at the boundary "+
			"(which admits a burst of 2×max across it), or %q to weight the "+
			"previous window's count into the current one",
			StrategyFixed,
			StrategySliding))
	case StrategyFixed, StrategySliding:
		installed := strategyName(c.LimiterMiddleware)
		if installed != c.Strategy {
			errs = append(errs, fmt.Errorf("fiber.limiter.strategy is %q but "+
				"limiter.Config.LimiterMiddleware holds %T: the key and the "+
				"installed handler must agree, or config.yaml describes a "+
				"limiter that is not the one running — and a nil here is not "+
				"an absence, since configDefault replaces it with FixedWindow",
				c.Strategy,
				c.LimiterMiddleware))
		}
	default:
		errs = append(errs, fmt.Errorf("fiber.limiter.strategy must be "+
			"exactly %q or %q (got %q): the value is matched as written, not "+
			"lowercased or trimmed, and anything unrecognised leaves "+
			"LimiterMiddleware nil — which the middleware replaces with "+
			"FixedWindow rather than rejecting",
			StrategyFixed,
			StrategySliding,
			c.Strategy))
	}

	// Both skips true is not a stricter limiter, it is a different one. Every
	// response is either < 400 or >= 400, so whichever bucket a request lands
	// in its hit is decremented straight back out once the status is known.
	// Sequential traffic never accumulates a count at all, and Max becomes
	// unreachable: the rate limit is gone.
	//
	// What is left is not nothing, which is why this is rejected rather than
	// warned about. The decrement happens AFTER c.Next(), so concurrent
	// requests on one key each hold their hit for the length of their handler,
	// and the (Max+1)-th request IN FLIGHT still sees a negative remainder and
	// gets a 429. The middleware quietly stops being a rate limiter and
	// becomes a per-client concurrency cap — reading as ON everywhere except
	// in the one place it acts, the same shape as fiber.trust_proxy without an
	// allowlist.
	//
	// This holds under both strategies. Sliding credits the hit back to
	// whichever bucket the original request landed in rather than to the
	// current one, which is a more careful decrement of the same hit, not a
	// smaller one.
	//
	// Both are guarded on the hit still being findable: fixed credits back
	// only while e.exp is the window the request was counted in, and sliding's
	// bucketForOriginalHit returns nil once the hit is older than a full
	// window. A handler that outlives its own window therefore keeps its hit,
	// under either strategy. That does not soften the check — it is a
	// straggler, not a mechanism — but it is why the residue is a concurrency
	// cap and not simply nothing.
	if c.SkipFailedRequests && c.SkipSuccessfulRequests {
		errs = append(errs, errors.New("fiber.limiter.skip_failed_requests "+
			"and fiber.limiter.skip_successful_requests are both true: every "+
			"response is either < 400 or >= 400, so every hit is decremented "+
			"back out as soon as the status is known — sequential traffic "+
			"never reaches fiber.limiter.max, and what survives is not a "+
			"rate limit but a cap on concurrent in-flight requests per "+
			"client — leave at least one of them false"))
	}

	return errors.Join(errs...)
}

// String returns a loggable representation of LimiterConfig.
//
// The pointer receiver means fmt only picks this up for a *LimiterConfig.
// Printing a value copy (%v on LimiterConfig, not &LimiterConfig) bypasses it
// and dumps the struct fields directly.
//
// The nil check is TWO-PART, mirroring Validate's. A logger's own guard, such
// as zerolog's `if val == nil` before it calls a Stringer, compares an
// INTERFACE with nil, which neither a typed nil pointer nor a wrapper around
// a nil embedded pointer satisfies. Without the second half, the half-built
// one would panic inside the startup log line instead of rendering a
// placeholder.
//
// Strategy is printed with %q, so a stray space in a rejected value shows in
// the log exactly as it does in the error.
func (c *LimiterConfig) String() string {
	if c == nil {
		return "<nil LimiterConfig>"
	}
	if c.Config == nil {
		return "<uninitialised LimiterConfig>"
	}
	return fmt.Sprintf("Strategy=%q "+
		"Expiration=%s "+
		"Max=%d "+
		"DisableHeaders=%t "+
		"DisableValueRedaction=%t "+
		"SkipFailedRequests=%t "+
		"SkipSuccessfulRequests=%t",
		c.Strategy,
		c.Expiration,
		c.Max,
		c.DisableHeaders,
		c.DisableValueRedaction,
		c.SkipFailedRequests,
		c.SkipSuccessfulRequests,
	)
}
