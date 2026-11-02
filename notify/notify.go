package notify

import (
	"context"
	"runtime/debug"
	"time"

	"github.com/mas-ony/go-toolkit/internal/logfield"
	"github.com/rs/zerolog"
)

// Run runs send once, on the caller's goroutine or, when async is set, on
// a goroutine of its own, which Run does not wait for. A notification is
// best-effort, so Run returns nothing: send logs its own failure.
//
// send gets a context that keeps the values of ctx but not its
// cancellation, so the end of the request does not cut off the notice of
// a row it saved, and that expires timeout after Run is called. It is
// released as soon as send returns. timeout must be positive: it is the
// only bound an async send has.
//
// A panic in send is logged at error level, under entity, with its value
// and the stack under the keys logfield gives every recovered panic, and
// goes no further. That is what keeps async safe: recover works only on
// the stack that panicked, so no middleware can catch a panic on the
// goroutine Run starts, and an unrecovered one ends the process. In sync
// mode it keeps a failed notification from turning a saved row into a
// 500.
//
// Under async, send runs after the handler has returned, once Fiber may
// have handed the request's buffers to another request. It must not read
// the fiber.Ctx, nor a string that still points into the request; copy
// what it needs before calling Run. What package request returns is a
// copy already.
func Run(
	ctx context.Context,
	log zerolog.Logger,
	entity string,
	async bool,
	timeout time.Duration,
	send func(ctx context.Context),
) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	run := func() {
		defer cancel()
		defer func() {
			if r := recover(); r != nil {
				logfield.Panic(log.Error(), r, debug.Stack()).
					Str("entity", entity).
					Msg("notification recovered from panic")
			}
		}()
		send(ctx)
	}
	if async {
		go run()
		return
	}
	run()
}
