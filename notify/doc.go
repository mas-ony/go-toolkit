// Package notify runs best-effort notifications: sends whose failure must
// not fail the request that asked for them, such as the WhatsApp, email
// or Telegram notice of a row the request has just saved.
//
// Each channel section of package config carries an Async setting, and
// its documentation says what a send needs under it: a context detached
// from the request, with a deadline of its own. Run is that, written
// once, with the recover a goroutine of its own needs as well:
//
//	notify.Run(ctx, log, "surat_masuk_disp", cfg.WhatsApp.Async,
//		time.Minute, func(ctx context.Context) {
//			if err := wa.SendTextContext(ctx, phone, msg); err != nil {
//				log.Error().Err(err).Msg("whatsapp notification failed")
//			}
//		})
//
// # What the tests hold in place
//
// The unit suite runs send in both modes and holds Run to its promises:
// the context keeps the request's values but not its cancellation, it
// carries the deadline, it is released once send returns, and a panic
// in send is logged and goes no further, on the caller's goroutine and
// on a goroutine of its own alike.
package notify
