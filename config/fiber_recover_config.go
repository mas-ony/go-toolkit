package config

// The fiber.recover.* section of config.yaml.
//
// Keys in this section, with their environment-variable spellings:
//
//	fiber.recover.enable_stack_trace
//		FIBER_RECOVER_ENABLE_STACK_TRACE
//
// One key, and that is the whole section — NewRecoverConfig below reads
// exactly that one. The environment spelling holds only for a Viper built by
// NewViper; see the package documentation.

import (
	"errors"
	"fmt"

	"github.com/gofiber/fiber/v3"
	fiberrecover "github.com/gofiber/fiber/v3/middleware/recover"
	"github.com/spf13/viper"
)

// RecoverConfig wraps recover.Config so it participates in the standard
// Validate/String lifecycle used by all other sub-configs.
//
// The middleware writes no response, which the name oversells: it calls
// recover(), hands the value to PanicHandler, and returns the resulting error
// up the chain exactly like a handler's `return err`. The 500 a client sees is
// written by the application's error handler, not here.
//
// It belongs THIRD in the stack — inside zerolog, inside requestid,
// outside limiter — and every one of those three is load-bearing:
//
//   - Inside zerolog, so the error this produces surfaces as the return value
//     of zerolog's c.Next() and the panicking request still gets its log line
//     with a status attached. Registered outside, the panic would unwind past
//     that c.Next() and the request would be missing from the log entirely.
//   - Inside requestid, so an id already exists when the panic is caught. Both
//     the stack-trace handler and the error handler read it back with
//     requestid.FromContext(c), and that id is what ties the opaque 500 a
//     client saw to the two log lines that explain it.
//   - Outside limiter, so a handler panic is already an ERROR by the time it
//     reaches the limiter's own `err = c.Next()`. The other way round it would
//     unwind past that call and none of the limiter's post-Next code would
//     run: the hit is never decremented and no X-RateLimit-* headers are set,
//     so a panicking request counts against the quota even under
//     skip_failed_requests. Nothing on this type needs that; the argument is
//     on LimiterConfig. It is noted here because it is what keeps this
//     middleware from moving inwards.
//
// The cost of that order is that a panic in the logging middleware itself is
// not caught. Far less likely than a panic in a handler, and recover could not
// have logged it anyway.
type RecoverConfig struct {
	// Embedded as a POINTER, so both the wrapper and the embedded struct can
	// be nil independently — hence the two-part nil check in Validate.
	// Embedding also promotes the fields (c.EnableStackTrace, not
	// c.RecoverConfig.Config.EnableStackTrace), which is why a nil embedded
	// pointer panics on field access rather than at the method call.
	*fiberrecover.Config
}

// NewRecoverConfig reads RecoverConfig fields from the provided Viper
// instance.
//
// Never returns an error: absent keys and uncastable values both come back as
// zero values, which Validate rejects afterwards.
//
// This function is the authoritative list of keys the fiber.recover.* section
// supports. A key present in config.yaml but missing here is dead weight —
// Viper never looks it up, so neither the file nor an environment variable can
// supply it.
//
// # The three fields NOT read here
//
// fiberrecover.Config has four exported fields and this constructor populates
// one. The remaining three are absent for the same reason: none can be
// expressed in YAML at any spelling. Next, PanicHandler, and StackTraceHandler
// are funcs. No scalar, sequence, or mapping could carry any of them.
//
// The middleware supplies a working default for each. A nil Next is never
// consulted, so the middleware never skips; a nil PanicHandler is replaced by
// DefaultPanicHandler; a nil StackTraceHandler is replaced by
// defaultStackTraceHandler — but only when EnableStackTrace is true, since the
// middleware's own configDefault gates that one substitution on the flag.
// Changing any of them means editing this file.
func NewRecoverConfig(v *viper.Viper) *RecoverConfig {
	return &RecoverConfig{
		Config: &fiberrecover.Config{
			// EnableStackTrace is the gate on StackTraceHandler, and it is a
			// gate rather than a formatting switch: the middleware calls the
			// handler ONLY when this is true. With it false, the
			// application's stack-trace handler is installed and never runs,
			// so a recovered panic produces no "recovered panic" line at all
			// — no stack, no panic value, no handler=panic field.
			//
			// The request is still logged and the client still gets a 500. The
			// zerolog middleware writes its usual line with status 500 and the
			// panic text in the error field, and the error handler writes its
			// "unhandled error" line with the request id. What is lost is the
			// only record of WHERE the panic happened, which is the one thing
			// the other two lines cannot reconstruct.
			//
			// So the trade is: true costs one debug.Stack() call — a few
			// kilobytes formatted into the log — per recovered panic, on a
			// path that should never be hit. False saves that and turns the
			// next production panic into a 500 nobody can locate. True is the
			// setting worth defaulting to for that reason.
			EnableStackTrace: v.GetBool("fiber.recover.enable_stack_trace"),
		},
	}
}

