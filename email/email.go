package email

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/mas-ony/go-toolkit/config"
)

// helloName is the name this client gives in EHLO, the one net/smtp gives
// when told nothing. A submission relay ignores it for a client that logs
// in. A relay that insists on a fully qualified name refuses it, and the
// error names EHLO as the step that failed.
const helloName = "localhost"

// loopbackHosts are the host spellings credentials may cross to without
// TLS: the three net/smtp's PlainAuth accepts, and the three
// config.EmailConfig.Validate accepts. A test holds the last two equal.
var loopbackHosts = map[string]bool{
	"localhost": true,
	"127.0.0.1": true,
	"::1":       true,
}

// auth logs in with PLAIN when the server offers it and with LOGIN
// otherwise. LOGIN answers two prompts in turn, so a value serves one
// conversation and deliver builds one per send.
type auth struct {
	username, password string
	mech               string
	prompts            int
}

// errNoStartTLS ends a starttls send to a server that does not offer the
// upgrade.
var errNoStartTLS = errors.New("the server does not offer it, and a " +
	"connection configured to be encrypted does not continue without")

// ErrNotConfigured is returned by New when there is no relay to send
// through, and by Send and SendText on a nil *Service.
//
// From New it is fatal wherever notification.channels includes email: the
// channel was chosen and its section is missing. From a send it means
// email is switched off, and it is a sentinel so a caller can tell that
// apart from a failure to deliver.
var ErrNotConfigured = errors.New("email: not configured")

// ErrInvalidAddress is returned by Send for a To, Cc, Bcc or Reply-To
// entry that does not parse as one address.
//
// It is a sentinel because the cause is a bad stored value rather than a
// failure to deliver: a caller that retries failed sends should not retry
// this one, and one that reports per-recipient outcomes wants to say
// which of the two happened.
var ErrInvalidAddress = errors.New("email: invalid address")

// ErrTooLarge is returned by Send when the relay announces a size limit
// the message exceeds. It is checked before MAIL FROM, so nothing about
// the message reached the relay, and sending it again fails the same way.
var ErrTooLarge = errors.New("email: message exceeds the relay's size limit")

// Service sends mail through one SMTP relay.
//
// It holds no connection: each send dials, delivers and hangs up, so a
// Service is nothing but settings, read-only after New and safe for
// concurrent use.
//
// It is also safe on a nil receiver, where Send and SendText return
// ErrNotConfigured. That is what lets a caller keep a nil *Service when
// email is not among notification.channels and call it unconditionally,
// rather than guard every call site — including one in a detached
// goroutine, where a panic would have nobody to catch it.
type Service struct {
	host string
	addr string
	mode string

	// tlsConf is shared by every send, which tls allows as long as
	// nothing modifies it after New. nil under tls: none.
	tlsConf *tls.Config

	username string
	password string
	from     *mail.Address
	timeout  time.Duration
	log      zerolog.Logger
}

// offers reports whether mechs lists mech, in any case.
func offers(mechs []string, mech string) bool {
	for _, m := range mechs {
		if strings.EqualFold(m, mech) {
			return true
		}
	}
	return false
}

// messageID returns a new Message-ID under the sender's domain. The
// random part is rand.Text, 128 bits, which is what keeps it unique
// without any state here.
func messageID(from string) string {
	domain := "localhost"
	if at := strings.LastIndexByte(from, '@'); at >= 0 &&
		at < len(from)-1 {
		domain = from[at+1:]
	}
	return "<" + rand.Text() + "@" + domain + ">"
}

// names lists recipients for an error message, the first few by address
// and the rest by count, so a send to a whole department does not produce
// an error line longer than the message.
func names(rcpts []string) string {
	const shown = 3
	if len(rcpts) <= shown {
		return strings.Join(rcpts, ", ")
	}
	return fmt.Sprintf("%s and %d more",
		strings.Join(rcpts[:shown], ", "), len(rcpts)-shown)
}

// stepErr names the step of the conversation err ended. When ctx has
// ended too, its error is wrapped alongside, since a deadline surfaces
// from the connection as an I/O timeout that mentions no context.
func stepErr(ctx context.Context, step string, err error) error {
	if cerr := ctx.Err(); cerr != nil {
		return fmt.Errorf("%s: %w: %w", step, cerr, err)
	}
	return fmt.Errorf("%s: %w", step, err)
}

