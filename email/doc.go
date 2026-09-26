// Package email sends notification mail through an SMTP relay: one
// message per call, over a connection opened for it and closed after.
//
//	// nil unless email is chosen, and then every send returns
//	// ErrNotConfigured instead of panicking.
//	var mailer *email.Service
//	if notif.Enabled(config.ChannelEmail) {
//		mailer, err = email.New(config.NewEmailConfig(v), log)
//		if err != nil {
//			return err // chosen, so a missing section is fatal too
//		}
//	}
//
//	err = mailer.Send(ctx, email.Message{
//		To:      []string{"Budi Santoso <budi@example.go.id>"},
//		Subject: "Laporan realisasi Triwulan III",
//		Text:    "Laporan terlampir.",
//		Attachments: []email.Attachment{{
//			Filename: "realisasi.pdf",
//			Data:     pdf,
//		}},
//	})
//
// Two files, two jobs. email.go holds the conversation with the relay.
// message.go builds the bytes that conversation uploads and never touches
// the network, which is what lets its tests read every message back
// without a server.
//
// # Delivered means the relay accepted it
//
// Send returns nil when the relay answers the end of DATA with success.
// That is the relay undertaking to deliver or bounce, and the last thing
// SMTP says while the connection is open: a mailbox that does not exist
// is a bounce to notification.email.from, possibly hours later, and never
// an error here.
//
// The reply to QUIT is not checked. By then the message is the relay's,
// and reporting a failed goodbye as a failed send would lead a caller
// that retries to deliver it twice.
//
// One message is all or nothing. Recipients are given to the relay one
// RCPT at a time, and the first refusal ends the send before DATA, so no
// recipient gets a message another recipient was refused. The refusal
// unwraps to a *textproto.Error, whose Code says what to do with it: 4xx
// is temporary and worth a later retry, 5xx is permanent and is not.
//
// # TLS has three modes and no fallback
//
// notification.email.tls picks one, and nothing here changes it at run
// time. starttls requires the server to offer STARTTLS and fails the send
// when it does not, before a credential or an address is sent; implicit
// speaks TLS from the first byte; none never negotiates it. The
// certificate is checked against notification.email.host unless
// notification.email.insecure is set.
//
// Credentials travel only over TLS or to a relay on loopback, which is
// the rule net/smtp's PlainAuth applies and the one EmailConfig.Validate
// applies before anything is sent. Login uses PLAIN, or LOGIN when the
// server offers LOGIN and not PLAIN; a server offering neither fails the
// send at AUTH, and the error names what it does offer.
//
// # Every send is bounded
//
// notification.email.timeout bounds one whole send, from the dial to the
// relay's verdict on the message, and ctx bounds it too. Whichever ends
// first ends the send, and the error then unwraps to
// context.DeadlineExceeded or context.Canceled. net/smtp takes no
// context, so the bound is enforced through the connection's deadline;
// the comment on deliver in email.go says how.
//
// Under notification.email.async a send outlives the request that asked for
// it, and that changes two things:
//
//	msg := email.Message{...} // built here, from copies
//	go func() {
//		ctx := context.WithoutCancel(reqCtx)
//		if err := mailer.Send(ctx, msg); err != nil {
//			log.Error().Err(err).Msg("notification email")
//		}
//	}()
//
// The context has to lose its cancellation, or the send dies the moment
// the handler returns. WithoutCancel keeps its values, so a request ID
// still reaches the log, and it leaves the timeout as the only bound,
// which is why Validate requires one.
//
// And the Message has to be built before the goroutine starts. Fiber
// reuses a request's memory once the handler returns, so a string or byte
// slice taken from the request and read afterwards may already hold the
// next request's data. Build the Message from copies, then hand it over.
//
// There is no pool and no queue. A notification is rare enough that a
// connection per message costs nothing worth saving, and a pool would
// hold connections the relay closes between messages, turning the first
// send after a quiet hour into a failure.
//
// # Addresses are parsed narrowly
//
// Each To, Cc and Bcc entry is ONE address. An entry holding a list —
// "a@example.go.id, b@example.go.id", as a spreadsheet cell or a text
// field tends to — is refused with ErrInvalidAddress rather than split,
// because a comma is also legal inside a quoted display name and
// splitting on it would be a guess.
//
// For the same reason, a display name containing a comma has to be
// quoted: "Budi Santoso, S.Kom" <budi@example.go.id> parses, and the
// same name without the quotes does not.
// (&mail.Address{Name: name, Address: addr}).String() quotes whatever
// needs it.
//
// Empty entries are skipped, and an address given twice, in one field or
// across two, is sent once. Bcc addresses reach the relay and appear in
// no header.
//
// # What a message looks like
//
// A non-ASCII subject or display name is encoded (RFC 2047), and so is a
// line break in one, so text from user input cannot add a header. The
// headers are then ASCII, unless an address itself is not, which only a
// relay offering SMTPUTF8 accepts.
//
// Each message carries a Message-ID under the sender's domain, and Send
// logs it on success, so a line in this service's log matches one in the
// relay's. Each also carries Auto-Submitted: auto-generated, which tells
// vacation responders and ticket systems not to answer.
//
// Bodies are UTF-8. Text is quoted-printable and attachments are base64,
// so the message is 7-bit with CRLF line endings throughout, the form
// SMTP was designed to carry. The whole message is built in memory, at
// about four thirds the size of its attachments.
//
// # What the tests hold in place
//
// message_test.go reads composed messages back with net/mail,
// mime/multipart and the MIME word decoder, so each claim in the section
// above is checked against the parsers a recipient's software stands in
// for, rather than against the bytes this package meant to write.
//
// email_test.go runs every send against an in-process SMTP relay on
// loopback that records what it was told: delivery with dot-stuffing,
// PLAIN and LOGIN, strict STARTTLS, a refused login, a refused recipient,
// the size check, an unanswered QUIT, the no-op paths, the Validate rule
// on clear-text credentials, and a nil Service.
//
// email_integration_test.go covers what needs a certificate, a clock or
// contention: STARTTLS before any credential, implicit TLS, certificate
// verification and insecure, the timeout and cancellation against a relay
// that stops answering, and concurrent sends under the race detector.
//
//	go test -tags integration -run Integration ./email
//
// It binds loopback listeners, generates its certificate in memory, and
// needs nothing else.
package email
