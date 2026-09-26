package email

// Tests for email.go.
//
// Every send here goes to relay, an SMTP server on loopback that answers
// the way a real one does and records what each connection told it. The
// assertions are about that record — what the relay was sent, and what it
// was never sent — rather than about what the client believes it did.
//
// Nothing here needs a certificate or waits on a clock. The TLS paths,
// the timeout and concurrent sends are in email_integration_test.go.

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/smtp"
	"net/textproto"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/mas-ony/go-toolkit/config"
)

// relay is an SMTP server on loopback. Its fields set its behaviour and
// are read-only once start has been called.
type relay struct {
	// ext lists extensions EHLO offers beyond the two below, "SIZE 2000"
	// say.
	ext []string

	// mechs are the AUTH mechanisms offered, none when empty, and users
	// the logins accepted. authAfterTLS offers AUTH only on an encrypted
	// connection, the way a submission server behaves.
	mechs        []string
	users        map[string]string
	authAfterTLS bool

	// tlsConf enables STARTTLS, or with implicit, TLS from the first
	// byte.
	tlsConf  *tls.Config
	implicit bool

	// refuse maps a recipient to the reply its RCPT gets.
	refuse map[string]string

	// silent accepts a connection and never greets it. stallAfterData
	// takes a whole message and never answers. dropQuit hangs up on QUIT
	// without a reply.
	silent         bool
	stallAfterData bool
	dropQuit       bool

	ln       net.Listener
	wg       sync.WaitGroup
	mu       sync.Mutex
	closed   bool
	conns    []net.Conn
	sessions []*session
}

// session is what one connection told the relay.
type session struct {
	helo    string
	tls     bool // encrypted by the end
	user    string
	mech    string
	authTLS bool // encrypted when the login happened
	from    string
	mailTLS bool // encrypted when MAIL FROM arrived
	rcpts   []string
	data    []byte // after dot-unstuffing, with LF line endings
}

// start listens on a free loopback port and serves until the test ends.
func (r *relay) start(t *testing.T) *relay {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if r.implicit {
		ln = tls.NewListener(ln, r.tlsConf)
	}
	r.ln = ln
	r.wg.Add(1)
	go r.accept()
	t.Cleanup(r.stop)
	return r
}

func (r *relay) stop() {
	r.mu.Lock()
	r.closed = true
	for _, c := range r.conns {
		_ = c.Close()
	}
	r.mu.Unlock()
	_ = r.ln.Close()
	r.wg.Wait()
}

func (r *relay) accept() {
	defer r.wg.Done()
	for {
		conn, err := r.ln.Accept()
		if err != nil {
			return
		}
		r.mu.Lock()
		if r.closed {
			r.mu.Unlock()
			_ = conn.Close()
			return
		}
		r.conns = append(r.conns, conn)
		r.mu.Unlock()

		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			r.serve(conn)
		}()
	}
}

// note records into s under the relay's lock, which is what a test's
// later read synchronises with.
func (r *relay) note(f func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f()
}

// seen returns a copy of every session so far.
func (r *relay) seen() []session {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]session, len(r.sessions))
	for i, s := range r.sessions {
		out[i] = *s
		out[i].rcpts = slices.Clone(s.rcpts)
		out[i].data = bytes.Clone(s.data)
	}
	return out
}

// only returns the one session the test expects.
func (r *relay) only(t *testing.T) session {
	t.Helper()
	s := r.seen()
	if len(s) != 1 {
		t.Fatalf("the relay saw %d connections, want 1", len(s))
	}
	return s[0]
}

