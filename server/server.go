package server

import (
	"errors"
	"runtime/debug"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/requestid"
	"github.com/mas-ony/go-toolkit/response"
	"github.com/rs/zerolog"
)

// requestFields adds what identifies the request to e: its id under field,
// the key the access log carries it under, and its method and path. Every
// line logged here is written through it, so each one can be joined to the
// request's access log line by the same three fields.
func requestFields(
	e *zerolog.Event,
	c fiber.Ctx,
	field string,
) *zerolog.Event {
	return e.Str(field, requestid.FromContext(c)).
		Str("method", c.Method()).
		Str("path", c.Path())
}

// ErrorHandler returns the handler that renders every error escaping a
// route in response's envelope, as c.Status(code).JSON(response.Fail(
// message)), so a failure reads like every success.
//
// A *fiber.Error carries its own status and a message that is safe to
// show, and is rendered as it is: a handler returns one for every failure
// it answers, a 500 included, having logged the cause first, and Fiber
// returns one for an unmatched route (404), a wrong method (405) and a
// body over fiber.body_limit (413). Anything else is a bug whose text may
// hold a query, a DSN fragment or a file system path, so it is logged in
// full at error level and the client is told only "internal server
// error". A panic the recover middleware turned into an error lands
// there too.
//
// One *fiber.Error is logged here as well: a body over fiber.body_limit.
// fasthttp rejects it before routing, so the access log line for that
// request reads 200 with no error, and this is the only line that shows
// the 413 the client received. Every other *fiber.Error is already on the
// access log line.
func ErrorHandler(
	log zerolog.Logger,
	requestIDField string,
) fiber.ErrorHandler {
	l := log.With().Str("handler", "error").Logger()

	return func(c fiber.Ctx, err error) error {
		if errors.Is(err, fiber.ErrRequestEntityTooLarge) {
			requestFields(l.Warn(), c, requestIDField).
				Int("status", fiber.StatusRequestEntityTooLarge).
				Msg("request body over fiber.body_limit")
		}

		if fe, ok := errors.AsType[*fiber.Error](err); ok {
			return c.Status(fe.Code).JSON(response.Fail(fe.Message))
		}

		requestFields(l.Error().Err(err), c, requestIDField).
			Msg("unhandled error")
		return c.Status(fiber.StatusInternalServerError).
			JSON(response.Fail("internal server error"))
	}
}

// StackTraceHandler returns the recover middleware's StackTraceHandler: a
// recovered panic is logged at error level, with its value and the stack,
// through the application's logger rather than the middleware's default,
// which writes to stderr in a format of its own.
//
// The middleware calls it only under fiber.recover.enable_stack_trace.
// With the key off a recovered panic logs nothing here; the client still
// gets the 500 ErrorHandler writes.
func StackTraceHandler(
	log zerolog.Logger,
	requestIDField string,
) func(fiber.Ctx, any) {
	l := log.With().Str("handler", "panic").Logger()

	return func(c fiber.Ctx, e any) {
		requestFields(l.Error().Interface("panic", e), c, requestIDField).
			Bytes("stack", debug.Stack()).
			Msg("recovered panic")
	}
}

// LimitReached is a limiter LimitReached that returns
// fiber.ErrTooManyRequests, so a client over its quota gets the 429 from
// ErrorHandler, in the envelope, where the limiter's default answers in
// plain text. The Retry-After header the limiter sets before calling it
// is kept.
func LimitReached(fiber.Ctx) error {
	return fiber.ErrTooManyRequests
}
