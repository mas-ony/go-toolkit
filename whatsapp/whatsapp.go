package whatsapp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/mdp/qrterminal/v3"
	// SQLite driver for whatsmeow's credential store. Blank import: the
	// package is never referenced by name, only for the side effect of its
	// init() registering the "sqlite3" driver name. Pure Go — SQLite built
	// to Wasm and translated to Go ahead of time — so there is no cgo and
	// no gcc at build time. See NewContext for why the registered NAME is
	// what picked this driver, and what the sandbox costs per connection.
	_ "github.com/ncruces/go-sqlite3/driver"
	"github.com/rs/zerolog"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

// Service is the application-level WhatsApp client. It is safe for concurrent
// use: whatsmeow's SendMessage acquires its own internal mutex before writing
// to the WebSocket (socket.NoiseSocket.SendFrame takes writeLock), and this
// struct is read-only after New returns.
//
// It is NOT safe to call on a nil receiver, unlike the send paths of
// *whatsmeow.Client, which return ErrClientIsNil instead of panicking — so
// the two cannot be reasoned about together. (whatsmeow's guards are not
// uniform either; the package documentation says which of its methods have
// them.) A caller holding a *Service that is nil when WhatsApp is not
// configured has to check it, and a call dispatched into a detached goroutine
// has nobody to catch the panic when it does not.
//
// The trap is that the no-op paths in SendText and SendDocument survive a nil
// receiver, because they return before reading any field. A smoke test with a
// blank number therefore suggests the whole type is nil-safe.
type Service struct {
	client *whatsmeow.Client
	log    zerolog.Logger
}

// storePragmas are the SQLite pragmas the credential store is opened with.
//
// foreign_keys(1) enforces referential integrity in whatsmeow's own tables
// (device, identity and session rows). Without it SQLite silently allows
// orphaned rows, which can leave inconsistent pairing state after an
// unclean shutdown.
//
// busy_timeout(10000) is here because naming ANY pragma suppresses the
// driver's default. It applies a one-minute busy timeout only when the DSN
// carries no _pragma at all, so asking for foreign keys alone would leave
// NO timeout — a harsher setting than the default, arrived at by asking for
// something unrelated to it.
//
// The timeout is what to look at if session writes start failing with
// "database is locked". sqlstore hands this string to database/sql, whose
// pool opens more than one connection, and SQLite under the default
// rollback journal refuses a second concurrent writer immediately rather
// than waiting for the first. Ten seconds is far longer than a credential
// write needs and turns that refusal into a wait. WAL mode through
// _pragma=journal_mode(wal), or a pool capped at one connection through
// sqlstore.NewWithDB, each close it from a different side.
//
// ORDER MATTERS. The driver documents busy timeout as one of the pragmas
// that must be set first, which is why it precedes foreign_keys rather than
// reading in the order they are explained above.
const storePragmas = "_pragma=busy_timeout(10000)&_pragma=foreign_keys(1)"

// The column width of the rules framing the QR block on stdout, and the
// text the top rule opens with. A mismatch between the two rules is only
// visible once a code is on screen, so both are built from these rather than
// written out as literals that have to be counted by hand.
const (
	qrBannerWidth = 49
	qrBannerLabel = "── WhatsApp QR Code "
)

// DefaultStorePath is the credential database New opens when no path is
// given. It is RELATIVE, so it resolves against the process working
// directory — which is the whole reason NewContext takes a path: two
// services started from the same directory would otherwise share one file
// and fight over a single device registration.
const DefaultStorePath = "whatsapp.db"

// phoneReplacer is built once at package init rather than per call.
// strings.Replacer is safe for concurrent use, which matters here because
// SendText is documented as safe to call from multiple goroutines.
var phoneReplacer = strings.NewReplacer("+", "", " ", "", "-", "")

// sqliteURIEscaper percent-encodes the three characters SQLite's URI parser
// reads as syntax rather than as filename: "?" opens the query, "#" opens
// the fragment, and "%" opens a percent-escape.
//
// Exactly three, and the narrowness is the point — the same reasoning the
// database package applies to a DSN. Escaping more would corrupt ordinary
// paths, since SQLite decodes what it is given; escaping fewer means a
// directory named "reports#2" silently truncates the path and opens a
// different, empty database in which the device is unpaired.
//
// strings.Replacer makes one pass and never rescans its own output, so the
// "%" rule cannot re-encode the escapes the other two produce.
var sqliteURIEscaper = strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23")