// dial opens the connection within ctx: TCP, and under implicit TLS the
// handshake too.
func (s *Service) dial(ctx context.Context) (net.Conn, error) {
	if s.mode == config.EmailTLSImplicit {
		d := tls.Dialer{Config: s.tlsConf}
		return d.DialContext(ctx, "tcp", s.addr)
	}
	var d net.Dialer
	return d.DialContext(ctx, "tcp", s.addr)
}

// deliver runs one SMTP conversation: connect, greet, secure, log in, and
// hand over msg for rcpts.
//
// net/smtp takes no context, so the bound is applied to the connection.
// ctx gains the configured timeout, and the moment it ends — by deadline
// or by cancellation — the connection's deadline moves to now, which
// fails whatever read or write is blocked. stepErr then wraps ctx's error
// in alongside the connection's, so a caller tests for
// context.DeadlineExceeded rather than for an I/O timeout it never set.
//
// The connection's deadline is never set from ctx's deadline directly.
// Those would be two clocks, and if the connection's fired first the
// error would carry no context error at all.
func (s *Service) deliver(
	ctx context.Context,
	rcpts []string,
	msg []byte,
) error {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	conn, err := s.dial(ctx)
	if err != nil {
		return stepErr(ctx, "connect", err)
	}
	stop := context.AfterFunc(ctx, func() {
		_ = conn.SetDeadline(time.Now())
	})
	defer stop()

	// NewClient closes the connection itself when the greeting fails.
	c, err := smtp.NewClient(conn, s.host)
	if err != nil {
		return stepErr(ctx, "greeting", err)
	}
	defer c.Close()

	if err := c.Hello(helloName); err != nil {
		return stepErr(ctx, "EHLO", err)
	}
	// Strict, for the reason on config.EmailConfig.TLS: the offer arrives
	// in clear text, so carrying on without it would hand the message and
	// the password to whoever deleted it.
	if s.mode == config.EmailTLSStartTLS {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return stepErr(ctx, "STARTTLS", errNoStartTLS)
		}
		if err := c.StartTLS(s.tlsConf); err != nil {
			return stepErr(ctx, "STARTTLS", err)
		}
	}
	if s.username != "" {
		a := &auth{username: s.username, password: s.password}
		if err := c.Auth(a); err != nil {
			return stepErr(ctx, "AUTH", err)
		}
	}

	// net/smtp does not declare the size in MAIL FROM, so a relay can
	// refuse an oversized message only after the whole upload. Checking
	// the announced limit first turns that wasted transfer into an error
	// that says why.
	if ok, param := c.Extension("SIZE"); ok {
		limit, err := strconv.ParseInt(param, 10, 64)
		if err == nil && limit > 0 && int64(len(msg)) > limit {
			return fmt.Errorf("%w: %d bytes against a limit of %d",
				ErrTooLarge, len(msg), limit)
		}
	}

	if err := c.Mail(s.from.Address); err != nil {
		return stepErr(ctx, "MAIL FROM", err)
	}
	for _, r := range rcpts {
		if err := c.Rcpt(r); err != nil {
			return stepErr(ctx, "RCPT TO <"+r+">", err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return stepErr(ctx, "DATA", err)
	}
	if _, err := w.Write(msg); err != nil {
		return stepErr(ctx, "upload", err)
	}
	// Close sends the terminating dot and reads the relay's verdict on
	// the whole message. This reply is the one that decides the send.
	if err := w.Close(); err != nil {
		return stepErr(ctx, "end of DATA", err)
	}

	// Accepted. The reply to QUIT is not checked; see the package
	// documentation.
	_ = c.Quit()
	return nil
}

// New returns a Service for the relay cfg describes.
//
// It does not dial. A relay that is down, or credentials it refuses,
// fail per send rather than at startup, where a mail outage would stop a
// service whose main job is something else.
//
// ErrNotConfigured means there is nothing to send with: cfg is nil or
// holds no setting at all, which is what a deployment without
// notification.email.* produces. Call New only when notification.channels
// includes email, and treat that error as fatal there. Any other problem
// is Validate's joined error, so a section filled in halfway fails loudly
// instead of quietly switching mail off.
func New(cfg *config.EmailConfig, log zerolog.Logger) (*Service, error) {
	if cfg == nil || *cfg == (config.EmailConfig{}) {
		return nil, ErrNotConfigured
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	// Validate has parsed From already. The error is checked all the
	// same, because a nil address would panic on every send.
	from, err := mail.ParseAddress(cfg.From)
	if err != nil {
		return nil, fmt.Errorf("notification.email.from: %w", err)
	}

	host := strings.TrimSpace(cfg.Host)
	s := &Service{
		host:     host,
		addr:     net.JoinHostPort(host, strconv.Itoa(cfg.Port)),
		mode:     strings.ToLower(strings.TrimSpace(cfg.TLS)),
		username: cfg.Username,
		password: cfg.Password,
		from:     from,
		timeout:  cfg.Timeout,
		log:      log,
	}
	if s.mode != config.EmailTLSNone {
		s.tlsConf = &tls.Config{
			ServerName: host,
			MinVersion: tls.VersionTLS12,
			//nolint:gosec // notification.email.insecure is the opt-in
			InsecureSkipVerify: cfg.Insecure,
		}
	}
	return s, nil
}

// Start refuses credentials over a connection that is not encrypted
// unless the server is on loopback, the rule PlainAuth applies. New runs
// Validate, which applies it to the configuration, so a Service does not
// get here with credentials for a remote relay in clear text; the check
// stays so that the send path does not depend on that.
func (a *auth) Start(server *smtp.ServerInfo) (string, []byte, error) {
	if !server.TLS && !loopbackHosts[server.Name] {
		return "", nil, errors.New("refusing to send credentials over " +
			"a connection that is not encrypted")
	}
	switch {
	case offers(server.Auth, "PLAIN"):
		a.mech = "PLAIN"
		return a.mech, []byte("\x00" + a.username + "\x00" + a.password),
			nil
	case offers(server.Auth, "LOGIN"):
		a.mech = "LOGIN"
		return a.mech, nil, nil
	}
	offered := strings.Join(server.Auth, " ")
	if offered == "" {
		offered = "no mechanism"
	}
	return "", nil, fmt.Errorf("the server offers %s, and this client "+
		"logs in only with PLAIN or LOGIN", offered)
}

// Next answers the server's challenges. PLAIN sends everything up front
// and expects none. LOGIN is asked for the username, then the password;
// the prompts are counted rather than read, because servers word them
// differently and agree only on the order.
func (a *auth) Next(fromServer []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	if a.mech == "LOGIN" {
		a.prompts++
		switch a.prompts {
		case 1:
			return []byte(a.username), nil
		case 2:
			return []byte(a.password), nil
		}
	}
	return nil, fmt.Errorf("unexpected %s challenge %q", a.mech, fromServer)
}

// Send delivers m through the relay, over a connection opened for it and
// closed once the relay has taken it.
//
// Nothing to send is not an error. A message with no subject, body or
// attachment, or with no recipient once empty entries are skipped,
// returns nil without dialing. Both are checked before ctx is, so the
// answer does not depend on timing. An address that is present but does
// not parse is different, because silence would lose the only sign that
// the stored value needs fixing: it returns ErrInvalidAddress, also
// without dialing.
//
// notification.email.timeout and ctx both bound the send, and whichever
// ends first ends it; the error then unwraps to context.DeadlineExceeded
// or context.Canceled. A refusal from the relay unwraps to the
// *textproto.Error carrying its reply code.
//
// A returned error names the recipients, which puts addresses wherever a
// caller logs it. That is deliberate: without them the line cannot say
// which notification failed. The success line logged here names them
// too, with the Message-ID, which the relay's own log also records.
//
// Safe for concurrent use, and on a nil receiver; see Service.
func (s *Service) Send(ctx context.Context, m Message) error {
	if s == nil {
		return ErrNotConfigured
	}
	if m.empty() {
		return nil
	}
	env, err := parseEnvelope(m)
	if err != nil {
		return err
	}
	if len(env.rcpts) == 0 {
		return nil
	}

	id := messageID(s.from.Address)
	data, err := compose(s.from, env, m, time.Now(), id)
	if err != nil {
		return fmt.Errorf("email: compose: %w", err)
	}
	if err := s.deliver(ctx, env.rcpts, data); err != nil {
		return fmt.Errorf("email: send to %s: %w", names(env.rcpts), err)
	}

	s.log.Info().
		Strs("to", env.rcpts).
		Str("message_id", id).
		Msg("email: message sent")
	return nil
}

// SendText sends a plain-text message to one recipient. It is Send with a
// Message holding those fields, so every rule on Send applies, including
// that an empty to is nothing to send.
func (s *Service) SendText(
	ctx context.Context,
	to, subject, body string,
) error {
	return s.Send(ctx, Message{
		To:      []string{to},
		Subject: subject,
		Text:    body,
	})
}
