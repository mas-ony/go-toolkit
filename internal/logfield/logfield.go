package logfield

import "github.com/rs/zerolog"

// Handler returns log with name added under the key every line a handler
// writes carries the handler's name under, so the lines of rest's
// handlers and of server's error and panic handlers are told apart by
// one key.
func Handler(log zerolog.Logger, name string) zerolog.Logger {
	return log.With().Str("handler", name).Logger()
}

// Request adds a request's method and path to e, under the keys every
// line about a request carries them under, and returns e.
func Request(e *zerolog.Event, method, path string) *zerolog.Event {
	return e.Str("method", method).Str("path", path)
}

// Panic adds a recovered panic to e, the value it was raised with and
// stack, the trace of the goroutine that recovered it, and returns e.
//
// The caller takes the trace with debug.Stack, so it opens at the frame
// that recovered rather than at this one.
func Panic(e *zerolog.Event, recovered any, stack []byte) *zerolog.Event {
	return e.Interface("panic", recovered).Bytes("stack", stack)
}