func (r *relay) serve(conn net.Conn) {
	defer conn.Close()
	s := &session{}
	_, encrypted := conn.(*tls.Conn)
	r.note(func() {
		s.tls = encrypted
		r.sessions = append(r.sessions, s)
	})

	if r.silent {
		_, _ = io.Copy(io.Discard, conn)
		return
	}
	tp := textproto.NewConn(conn)
	reply := func(format string, args ...any) {
		_ = tp.PrintfLine(format, args...)
	}
	reply("220 relay.test ESMTP")

	for {
		line, err := tp.ReadLine()
		if err != nil {
			return
		}
		verb, arg, _ := strings.Cut(line, " ")
		switch strings.ToUpper(verb) {
		case "EHLO":
			r.note(func() { s.helo = arg })
			lines := append([]string{"relay.test"}, r.offer(encrypted)...)
			for i, l := range lines {
				sep := "-"
				if i == len(lines)-1 {
					sep = " "
				}
				reply("250%s%s", sep, l)
			}
		case "STARTTLS":
			if r.tlsConf == nil || encrypted {
				reply("502 5.5.1 not offered")
				continue
			}
			reply("220 2.0.0 ready")
			tc := tls.Server(conn, r.tlsConf)
			if err := tc.Handshake(); err != nil {
				return
			}
			tp = textproto.NewConn(tc)
			encrypted = true
			r.note(func() { s.tls = true })
		case "AUTH":
			if !slices.Contains(r.offer(encrypted),
				"AUTH "+strings.Join(r.mechs, " ")) {
				reply("503 5.5.1 AUTH not offered")
				continue
			}
			user, mech, ok := r.login(tp, arg)
			if !ok {
				reply("535 5.7.8 authentication failed")
				continue
			}
			r.note(func() {
				s.user, s.mech, s.authTLS = user, mech, encrypted
			})
			reply("235 2.7.0 authenticated")
		case "MAIL":
			from := between(arg, "<", ">")
			r.note(func() { s.from, s.mailTLS = from, encrypted })
			reply("250 2.1.0 ok")
		case "RCPT":
			to := between(arg, "<", ">")
			if answer, ok := r.refuse[to]; ok {
				reply("%s", answer)
				continue
			}
			r.note(func() { s.rcpts = append(s.rcpts, to) })
			reply("250 2.1.5 ok")
		case "DATA":
			reply("354 end with <CRLF>.<CRLF>")
			data, err := tp.ReadDotBytes()
			if err != nil {
				return
			}
			r.note(func() { s.data = data })
			if r.stallAfterData {
				_, _ = io.Copy(io.Discard, tp.R)
				return
			}
			reply("250 2.0.0 queued")
		case "RSET", "NOOP":
			reply("250 2.0.0 ok")
		case "QUIT":
			if !r.dropQuit {
				reply("221 2.0.0 bye")
			}
			return
		default:
			reply("500 5.5.2 unrecognised")
		}
	}
}

// offer is what EHLO lists after the greeting line.
func (r *relay) offer(encrypted bool) []string {
	ext := slices.Clone(r.ext)
	if r.tlsConf != nil && !encrypted {
		ext = append(ext, "STARTTLS")
	}
	if len(r.mechs) > 0 && (encrypted || !r.authAfterTLS) {
		ext = append(ext, "AUTH "+strings.Join(r.mechs, " "))
	}
	return ext
}

// login runs the server half of AUTH PLAIN or LOGIN.
func (r *relay) login(
	tp *textproto.Conn,
	arg string,
) (user, mech string, ok bool) {
	mech, initial, _ := strings.Cut(arg, " ")
	mech = strings.ToUpper(mech)
	ask := func(prompt string) string {
		_ = tp.PrintfLine("334 %s",
			base64.StdEncoding.EncodeToString([]byte(prompt)))
		line, _ := tp.ReadLine()
		b, _ := base64.StdEncoding.DecodeString(line)
		return string(b)
	}

	var pass string
	switch mech {
	case "PLAIN":
		b, err := base64.StdEncoding.DecodeString(initial)
		fields := strings.Split(string(b), "\x00")
		if err != nil || len(fields) != 3 {
			return "", mech, false
		}
		user, pass = fields[1], fields[2]
	case "LOGIN":
		user = ask("Username:")
		pass = ask("Password:")
	default:
		return "", mech, false
	}
	want, known := r.users[user]
	return user, mech, known && pass == want
}

