// Package whatsapp sends outbound WhatsApp messages through whatsmeow.
//
// It is a narrow wrapper and deliberately one-directional: New connects and
// pairs, SendText and SendDocument deliver, Disconnect closes the socket.
// Nothing here receives, and no event handler is registered. A service that
// needs incoming messages wants whatsmeow directly, not this.
//
//	svc, err := whatsapp.New(log)
//	if err != nil {
//		log.Warn().Err(err).Msg("notifications disabled")
//		return nil // an unpaired device is a state, not a crash
//	}
//	defer svc.Disconnect()
//
// # Pairing is a blocking, once-per-device event
//
// On first run the credential store is empty, whatsmeow asks WhatsApp for a
// QR code, and New renders it to stdout and blocks. It keeps blocking until
// the code is scanned or the codes stop refreshing, which is minutes rather
// than seconds. A process started with an empty store therefore stalls for
// that whole window before doing anything else.
//
// Pair the device before first use, or start with an already-paired store in
// place. NewContext is the way to bound the wait; New cannot, because the
// context it passes is context.Background.
//
// An unscanned window is ErrNotPaired, not success. The client is connected
// but not logged in at that point, and returning it would hand back a
// non-nil *Service that passes every nil check and then fails at every send.
//
// Once paired, the credentials live in the store and survive restarts. A new
// QR code is needed only after an explicit logout or after the linked
// session is revoked from the phone.
//
// # The credential store is a file, and its path is a parameter
//
// whatsmeow's sqlstore backed by SQLite. The store is small and written to
// rarely, so a file avoids standing up a second server for a handful of
// rows, and the SQLite driver is pure Go — this package builds and runs
// under CGO_ENABLED=0 with no C toolchain present.
//
// The path defaults to DefaultStorePath, which is RELATIVE and therefore
// resolves against the process working directory. That is fine for one
// service on one host and wrong as soon as there are two: both would open
// the same file and fight over one device registration. Pass an explicit
// path to NewContext for anything that is not a single deployment, and put
// it on storage that survives a restart — losing the file means pairing
// again.
//
// # The QR code is a live credential
//
// While a code is on screen it is also emitted as an INFO log line under the
// key "qr", as a fallback for a terminal that cannot render the block. That
// line is the whole pairing payload: anyone who can read the log while the
// code is current can render it and link a device of their own. Each code
// expires with its cycle, which bounds the window rather than closing it.
//
// Pair somewhere the log is not shared, or drop the field and keep the
// terminal rendering alone.
//
// # Phone numbers are normalised, then refused rather than repaired
//
// A recipient may arrive as "+62 812-3456789". Leading "+", spaces and
// hyphens are stripped; everything else survives, and a number still
// holding anything but digits afterwards is ErrInvalidPhone rather than a
// send.
//
// That narrowness is the decision. A wider filter would silently repair
// typed junk into a number nobody verified, trading a rejected send for one
// delivered to the wrong person. The dot is the case that makes it concrete:
// whatsmeow reads the user part's dot as an AGENT separator, so
// "628.1@s.whatsapp.net" is not malformed — it is a well-formed address
// naming somebody's device.
//
// Empty and malformed are deliberately different answers. A recipient that
// is empty once normalised is a silent no-op, because an unfilled phone
// column is an expected state and callers should not have to guard every
// send. A malformed one is a value somebody entered that cannot mean what it
// appears to, and silence there loses the only signal that the record needs
// fixing.
//
// # Numbers reach the logs
//
// On success the recipient's number is logged. On failure it is in the error
// text, so a caller that logs the error puts it wherever its logs go. Both
// are deliberate: without the number the line cannot say WHICH notification
// failed. If that trade is revisited, masking belongs in both places —
// suppressing the error text alone still leaves every delivered number in
// the log.
//
// # A *Service is not nil-safe; *whatsmeow.Client mostly is
//
// The two cannot be reasoned about together. Service does not guard its
// nil receiver, and a caller holding a nil *Service because WhatsApp is not
// configured has to check it.
//
// whatsmeow guards the SEND paths, which is what the tests below rely on,
// but not uniformly. SendMessage checks for a nil client on entry. Upload
// does not: it encrypts the whole payload first and fails with the same
// ErrClientIsNil two calls deep, when it reaches the media connection — so
// a nil-client SendDocument does the encryption work before failing.
// Connect has no guard at all and would panic, which is harmless only
// because this package calls it on nothing but a client NewClient has just
// built. That was read from whatsmeow's own source at the pinned version;
// re-read it before relying on any of it for a new call site.
//
// The trap is that the no-op paths survive a nil receiver, because they
// return before reading any field. A smoke test with a blank number
// therefore suggests the whole type is nil-safe. It is not, and a call
// dispatched into a detached goroutine has nobody to catch the panic.
//
// Otherwise a Service is safe for concurrent use: whatsmeow's SendMessage
// takes its own write lock, and this struct is read-only after New returns.
//
// # Contexts
//
// Each entry point has a shortcut form and an explicit-context form, the
// same split the database package uses. The shortcuts pass
// context.Background, which means no deadline and no cancellation at all.
//
// That matters most for SendDocument, which uploads the bytes to WhatsApp's
// media servers before it sends anything. It is noticeably slower than
// SendText and bounded only by whatsmeow's own transport, so a request path
// that needs to return should use SendDocumentContext and pass its own
// deadline — or hand the send to a worker and return without it.
//
// # What the tests hold in place
//
// There is no integration suite, and the reason is not that one would be
// hard to write. It is that a meaningful one would need a paired device and
// would send real messages to a real phone on every run, which is not a
// thing to put in CI. Everything below is what can be held without that.
//
// The seam is a Service whose client is a NIL *whatsmeow.Client. Service
// holds a concrete type rather than an interface, so nothing can be
// substituted for it, and whatsmeow's own nil guards turn every send into a
// predictable error. That reaches the no-op paths, the normalisation, the
// ErrInvalidPhone rejection, the error wrapping and the log lines — which is
// to say everything this package actually decides.
//
// What it cannot reach is the JID construction, because a JID is built and
// consumed within a single statement and never becomes an argument a test
// can inspect. Swapping the user server for the group server would pass
// every behavioural test here. So one test parses this package's own source
// instead and asserts that every types.NewJID call uses the user server and
// that there are exactly two of them — meaning a third send path added later
// has to come and add itself rather than inheriting silence.
package whatsapp
