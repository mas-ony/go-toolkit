// Package telegram sends notifications as a Telegram bot: a text message,
// or a document with a caption, to one chat at a time.
//
// It is the Telegram channel beside whatsapp and email. A deployment
// switches it on by listing telegram in notification.channels and
// configures it in notification.telegram.*, which the config package reads
// and validates. New takes that section and nothing else, and does not
// dial, so a network that cannot reach Telegram delays no startup.
//
//	bot, err := telegram.New(config.NewTelegramConfig(v), log)
//	if err != nil {
//		return err
//	}
//	err = bot.SendText(ctx, "123456789", "Laporan sudah siap")
//	err = bot.SendDocument(ctx, "-1001234567890", pdf,
//		"realisasi.pdf", "Laporan realisasi Triwulan III")
//
// # Who can be written to
//
// A chat is named by its numeric id, which is negative for a group,
// supergroup or channel, or by @username for a public channel or
// supergroup. A person has no username a bot can write to. The bot reaches
// a person only after they have started it, and the id to use is the one
// their own first message carried, so an application that notifies people
// has to store that id when they write. The id of a group changes when
// Telegram upgrades it to a supergroup, and the old one then answers 400.
//
// A chat that is empty once trimmed is a silent no-op, so a record with the
// column unfilled needs no guard, as with whatsapp. One that is neither a
// number nor @name is ErrInvalidChatID, because the cause is a bad stored
// value rather than a failure to deliver.
//
// # What is sent
//
// Text goes as plain text. No parse mode is set, so nothing in it is read as
// Markdown or HTML and there is nothing to escape: an underscore in a name
// stays an underscore. The Bot API takes up to MaxTextRunes characters of
// text and MaxCaptionRunes of caption, and a longer one is ErrTooLong,
// returned before any request is made. Text that is blank is nothing to
// send, like an empty message on the other channels.
//
// A document is the file's bytes and the name the recipient sees. A blank
// name is sent as "document". The hosted API takes files up to 50 MB. That
// is not checked here, because a Bot API server of your own takes more, so
// the refusal for a file that is too large comes back as an *APIError.
//
// # What a nil error means
//
// The API accepted the message. It says nothing about whether the person
// has read it, and one who blocked the bot shows up as a 403 on the next
// send, not on the one before.
//
// A refusal is an *APIError carrying Telegram's own code and description.
// A 429 also carries how long to wait, in RetryAfter. The other 4xx
// refusals describe the request or the chat, and sending the same thing
// again fails the same way; a 429 and a 5xx are worth retrying after a
// pause. Nothing here retries, because a retry belongs to the caller, who
// knows whether the notification is still worth sending.
//
// # Bounds
//
// notification.telegram.timeout and the context each bound a send, and
// whichever ends first ends it. The error then unwraps to
// context.DeadlineExceeded or context.Canceled.
//
// Under notification.telegram.async a send has no request to end it, so the
// timeout is its only bound. Send on context.WithoutCancel(ctx), and build
// the arguments from copies before the goroutine starts, since Fiber reuses
// request memory once the handler returns.
//
// # The token
//
// The Bot API takes the token in the URL of every request. net/http puts
// that URL in the errors it returns, so the errors from this package are
// cut back to their cause, and the token is masked out of any text that
// still carries it. No log line names it either.
//
// A redirect is never followed, since following one would hand the token to
// wherever it points. For the same reason the config package refuses an
// http:// API URL unless it is on loopback.
//
// A returned error does name the chat, and the success line logged names
// it too, with the id of the message Telegram assigned. That puts a chat id
// wherever a caller logs it, which is deliberate: without it the line cannot
// say which notification failed.
//
// # Switched off
//
// A nil *Service is safe. Every send on it returns ErrNotConfigured, as
// email's does, so a caller can keep a nil *Service when telegram is not
// among notification.channels and call it without a guard, including from
// a detached goroutine, where a panic would have nobody to catch it.
package telegram