// WithStackTraceHandler returns the middleware configuration with h installed
// as the stack-trace handler. It is the only supported way to build the value
// handed to recover.New:
//
//	app.Use(fiberrecover.New(
//	  cfg.Recover.WithStackTraceHandler(myStackTraceHandler)))
//
// The handler belongs to the application rather than to this package,
// because it writes through the application's logger and reads the request
// id back out of the Fiber context — both of which are the HTTP layer's
// business, and neither of which exists when this configuration is built.
//
// The handler should take the key it logs the request id under from
// cfg.Zerolog.RequestIDField(), derived once in the application and shared
// with the error handler, so that key follows
// fiber.zerolog.fields_snake_case at all three sites rather than at one of
// them. See ZerologConfig.RequestIDField for why the name is derived rather
// than spelled as a literal at each site.
//
// # Mechanics
//
// The returned value is a shallow copy, so the stored config is never written
// to and a second call cannot see the first one's handler. The struct holds
// one bool and three funcs, so there is nothing shared to write through —
// unlike ZerologConfig.WithLogger's copy, whose Fields still shares a backing
// array.
//
// Passing a nil h is not an error and not checked: recover.configDefault
// substitutes defaultStackTraceHandler when EnableStackTrace is true and the
// handler is nil, so the result is the middleware's stderr fallback rather
// than a crash. That is a quiet degradation, which is why the application's
// call site is the one place this is meant to be built.
//
// Panics if c or c.Config is nil, like every other promoted-field access on
// this type. Validate rejects both at startup, well before the application
// runs.
func (c *RecoverConfig) WithStackTraceHandler(
	h func(fiber.Ctx, any),
) fiberrecover.Config {
	out := *c.Config
	out.StackTraceHandler = h
	return out
}

// Validate returns a joined error for every invalid or missing RecoverConfig
// field.
//
// Deliberately unchecked:
//
//   - Whether the application actually calls WithStackTraceHandler. That is
//     the one way this section fails in practice and the one thing it cannot
//     catch: skipping the call is not a crash, it installs the middleware's
//     own defaultStackTraceHandler and splits the service's output across two
//     formats. Nothing in this package can see that call site.
//   - Next, PanicHandler, and StackTraceHandler. All are nil here by design
//     and stay nil for the life of the process; the middleware's configDefault
//     substitutes DefaultPanicHandler for the nil PanicHandler and never
//     consults a nil Next; StackTraceHandler is supplied to a COPY at the
//     application's call site by WithStackTraceHandler, so it is still nil in
//     the value this method sees and requiring it here would reject every
//     valid config. None can be expressed in YAML.
//   - EnableStackTrace. A bool has no invalid value: both settings are
//     meaningful and an absent key reads as false, which is the documented
//     default. There is nothing a check could reject. What false COSTS is real
//     — a recovered panic with no record of where it happened — but that is an
//     operator's decision, not a malformed value. The argument for true lives
//     on the field in NewRecoverConfig, where it can be read by someone about
//     to change it.
//
// That accounts for every field, so this method has no checks beyond the nil
// guard and returns nil directly. Like WhatsAppConfig's, it does not open
// with the `var errs []error` the other Validate methods start with, because
// there is nothing here to append to one.
func (c *RecoverConfig) Validate() error {
	if c == nil || c.Config == nil {
		return errors.New("fiber.recover config was not initialised")
	}
	return nil
}

// String returns a loggable representation of RecoverConfig.
//
// The pointer receiver means fmt only picks this up for a *RecoverConfig.
// Printing a value copy (%v on RecoverConfig, not &RecoverConfig) bypasses it
// and dumps the struct fields directly.
//
// The nil check is TWO-PART, mirroring Validate's. A logger's own guard, such
// as zerolog's `if val == nil` before it calls a Stringer, compares an
// INTERFACE with nil, which neither a typed nil pointer nor a wrapper around
// a nil embedded pointer satisfies. Without the second half, the half-built
// one would panic inside the startup log line instead of rendering a
// placeholder.
func (c *RecoverConfig) String() string {
	if c == nil {
		return "<nil RecoverConfig>"
	}
	if c.Config == nil {
		return "<uninitialised RecoverConfig>"
	}
	return fmt.Sprintf("EnableStackTrace=%t",
		c.EnableStackTrace,
	)
}
