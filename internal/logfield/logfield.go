package logfield

import "github.com/rs/zerolog"

// To, File and MessageID are the keys a line about a delivered
// notification carries its recipient, the file it carried and the id the
// channel gave it under, which email, telegram and whatsapp write on
// every message and document they report sent. They are keys rather than
// helpers because each channel writes them with a type of its own: one
// chat or a list of addresses, an id that is text or a number.
const (
	To        = "to"
	File      = "file"
	MessageID = "message_id"
)

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

// Entity adds entity, the name a service gives the rows of one table, to
// e under the key every line about those rows carries it under, and
// returns e. crud writes it on the line a service logs for each row it
// writes, and notify on the panic of a notification about one, so a
// search for one table's rows finds both.
func Entity(e *zerolog.Event, entity string) *zerolog.Event {
	return e.Str("entity", entity)
}