func between(s, open, close string) string {
	_, rest, _ := strings.Cut(s, open)
	inside, _, _ := strings.Cut(rest, close)
	return inside
}

// service builds a Service for r through New, the way an application
// does, after change has adjusted the configuration.
func service(
	t *testing.T,
	r *relay,
	change func(*config.EmailConfig),
) *Service {
	t.Helper()
	addr := r.ln.Addr().(*net.TCPAddr)
	cfg := &config.EmailConfig{
		Host:    "127.0.0.1",
		Port:    addr.Port,
		TLS:     config.EmailTLSNone,
		From:    "Notifikasi <noreply@example.go.id>",
		Timeout: 10 * time.Second,
	}
	if change != nil {
		change(cfg)
	}
	s, err := New(cfg, zerolog.Nop())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func login(user, pass string) func(*config.EmailConfig) {
	return func(c *config.EmailConfig) {
		c.Username, c.Password = user, pass
	}
}

// text is the decoded body of a single-part message.
func text(t *testing.T, raw []byte) string {
	t.Helper()
	return string(root(t, read(t, raw)).body)
}

func TestSendDeliversOneMessage(t *testing.T) {
	t.Parallel()
	r := (&relay{}).start(t)
	s := service(t, r, nil)
	var logs bytes.Buffer
	s.log = zerolog.New(&logs)

	// A line holding only "." ends DATA unless it is stuffed, which
	// would cut the message off there and leave the rest to be read as
	// commands. The body ends in a line break because SMTP ends every
	// message with one, and adds it to a body that lacks it.
	body := "sebelum\n.\n.titik di awal\nsesudah\n"
	err := s.Send(t.Context(), Message{
		To:      []string{"Budi <budi@example.go.id>"},
		Cc:      []string{"sari@example.go.id"},
		Bcc:     []string{"arsip@example.go.id", "BUDI@example.go.id"},
		Subject: "Laporan",
		Text:    body,
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	got := r.only(t)
	if got.helo != helloName {
		t.Errorf("EHLO %q, want %q", got.helo, helloName)
	}
	if got.from != "noreply@example.go.id" {
		t.Errorf("MAIL FROM <%s>, want the configured sender", got.from)
	}
	want := []string{
		"budi@example.go.id", "sari@example.go.id", "arsip@example.go.id",
	}
	if !slices.Equal(got.rcpts, want) {
		t.Errorf("RCPT TO %v, want %v", got.rcpts, want)
	}
	if bytes.Contains(got.data, []byte("arsip@")) {
		t.Error("the Bcc address reached the message itself")
	}
	if b := text(t, got.data); b != body {
		t.Errorf("body arrived as %q, want %q", b, body)
	}

	// The success line carries the Message-ID the relay received, which
	// is what lets one log be matched against the other.
	var line struct {
		To        []string `json:"to"`
		MessageID string   `json:"message_id"`
		Message   string   `json:"message"`
	}
	if err := json.Unmarshal(logs.Bytes(), &line); err != nil {
		t.Fatalf("log line %q: %v", logs.String(), err)
	}
	id := read(t, got.data).Header.Get("Message-ID")
	if line.Message != "email: message sent" || line.MessageID != id ||
		!slices.Equal(line.To, want) {
		t.Errorf("logged %+v, want the recipients and Message-ID %s",
			line, id)
	}
}

func TestSendLogsInWithPlainOrLogin(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		offered []string
		want    string
	}{
		{[]string{"LOGIN", "PLAIN"}, "PLAIN"},
		{[]string{"LOGIN"}, "LOGIN"},
		{[]string{"login"}, "LOGIN"},
	} {
		t.Run(strings.Join(c.offered, "+"), func(t *testing.T) {
			t.Parallel()
			// A password with spaces at both ends, which nothing on
			// the way may trim.
			r := (&relay{
				mechs: c.offered,
				users: map[string]string{"notif": " rahasia s3cret "},
			}).start(t)
			s := service(t, r, login("notif", " rahasia s3cret "))

			if err := s.SendText(t.Context(), "budi@example.go.id",
				"s", "b"); err != nil {
				t.Fatalf("Send: %v", err)
			}
			got := r.only(t)
			if got.user != "notif" || got.mech != c.want {
				t.Errorf("logged in as %q with %s, want notif with %s",
					got.user, got.mech, c.want)
			}
		})
	}
}

func TestSendReportsARefusedLogin(t *testing.T) {
	t.Parallel()
	r := (&relay{
		mechs: []string{"PLAIN"},
		users: map[string]string{"notif": "benar"},
	}).start(t)
	s := service(t, r, login("notif", "salah"))

	err := s.SendText(t.Context(), "budi@example.go.id", "s", "b")
	var reply *textproto.Error
	if !errors.As(err, &reply) || reply.Code != 535 {
		t.Fatalf("err = %v, want the relay's 535", err)
	}
	if !strings.Contains(err.Error(), "AUTH") {
		t.Errorf("err = %v, want it to name AUTH", err)
	}
	if got := r.only(t); got.from != "" {
		t.Error("MAIL FROM was sent after a refused login")
	}
}

func TestSendRefusesAServerWithNeitherMechanism(t *testing.T) {
	t.Parallel()
	r := (&relay{mechs: []string{"CRAM-MD5", "XOAUTH2"}}).start(t)
	s := service(t, r, login("notif", "rahasia"))

	err := s.SendText(t.Context(), "budi@example.go.id", "s", "b")
	if err == nil || !strings.Contains(err.Error(), "CRAM-MD5 XOAUTH2") {
		t.Fatalf("err = %v, want it to name what the server offers", err)
	}
	if got := r.only(t); got.from != "" {
		t.Error("MAIL FROM was sent without a login")
	}
}

// starttls is strict. The relay here offers AUTH in clear text, which is
// exactly the offer a downgraded connection makes, and nothing is sent in
// answer to it: no credential, no address.
func TestStartTLSIsRequiredWhenConfigured(t *testing.T) {
	t.Parallel()
	r := (&relay{
		mechs: []string{"PLAIN"},
		users: map[string]string{"notif": "rahasia"},
	}).start(t)
	s := service(t, r, func(c *config.EmailConfig) {
		c.TLS = config.EmailTLSStartTLS
		login("notif", "rahasia")(c)
	})

	err := s.SendText(t.Context(), "budi@example.go.id", "s", "b")
	if !errors.Is(err, errNoStartTLS) ||
		!strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("err = %v, want the missing STARTTLS reported", err)
	}
	if got := r.only(t); got.user != "" || got.from != "" {
		t.Errorf("sent without TLS: login %q, MAIL FROM %q",
			got.user, got.from)
	}
}

// One refused recipient fails the whole send before DATA, and the reply
// code survives the wrapping, because 4xx and 5xx call for different
// handling.
func TestSendStopsAtTheFirstRefusedRecipient(t *testing.T) {
	t.Parallel()
	for _, code := range []int{450, 550} {
		r := (&relay{refuse: map[string]string{
			"sari@example.go.id": map[int]string{
				450: "450 4.2.1 mailbox busy",
				550: "550 5.1.1 no such user",
			}[code],
		}}).start(t)
		s := service(t, r, nil)

		err := s.Send(t.Context(), Message{
			To: []string{
				"budi@example.go.id", "sari@example.go.id",
				"eko@example.go.id",
			},
			Text: "isi",
		})
		var reply *textproto.Error
		if !errors.As(err, &reply) || reply.Code != code {
			t.Fatalf("err = %v, want the relay's %d", err, code)
		}
		if !strings.Contains(err.Error(), "RCPT TO <sari@example.go.id>") {
			t.Errorf("err = %v, want it to name the refused recipient",
				err)
		}
		got := r.only(t)
		if !slices.Equal(got.rcpts, []string{"budi@example.go.id"}) {
			t.Errorf("RCPT TO %v, want only the one before the refusal",
				got.rcpts)
		}
		if got.data != nil {
			t.Errorf("%d: the message was uploaded anyway", code)
		}
	}
}

func TestSendChecksTheAnnouncedSizeFirst(t *testing.T) {
	t.Parallel()
	r := (&relay{ext: []string{"SIZE 2000"}}).start(t)
	s := service(t, r, nil)

	big := Message{
		To: []string{"budi@example.go.id"},
		Attachments: []Attachment{{
			Filename: "besar.bin",
			Data:     make([]byte, 5000),
		}},
	}
	if err := s.Send(t.Context(), big); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	if got := r.only(t); got.from != "" {
		t.Error("MAIL FROM was sent for a message over the limit")
	}

	if err := s.SendText(t.Context(), "budi@example.go.id", "kecil",
		"muat"); err != nil {
		t.Errorf("a message under the limit: %v", err)
	}
}

// The relay took the message and then hung up on QUIT. That is a
// delivered message, and reporting it as failed would get it sent twice.
func TestSendIgnoresAnUnansweredQuit(t *testing.T) {
	t.Parallel()
	r := (&relay{dropQuit: true}).start(t)
	s := service(t, r, nil)

	if err := s.SendText(t.Context(), "budi@example.go.id", "s",
		"b"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if r.only(t).data == nil {
		t.Error("the relay has no message")
	}
}

// Nothing to send returns nil without dialing, and does so under a
// context that is already done, since the answer should not depend on
// timing.
func TestNothingToSendNeverDials(t *testing.T) {
	t.Parallel()
	r := (&relay{}).start(t)
	s := service(t, r, nil)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	for name, m := range map[string]Message{
		"no content": {To: []string{"budi@example.go.id"}},
		"blank content": {To: []string{"budi@example.go.id"},
			Subject: " ", Text: "\n"},
		"no recipient": {Text: "isi"},
		"blank recipients": {To: []string{"", "  "}, Cc: []string{""},
			Text: "isi"},
		"a bad address but nothing to say": {To: []string{"budi"}},
	} {
		if err := s.Send(ctx, m); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := s.SendText(ctx, "", "s", "b"); err != nil {
		t.Errorf("SendText to nobody: %v", err)
	}
	if n := len(r.seen()); n != 0 {
		t.Errorf("the relay saw %d connections, want none", n)
	}
}

func TestABadAddressFailsBeforeDialing(t *testing.T) {
	t.Parallel()
	r := (&relay{}).start(t)
	s := service(t, r, nil)

	err := s.Send(t.Context(), Message{
		To:   []string{"budi@example.go.id, sari@example.go.id"},
		Text: "isi",
	})
	if !errors.Is(err, ErrInvalidAddress) {
		t.Fatalf("err = %v, want ErrInvalidAddress", err)
	}
	if n := len(r.seen()); n != 0 {
		t.Errorf("the relay saw %d connections, want none", n)
	}
}

func TestSendUnderACancelledContext(t *testing.T) {
	t.Parallel()
	r := (&relay{}).start(t)
	s := service(t, r, nil)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := s.SendText(ctx, "budi@example.go.id", "s", "b")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestNilServiceIsNotConfigured(t *testing.T) {
	t.Parallel()
	var s *Service
	if err := s.Send(t.Context(), Message{
		To: []string{"budi@example.go.id"}, Text: "isi",
	}); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("Send = %v, want ErrNotConfigured", err)
	}
	if err := s.SendText(t.Context(), "budi@example.go.id", "s",
		"b"); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("SendText = %v, want ErrNotConfigured", err)
	}
}

// Absent and broken are different answers: a deployment without the
// section runs without mail, and one that filled it in halfway is told
// what is missing.
func TestNewTellsAbsentFromBroken(t *testing.T) {
	t.Parallel()
	for name, cfg := range map[string]*config.EmailConfig{
		"nil":  nil,
		"zero": {},
	} {
		if s, err := New(cfg, zerolog.Nop()); s != nil ||
			!errors.Is(err, ErrNotConfigured) {
			t.Errorf("%s: New = %v, %v, want nil, ErrNotConfigured",
				name, s, err)
		}
	}

	_, err := New(&config.EmailConfig{Host: "smtp.example.go.id"},
		zerolog.Nop())
	if err == nil || errors.Is(err, ErrNotConfigured) ||
		!strings.Contains(err.Error(), "notification.email.tls") {
		t.Errorf("a half-filled section: err = %v, want Validate's "+
			"error naming the missing keys", err)
	}

	// And a complete one does not dial.
	r := (&relay{}).start(t)
	service(t, r, nil)
	if n := len(r.seen()); n != 0 {
		t.Errorf("New dialed the relay %d times", n)
	}
}

// Validate refuses credentials in clear text to a relay that is not on
// loopback, and the send path refuses them too. The two keep separate
// lists, so this holds them to the same answer for every spelling that
// could divide them.
func TestClearTextCredentialsAgreeWithValidate(t *testing.T) {
	t.Parallel()
	for _, host := range []string{
		"localhost", "127.0.0.1", "::1",
		"LOCALHOST", "127.0.0.2", "[::1]", "0.0.0.0",
		"smtp.example.go.id",
	} {
		cfg := &config.EmailConfig{
			Host: host, Port: 25, TLS: config.EmailTLSNone,
			Username: "notif", Password: "rahasia",
			From: "noreply@example.go.id", Timeout: time.Minute,
		}
		validates := cfg.Validate() == nil

		a := &auth{username: "notif", password: "rahasia"}
		_, _, err := a.Start(&smtp.ServerInfo{
			Name: host, Auth: []string{"PLAIN"},
		})
		if sends := err == nil; sends != validates {
			t.Errorf("%s: Validate passes = %t, send path logs in = %t",
				host, validates, sends)
		}
	}

	if !loopbackHosts["localhost"] || loopbackHosts["smtp.example.go.id"] {
		t.Error("the loopback list itself is wrong")
	}
}

// LOGIN's prompts are counted rather than read, so a server that words
// them its own way still gets the username first and the password
// second.
func TestLoginAnswersPromptsInOrder(t *testing.T) {
	t.Parallel()
	a := &auth{username: "notif", password: "rahasia"}
	mech, initial, err := a.Start(&smtp.ServerInfo{
		Name: "smtp.example.go.id", TLS: true, Auth: []string{"LOGIN"},
	})
	if err != nil || mech != "LOGIN" || initial != nil {
		t.Fatalf("Start = %q, %q, %v", mech, initial, err)
	}
	for _, c := range []struct{ prompt, want string }{
		{"User Name\x00", "notif"},
		{"Kata sandi:", "rahasia"},
	} {
		got, err := a.Next([]byte(c.prompt), true)
		if err != nil || string(got) != c.want {
			t.Errorf("Next(%q) = %q, %v, want %q",
				c.prompt, got, err, c.want)
		}
	}
	if _, err := a.Next([]byte("lagi?"), true); err == nil {
		t.Error("a third prompt was answered")
	}
	if got, err := a.Next(nil, false); got != nil || err != nil {
		t.Errorf("the closing Next = %q, %v, want nothing", got, err)
	}
}

func TestNamesCountsWhatItDoesNotList(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		n    int
		want string
	}{
		{1, "a0@x"},
		{3, "a0@x, a1@x, a2@x"},
		{5, "a0@x, a1@x, a2@x and 2 more"},
	} {
		var rcpts []string
		for i := range c.n {
			rcpts = append(rcpts, "a"+string(rune('0'+i))+"@x")
		}
		if got := names(rcpts); got != c.want {
			t.Errorf("names(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}

func TestMessageIDIsUniqueUnderTheSendersDomain(t *testing.T) {
	t.Parallel()
	a, b := messageID("noreply@example.go.id"),
		messageID("noreply@example.go.id")
	if a == b {
		t.Error("two Message-IDs are equal")
	}
	if !strings.HasPrefix(a, "<") ||
		!strings.HasSuffix(a, "@example.go.id>") {
		t.Errorf("Message-ID = %s, want <...@example.go.id>", a)
	}
}