// QR banner rules. They are built rather than spelled out because a literal
// row of box-drawing characters costs three bytes per column, which puts the
// source line well past any sane line-length limit while telling the reader
// nothing the construction below does not.
var (
	qrBannerTop    = "\n" + qrRule(qrBannerLabel)
	qrBannerBottom = qrRule("")
)

// ErrNotPaired is returned by New when the QR window closed without the
// device being linked. It is a sentinel so a caller can tell "WhatsApp is not
// set up yet" apart from a store or network failure, which is the difference
// between an expected first-run state and something broken.
var ErrNotPaired = errors.New("whatsapp: QR window closed without pairing")

// ErrInvalidPhone is returned by SendText and SendDocument for a recipient
// that still holds something other than a digit once normalised.
//
// It is a sentinel because the cause is a bad stored value rather than a
// failure to deliver: a caller that retries transient send failures should
// not retry this one, and one that reports per-recipient outcomes wants to
// say which of the two happened.
var ErrInvalidPhone = errors.New("whatsapp: phone number is not digits")

// phoneDigits reports whether a normalised number is safe to build a JID
// from, which for a user JID means digits and nothing else.
//
// It is a character check, not a plausibility check: a number with the wrong
// digit count, or one missing its country code, satisfies it and fails at
// delivery instead. Validating the shape belongs where the number is entered.
//
// The empty string satisfies it vacuously. That case is the callers' silent
// no-op — an unfilled phone column, not bad data — and is checked before
// this.
func phoneDigits(phone string) bool {
	for _, r := range phone {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// qrRule pads label with box-drawing characters out to qrBannerWidth
// columns.
//
// The pad is counted in RUNES and not bytes, which is the whole reason this
// is a function. "─" is three bytes wide, so subtracting len(label) gives a
// rule a third of the intended length against a label that is itself
// box-drawing, and the two rules stop matching.
//
// The floor at zero is for a label somebody later makes longer than the
// width. strings.Repeat panics on a negative count, and this runs during
// package initialisation, so the panic would take the process down before it
// could log what was wrong with it.
func qrRule(label string) string {
	n := max(qrBannerWidth-utf8.RuneCountInString(label), 0)
	return label + strings.Repeat("─", n)
}

// normalisePhone strips the punctuation a WhatsApp JID cannot carry, leaving
// the bare digits a JID user part needs.
//
// Only "+", spaces and hyphens are removed. Everything else survives,
// including parentheses and dots, and the dot is worse than "malformed":
// types.ParseJID splits the user part on "." and reads the second half as an
// AGENT, so "628.1@s.whatsapp.net" is not rejected — it is a well-formed
// AD-JID naming a device. Two dots is a parse error; exactly one is silently
// a different address.
//
// Keeping the set narrow rather than reducing to digits is a decision, not an
// oversight. The number is free text on a stored record and typically reaches
// SendText unvalidated, so a wider filter would silently repair typed junk
// into a number nobody verified, trading a rejected send for one addressed to
// a number nobody typed. What survives this is therefore refused by the
// callers rather than repaired; see phoneDigits and ErrInvalidPhone.
func normalisePhone(phone string) string {
	return phoneReplacer.Replace(phone)
}

// storeDSN builds the SQLite URI for a credential store at path, falling
// back to DefaultStorePath when path is empty.
//
// "file:" with a single slash covers both shapes: "file:whatsapp.db" is
// relative to the working directory and "file:/var/lib/app/whatsapp.db" is
// absolute, so nothing here has to tell them apart.
func storeDSN(path string) string {
	return "file:" + sqliteURIEscaper.Replace(storeFile(path)) + "?" +
		storePragmas
}

// storeFile is the credential store's path as NewContext opens it: path,
// or DefaultStorePath when path is empty. The ErrNotPaired message names it,
// since telling an operator to put a paired store in place is only useful
// if it says where, and a caller that passed its own path is not reading
// DefaultStorePath.
func storeFile(path string) string {
	if path == "" {
		return DefaultStorePath
	}
	return path
}

// New creates and connects a WhatsApp Service.
//
// On first run (no existing session in whatsapp.db), whatsmeow requests a QR
// code from WhatsApp's servers; New renders it as a scannable QR block on
// stdout and additionally emits the raw code as an INFO log entry with key
// "qr". Scan the code once with the WhatsApp mobile app under Linked Devices
// → Link a Device. After pairing, the credentials are stored in whatsapp.db
// and subsequent starts connect automatically without a QR code. The
// whatsmeow credential tables are created on first run; no manual migration
// is needed.
//
// That log line carries a live credential, which is the cost of the plaintext
// fallback. The code is the whole pairing payload, so anyone who can read the
// log while it is still current can render it and link a device of their own.
// Each code expires with its cycle, which bounds the window rather than
// closing it: for the length of the pairing attempt, read access to the log
// is access to the account. Pair somewhere the log is not shared, or drop the
// field and keep the terminal rendering alone.
//
// Blocking behaviour, which is the operational hazard on this path. On first
// run New blocks until pairing completes or whatsmeow closes the QR channel,
// and the channel is only closed once the codes stop refreshing, on the order
// of minutes rather than seconds. A process started with an empty whatsapp.db
// therefore stalls for that whole window before it does anything else. Pair
// the device before first use, or start with an already-paired whatsapp.db in
// place. The wait takes its bound from the context handed to GetQRChannel,
// and the one below is context.Background, so it has none.
//
// An unscanned QR window is an ERROR, not a success. The client is connected
// but not logged in at that point, and returning it would hand the caller a
// non-nil Service that passes every nil check and then fails at every send.
// ErrNotPaired lets a caller log the state and carry on with notifications
// disabled, which is the outcome that path should get.
func New(log zerolog.Logger) (*Service, error) {
	return NewContext(context.Background(), log, DefaultStorePath)
}

// NewContext is New with an explicit context and credential store path.
//
// ctx bounds the QR wait, which is the only thing in this package that can
// block for minutes. New cannot bound it at all, so a process that must
// come up within a deadline — or come down on a signal while still
// unpaired — uses this and passes a context it can cancel. Cancelling it
// ends the wait; it does not un-pair a device that has already been linked.
//
// The context does NOT outlive the call. The Service it returns holds a
// WebSocket that lives until Disconnect, and every send is bounded by the
// context its own caller passes, so handing a request context here would
// be a category mistake rather than a tighter bound.
//
// storePath names the credential database. An empty string means
// DefaultStorePath, so a caller that wants the default but needs the
// context can ask for one thing without the other.
func NewContext(
	ctx context.Context,
	log zerolog.Logger,
	storePath string,
) (*Service, error) {

	// Bridge zerolog to whatsmeow's internal logger so all whatsmeow events
	// (connection status, message receipts, errors) flow through the same
	// structured logger as the rest of the application.
	waLogger := waLog.Zerolog(
		log.With().Str("component", "whatsmeow").Logger())

	// "sqlite3" does two jobs here: sqlstore passes it to sql.Open as the
	// driver name AND uses it to choose its own SQL dialect. That coupling
	// is what picked the driver. A driver registered under any other name
	// forces the two apart — modernc.org/sqlite registers "sqlite", and
	// reaching it means opening the *sql.DB here and switching to
	// sqlstore.NewWithDB so whatsmeow still hears "sqlite3". This driver
	// registers "sqlite3" itself, so the one name serves both jobs and
	// nothing needs bridging.
	//
	// NewWithDB is also the way in if this ever needs SetMaxOpenConns.
	// Every pooled connection runs in its own Wasm instance, so the pool
	// costs more memory here than it would with a cgo driver, and capping
	// it at one would close the locking question above from the other
	// side. Neither is worth doing for a store this small until something
	// says otherwise.
	container, err := sqlstore.New(ctx, "sqlite3", storeDSN(storePath),
		waLogger)
	if err != nil {
		return nil, fmt.Errorf("whatsapp: init store: %w", err)
	}

	// sqlstore.New opens a *sql.DB, so every failure below has to close it.
	// The error this function returns is meant to be non-fatal, so a leaked
	// container is not collected by a dying process: it holds the
	// whatsapp.db file handle and a database/sql pool for the life of a
	// service that is still answering requests.
	//
	// A flag rather than a plain defer, because the SUCCESS path must NOT
	// close it. whatsmeow writes session state through this container for as
	// long as the client lives, so a returned Service depends on it staying
	// open.
	//
	// Which leaves nothing closing it on that path either, and the process
	// exiting is what does the job. Adequate for SQLite, and it still means
	// Service uses a resource it does not own. Closing it from Disconnect
	// needs care, since whatsmeow goroutines can still be writing when
	// Disconnect returns and would then log against a closed database.
	retained := false
	defer func() {
		if !retained {
			_ = container.Close()
		}
	}()

	// GetFirstDevice returns the stored device record if one exists, or
	// creates a new empty device record if the database is empty (first run).
	deviceStore, err := container.GetFirstDevice(ctx)
	if err != nil {
		return nil, fmt.Errorf("whatsapp: get device: %w", err)
	}

	client := whatsmeow.NewClient(deviceStore, waLogger)

	if client.Store.ID != nil {
		// Existing session — connect directly without QR.
		if err := client.Connect(); err != nil {
			return nil, fmt.Errorf("whatsapp: connect: %w", err)
		}
		log.Info().Msg("whatsapp: client connected")
		retained = true
		return &Service{client: client, log: log}, nil
	}

	// No session credentials in the store — the device has not been paired
	// yet. Request a QR channel before connecting so the code can be printed
	// as it arrives.
	//
	// The error is reported rather than discarded because of what discarding
	// it costs. GetQRChannel hands back a nil channel alongside it, and the
	// range below blocks forever on a nil channel, so the failure would
	// present as a process that starts, logs nothing further, and never
	// serves a request — the one outcome no caller can diagnose.
	//
	// Whatsmeow refuses the call when the client is nil, when it is already
	// connected, or when the store already holds a device ID. None of the
	// three is reachable here, which is why reaching the hang would take a
	// change in whatsmeow rather than a bug in this function.
	qrChan, err := client.GetQRChannel(ctx)
	if err != nil {
		return nil, fmt.Errorf("whatsapp: get QR channel: %w", err)
	}
	if err := client.Connect(); err != nil {
		// qrChan is abandoned here rather than drained. Nothing feeds a
		// channel whose connection never opened, and nothing closes it
		// either: the context below is the only thing that would, and it
		// is context.Background. A cancellable context is the lever if
		// this path ever needs to be tidy rather than merely correct.
		return nil, fmt.Errorf("whatsapp: connect for QR: %w", err)
	}

	// The QR channel emits one "code" event per QR cycle (codes refresh
	// every ~20 s) until the user scans or the attempt times out.
	for evt := range qrChan {
		if evt.Event == "code" {
			// Render the QR code as half-block Unicode characters
			// directly to stdout so it is visible in the terminal or
			// the captured log. GenerateHalfBlock produces the most
			// compact output that all modern terminals can display
			// without extra libraries.
			//
			// The write discards are explicit: a failed write to stdout
			// is not worth failing pairing over, and the structured log
			// line below is the fallback for exactly that case, and for
			// a terminal that cannot render Unicode.
			_, _ = fmt.Fprintln(os.Stdout, qrBannerTop)
			qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
			_, _ = fmt.Fprintln(os.Stdout, qrBannerBottom)
			log.Info().
				Str("qr", evt.Code).
				Msg("whatsapp: scan QR code to link device (plaintext)")
			continue
		}
		log.Info().Str("event", evt.Event).Msg("whatsapp: QR channel event")
	}

	// The channel closing does not mean the device was paired. Store.ID is
	// what says so: whatsmeow writes it once the pairing handshake
	// completes, so a nil ID here means the window expired unscanned.
	//
	// Disconnect before returning, because Connect above opened a WebSocket
	// that nothing else holds a reference to once this function returns an
	// error.
	if client.Store.ID == nil {
		client.Disconnect()
		return nil, fmt.Errorf(
			"%w; scan the code shown above, or start with a paired "+
				"store already at %s", ErrNotPaired, storeFile(storePath))
	}

	log.Info().Msg("whatsapp: client connected")
	retained = true
	return &Service{client: client, log: log}, nil
}

// SendText delivers a plain-text WhatsApp message to the given phone number.
//
// phone must be in E.164-compatible format: digits only, starting with
// the country code, e.g. "628123456789". "+", spaces, and hyphens are
// stripped by normalisePhone wherever they appear, before the JID is
// constructed, so "62 812-3456789" and "+62 812-3456789" both work. Other
// characters (parentheses, dots, etc.) are NOT stripped; a number still
// holding one after normalisation is refused with ErrInvalidPhone rather
// than sent. See normalisePhone for why refusing beats filtering further.
//
// The resulting WhatsApp JID has the form "<phone>@s.whatsapp.net"
// (types.DefaultUserServer). Group JIDs (@g.us) are not supported; this
// function is for individual notifications only.
//
// An empty message, or a phone that is empty ONCE NORMALISED, is a silent
// no-op (returns nil without sending). A missing phone number is an expected
// operational state — the column has not been filled in yet — and callers
// should not need to guard every send with a non-empty check.
//
// Empty and malformed are deliberately NOT the same answer. Nothing was asked
// for in the first case and the right response is silence; the second is a
// value somebody entered that cannot mean what it appears to, and silence
// there loses the only signal that the row needs fixing.
//
// The guard runs on the NORMALISED value, which is not merely tidier. A field
// holding nothing but spaces would otherwise pass it, normalise to "", and
// produce types.NewJID("", DefaultUserServer), which renders as the bare
// "s.whatsapp.net", WhatsApp's own address rather than any user's. The send
// then fails somewhere remote from here with nothing pointing back at a phone
// column holding one space.
//
// A returned error names the recipient's number, and that is a deliberate
// trade rather than an oversight. A caller that logs the error puts a mobile
// number wherever its logs go. The number is kept because without it the line
// cannot say WHICH notification failed.
//
// The success path makes the same trade without the caller's involvement: it
// logs the number itself, on every send. So masking the middle digits, the
// remedy if the trade is revisited, belongs in both places, and a caller that
// suppresses the error text alone still leaves every delivered number in the
// log.
//
// Safe for concurrent use, and unsafe on a nil receiver; see Service.
func (s *Service) SendText(phone, message string) error {
	return s.SendTextContext(context.Background(), phone, message)
}

// SendTextContext is SendText with an explicit context, which is the only
// way to bound how long a send may take: whatsmeow's transport applies its
// own deadlines and this package adds none.
//
// A cancelled context stops the send. It says nothing about DELIVERY — a
// message whose write reached WhatsApp before the cancellation may still
// arrive, so a caller that retries on cancellation should expect the
// recipient to see it twice rather than not at all.
//
// The no-op cases are checked before the context is, so an empty recipient
// or message returns nil even under a context that is already done. That
// keeps "nothing to send" a single answer rather than one that depends on
// timing.
func (s *Service) SendTextContext(
	ctx context.Context,
	phone, message string,
) error {
	// Normalise to bare digits BEFORE the guard. WhatsApp JIDs require no
	// punctuation, and a number that normalises away to nothing is as empty
	// as one that arrived empty.
	digits := normalisePhone(phone)
	if digits == "" || message == "" {
		return nil
	}
	if !phoneDigits(digits) {
		return fmt.Errorf("%w: %q", ErrInvalidPhone, phone)
	}

	jid := types.NewJID(digits, types.DefaultUserServer)

	// SendMessage returns (types.SendResponse, error). The SendResponse
	// contains the server-assigned message ID and timestamp, which are not
	// needed for fire-and-forget notifications. Assign to _ to make the
	// discard explicit.
	_, err := s.client.SendMessage(ctx, jid, &waE2E.Message{
		Conversation: proto.String(message),
	})
	if err != nil {
		return fmt.Errorf("whatsapp: send to %s: %w", digits, err)
	}

	s.log.Info().Str("to", digits).Msg("whatsapp: message sent")
	return nil
}

// SendDocument delivers a document attachment (PDF, DOCX, etc.) to the given
// phone number, with an optional caption shown above the file in the chat.
//
// data is the raw file bytes. filename is shown to the recipient as the
// document's name and should include the extension (e.g. "report.pdf");
// mimetype should match the actual content type (e.g. "application/pdf").
//
// Sending a document is a two-step whatsmeow operation, unlike SendText's
// single call: Upload first encrypts and uploads the bytes to WhatsApp's
// media servers, returning the URL and keys needed to reference the blob;
// SendMessage then sends a DocumentMessage pointing at that uploaded blob,
// which is the actual chat message the recipient sees. Because of the upload
// step this is noticeably slower than SendText and depends on network
// conditions — avoid calling it on a request path that needs to return
// quickly.
//
// This form and SendText pass context.Background, so neither bounds how
// long a send takes, and the only deadline is whatever whatsmeow's own
// transport applies. SendDocumentContext is the form that can, and the one
// to use on any path that has to return.
//
// Empty-input behaviour is NOT identical to SendText's. Both no-op on a
// recipient that is empty once normalised, and this one also no-ops on empty
// data. But SendText refuses an empty MESSAGE, whereas filename, mimetype and
// caption are unchecked here: an empty filename ships FileName:"" and the
// recipient receives a nameless attachment. That is deliberate — an empty
// message is nothing to deliver, while a nameless document still delivers its
// contents, and a notification that arrives badly labelled beats one that
// silently does not arrive. A caller passing a stored filename straight
// through should expect an empty column to reach this unchecked.
//
// Phone normalisation, the ErrInvalidPhone rejection, the error message
// naming the recipient, and the non-nil-receiver requirement are all as
// described on SendText.
func (s *Service) SendDocument(
	phone string,
	data []byte,
	filename, mimetype, caption string,
) error {
	return s.SendDocumentContext(
		context.Background(), phone, data, filename, mimetype, caption)
}

// SendDocumentContext is SendDocument with an explicit context, and it is
// the one this package most wants a caller to reach for.
//
// The upload is a network round trip proportional to the size of data, so
// this is the slowest thing here by a wide margin. Under
// context.Background it has no bound whatsoever; under a request's own
// context it ends when the request does.
//
// The context covers BOTH steps, which is what makes a cancellation
// mid-flight ordinary rather than exceptional: an upload may complete and
// the send that would reference it never happen, leaving encrypted bytes
// on WhatsApp's media servers that nothing points at. Nothing here cleans
// those up, and nothing needs to — they expire on their own and the
// recipient never saw a message.
func (s *Service) SendDocumentContext(
	ctx context.Context,
	phone string,
	data []byte,
	filename, mimetype, caption string,
) error {
	// Normalise before the guard, for the reason given on SendText.
	digits := normalisePhone(phone)
	if digits == "" || len(data) == 0 {
		return nil
	}
	if !phoneDigits(digits) {
		return fmt.Errorf("%w: %q", ErrInvalidPhone, phone)
	}

	jid := types.NewJID(digits, types.DefaultUserServer)

	// Upload encrypts data and stores it on WhatsApp's media servers,
	// returning the URL and encryption keys needed to reference it from a
	// message.
	uploaded, err := s.client.Upload(ctx, data, whatsmeow.MediaDocument)
	if err != nil {
		return fmt.Errorf("whatsapp: upload document for %s: %w",
			digits, err)
	}

	docMsg := &waE2E.DocumentMessage{
		URL:           &uploaded.URL,
		DirectPath:    &uploaded.DirectPath,
		MediaKey:      uploaded.MediaKey,
		FileEncSHA256: uploaded.FileEncSHA256,
		FileSHA256:    uploaded.FileSHA256,
		FileLength:    &uploaded.FileLength,
		Mimetype:      proto.String(mimetype),
		FileName:      proto.String(filename),
	}
	if caption != "" {
		docMsg.Caption = proto.String(caption)
	}

	_, err = s.client.SendMessage(ctx, jid, &waE2E.Message{
		DocumentMessage: docMsg,
	})
	if err != nil {
		return fmt.Errorf("whatsapp: send document to %s: %w",
			digits, err)
	}

	s.log.Info().
		Str("to", digits).
		Str("file", filename).
		Msg("whatsapp: document sent")
	return nil
}

// Disconnect gracefully closes the WhatsApp WebSocket connection.
// Call this during application shutdown after all in-flight requests have
// completed to ensure any pending message acknowledgements are flushed.
//
// Nothing in this package registers it. A caller that wants the WebSocket
// closed rather than dropped has to wire it into its own shutdown sequence,
// a signal handler for instance.
//
// It does not close the credential store; see the note in New.
func (s *Service) Disconnect() {
	s.client.Disconnect()
	s.log.Info().Msg("whatsapp: disconnected")
}
