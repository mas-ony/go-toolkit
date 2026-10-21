package telegram

// Tests for telegram.go.
//
// Every send here goes to bot, an HTTP server on loopback that answers the
// way the Bot API does and records what each request told it. The
// assertions are about that record, what the API was sent and what it was
// never sent, rather than about what the client believes it did.
//
// Nothing here needs a certificate or waits on a clock. The TLS path, the
// timeout and concurrent sends are in telegram_integration_test.go.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mas-ony/go-toolkit/config"
	"github.com/mas-ony/go-toolkit/internal/testutil"
	"github.com/mas-ony/go-toolkit/internal/upstream"
	"github.com/rs/zerolog"
)

// upload is the file part of a document request.
type upload struct {
	field, name, contentType string
	data                     []byte
}

// hit is what one request told the API.
type hit struct {
	method      string
	path        string
	contentType string

	// secure is whether the request arrived over TLS.
	secure bool

	// form holds a text message's fields.
	form url.Values

	// fields and file hold a document's text fields and its file part.
	fields map[string]string
	file   *upload
}

// bot is a stand-in for the Bot API on loopback. Its fields set its
// behaviour and are read-only once start has been called.
type bot struct {
	// reply decides the answer. Left nil, every request is accepted as
	// message 42.
	reply func(w http.ResponseWriter, r *http.Request)

	// tls serves over TLS with the certificate httptest makes, which a
	// client has to be told to trust.
	tls bool

	srv  *httptest.Server
	mu   sync.Mutex
	hits []hit
}

// accepted is the answer to a message that was sent.
const accepted = `{"ok":true,"result":{"message_id":42}}`

// start begins serving and stops at the end of the test that started it.
func (b *bot) start(t *testing.T) *bot {
	t.Helper()
	h := http.HandlerFunc(b.serve)
	if b.tls {
		b.srv = httptest.NewTLSServer(h)
	} else {
		b.srv = httptest.NewServer(h)
	}
	t.Cleanup(b.srv.Close)
	return b
}

// serve records the request and answers it.
func (b *bot) serve(w http.ResponseWriter, r *http.Request) {
	h := hit{
		method:      r.Method,
		path:        r.URL.Path,
		contentType: r.Header.Get("Content-Type"),
		secure:      r.TLS != nil,
	}
	kind, params, _ := mime.ParseMediaType(h.contentType)
	switch kind {
	case formType:
		_ = r.ParseForm()
		h.form = r.PostForm
	case "multipart/form-data":
		h.fields = map[string]string{}
		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			p, err := mr.NextPart()
			if err != nil {
				break
			}
			data, _ := io.ReadAll(p)
			if p.FileName() == "" {
				h.fields[p.FormName()] = string(data)
				continue
			}
			h.file = &upload{
				field:       p.FormName(),
				name:        p.FileName(),
				contentType: p.Header.Get("Content-Type"),
				data:        data,
			}
		}
	}

	b.mu.Lock()
	b.hits = append(b.hits, h)
	b.mu.Unlock()

	if b.reply != nil {
		b.reply(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, accepted)
}

// seen returns every request so far, in arrival order.
func (b *bot) seen() []hit {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]hit(nil), b.hits...)
}

// only returns the one request the API is expected to have seen.
func (b *bot) only(t *testing.T) hit {
	t.Helper()
	return testutil.Only(t, "requests at the API", b.seen())
}

// respond returns a reply that answers every request with status and body.
func respond(
	status int,
	body string,
) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

// service builds a Service for b through New, the way an application does,
// after change has adjusted the configuration.
func service(
	t *testing.T,
	b *bot,
	change func(*config.TelegramConfig),
) *Service {
	t.Helper()
	cfg := &config.TelegramConfig{
		Token:   testutil.BotToken,
		APIURL:  b.srv.URL,
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

// A send delivers one message the API reads back intact, sets nothing else
// on it, and logs its success line.
func TestSendTextPostsOneMessage(t *testing.T) {
	t.Parallel()
	b := (&bot{}).start(t)
	s := service(t, b, nil)
	var logs bytes.Buffer
	s.log = zerolog.New(&logs)

	const text = "Laporan realisasi siap.\nRp 1.250.000 & 50% <tercapai>_"
	if err := s.SendText(t.Context(), "123456789", text); err != nil {
		t.Fatalf("SendText: %v", err)
	}

	h := b.only(t)
	if h.method != http.MethodPost {
		t.Errorf("method = %s, want POST", h.method)
	}
	if want := "/bot" + testutil.BotToken + "/sendMessage"; h.path != want {
		t.Errorf("path = %q, want %q", h.path, want)
	}
	if h.contentType != formType {
		t.Errorf("content type = %q, want %q", h.contentType, formType)
	}
	if got := h.form.Get("chat_id"); got != "123456789" {
		t.Errorf("chat_id = %q, want 123456789", got)
	}
	if got := h.form.Get("text"); got != text {
		t.Errorf("text = %q, want %q", got, text)
	}
	// Plain text means no parse mode, or anything else, is set.
	if len(h.form) != 2 {
		t.Errorf("form = %v, want chat_id and text only", h.form)
	}

	log := logs.String()
	for _, want := range []string{
		`"message":"telegram: message sent"`,
		`"to":"123456789"`,
		`"message_id":42`,
	} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %s:\n%s", want, log)
		}
	}
	if strings.Contains(log, testutil.BotToken) {
		t.Errorf("the log carries the token:\n%s", log)
	}
}

// A chat is accepted as a number, as a negative one, as a channel's
// username, and with the padding a stored value tends to carry.
func TestChatsAreAcceptedInEveryForm(t *testing.T) {
	t.Parallel()
	b := (&bot{}).start(t)
	s := service(t, b, nil)

	cases := []struct{ in, want string }{
		{"123456789", "123456789"},
		{"-987654321", "-987654321"},
		{"-1001234567890", "-1001234567890"},
		{"@laporan_channel", "@laporan_channel"},
		{"  555\n", "555"},
	}
	for _, c := range cases {
		if err := s.SendText(t.Context(), c.in, "x"); err != nil {
			t.Errorf("SendText(%q): %v", c.in, err)
		}
	}

	got := b.seen()
	if len(got) != len(cases) {
		t.Fatalf("the API saw %d requests, want %d", len(got), len(cases))
	}
	for i, c := range cases {
		if chat := got[i].form.Get("chat_id"); chat != c.want {
			t.Errorf("%q was sent as %q, want %q", c.in, chat, c.want)
		}
	}
}

// A chat that is no number and no username is ErrInvalidChatID, returned
// without a request. A phone number is one of them, since it is the value
// most likely to be stored in the wrong column.
func TestABadChatFailsBeforeRequesting(t *testing.T) {
	t.Parallel()
	b := (&bot{}).start(t)
	s := service(t, b, nil)

	for _, chat := range []string{
		"budi", "12a", "12 34", "--5", "-", "12.5", "0x10",
		"+628123456789", "@", "@abc", "@9abcde", "@has-dash", "@has space",
	} {
		err := s.SendText(t.Context(), chat, "x")
		if !errors.Is(err, ErrInvalidChatID) {
			t.Errorf("SendText(%q) = %v, want ErrInvalidChatID", chat, err)
		}
		err = s.SendDocument(t.Context(), chat, []byte("x"), "a.pdf", "")
		if !errors.Is(err, ErrInvalidChatID) {
			t.Errorf("SendDocument(%q) = %v, want ErrInvalidChatID",
				chat, err)
		}
	}
	if n := len(b.seen()); n != 0 {
		t.Errorf("the API saw %d requests, want none", n)
	}
}

// Nothing to send returns nil without a request, and does so under a
// context that is already done, since the answer should not depend on
// timing.
func TestNothingToSendNeverCallsTheAPI(t *testing.T) {
	t.Parallel()
	b := (&bot{}).start(t)
	s := service(t, b, nil)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	for name, err := range map[string]error{
		"no chat":      s.SendText(ctx, "", "x"),
		"a blank chat": s.SendText(ctx, " \n", "x"),
		"no text":      s.SendText(ctx, "123", ""),
		"blank text":   s.SendText(ctx, "123", " \n\t"),
		"a document to no chat": s.SendDocument(
			ctx, "", []byte("x"), "a.pdf", "c"),
		"a document to a blank chat": s.SendDocument(
			ctx, "  ", []byte("x"), "a.pdf", "c"),
		"a document with no data": s.SendDocument(
			ctx, "123", nil, "a.pdf", "c"),
		"a document with empty data": s.SendDocument(
			ctx, "123", []byte{}, "a.pdf", "c"),
	} {
		if err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if n := len(b.seen()); n != 0 {
		t.Errorf("the API saw %d requests, want none", n)
	}
}

// checkSend is the one place SendText and SendDocument decide a send, and
// it decides nothing to send before anything else: an empty chat or
// payload is a silent nil whatever the chat or the caption holds. A send
// that is not empty gets its chat back trimmed, or ErrInvalidChatID, or
// ErrTooLong.
func TestNothingToSendIsDecidedFirst(t *testing.T) {
	t.Parallel()
	over := strings.Repeat("x", MaxCaptionRunes+1)

	tests := []struct {
		name   string
		chatID string
		empty  bool
		value  string
		chat   string
		ok     bool
		err    error
	}{
		{"a chat to trim", " 555\n", false, "x", "555", true, nil},
		{"no chat", "", false, "x", "", false, nil},
		{"a blank chat", " \n", false, "x", "", false, nil},
		{"nothing to send", "555", true, "", "", false, nil},
		{"nothing to send to a malformed chat", "budi", true, "", "",
			false, nil},
		{"nothing to send under a caption over the limit", "555", true,
			over, "", false, nil},
		{"a malformed chat", "budi", false, "x", "", false,
			ErrInvalidChatID},
		{"a caption over the limit", "555", false, over, "", false,
			ErrTooLong},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			chat, ok, err := checkSend(tc.chatID, tc.empty, "caption",
				tc.value, MaxCaptionRunes)
			if chat != tc.chat || ok != tc.ok || !errors.Is(err, tc.err) {
				t.Errorf("checkSend(%q, %t) = %q, %t, %v, want %q, %t, %v",
					tc.chatID, tc.empty, chat, ok, err, tc.chat, tc.ok,
					tc.err)
			}
		})
	}
}

// The limit is counted in characters and not bytes, so a text in a script
// that takes two bytes a letter is not turned away at half the length. One
// over is ErrTooLong, reported with both counts, and never sent.
func TestTextIsCountedInCharacters(t *testing.T) {
	t.Parallel()
	b := (&bot{}).start(t)
	s := service(t, b, nil)

	atLimit := strings.Repeat("\u00e9", MaxTextRunes)
	if err := s.SendText(t.Context(), "1", atLimit); err != nil {
		t.Fatalf("a text at the limit: %v", err)
	}

	err := s.SendText(t.Context(), "1", atLimit+"\u00e9")
	if !errors.Is(err, ErrTooLong) {
		t.Fatalf("a text over the limit: err = %v, want ErrTooLong", err)
	}
	for _, want := range []string{"4097", "4096"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to report %s", err, want)
		}
	}
	if n := len(b.seen()); n != 1 {
		t.Errorf("the API saw %d requests, want only the one at the limit",
			n)
	}
}

// A document is uploaded as one multipart request whose file arrives byte
// for byte, under the name it was given, beside the chat and the caption.
func TestSendDocumentUploadsOneFile(t *testing.T) {
	t.Parallel()
	b := (&bot{}).start(t)
	s := service(t, b, nil)
	var logs bytes.Buffer
	s.log = zerolog.New(&logs)
	pdf := []byte("%PDF-1.7\r\n\x00\xff\xfe binary \r\n\x00")

	err := s.SendDocument(t.Context(), "-1001234567890", pdf,
		"realisasi.pdf", "Laporan Triwulan III")
	if err != nil {
		t.Fatalf("SendDocument: %v", err)
	}

	h := b.only(t)
	if want := "/bot" + testutil.BotToken + "/sendDocument"; h.path != want {
		t.Errorf("path = %q, want %q", h.path, want)
	}
	if !strings.HasPrefix(h.contentType, "multipart/form-data") {
		t.Errorf("content type = %q, want multipart/form-data",
			h.contentType)
	}
	if h.fields["chat_id"] != "-1001234567890" ||
		h.fields["caption"] != "Laporan Triwulan III" ||
		len(h.fields) != 2 {
		t.Errorf("fields = %v, want chat_id and caption only", h.fields)
	}
	if h.file == nil {
		t.Fatal("the request carried no file")
	}
	if h.file.field != "document" || h.file.name != "realisasi.pdf" {
		t.Errorf("file = %q named %q, want document named realisasi.pdf",
			h.file.field, h.file.name)
	}
	if !bytes.Equal(h.file.data, pdf) {
		t.Errorf("the file arrived as %q, want %q", h.file.data, pdf)
	}

	log := logs.String()
	for _, want := range []string{
		`"message":"telegram: document sent"`,
		`"to":"-1001234567890"`,
		`"file":"realisasi.pdf"`,
		`"message_id":42`,
	} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %s:\n%s", want, log)
		}
	}
}

// No caption means no caption field, and a blank name is replaced by one,
// because a part without a filename is a text field to the API and not a
// file. A name loses its padding and keeps everything else, quotes and
// accents included.
func TestDocumentNamesAndCaptions(t *testing.T) {
	t.Parallel()
	b := (&bot{}).start(t)
	s := service(t, b, nil)

	cases := []struct{ in, want string }{
		{"", "document"},
		{" \t", "document"},
		{"  laporan.pdf ", "laporan.pdf"},
		{"Laporan Triwulan III.pdf", "Laporan Triwulan III.pdf"},
		{`a"b.pdf`, `a"b.pdf`},
		{"rekap-m\u00e8i.pdf", "rekap-m\u00e8i.pdf"},
	}
	for _, c := range cases {
		err := s.SendDocument(t.Context(), "1", []byte("x"), c.in, "")
		if err != nil {
			t.Errorf("SendDocument(%q): %v", c.in, err)
		}
	}

	got := b.seen()
	if len(got) != len(cases) {
		t.Fatalf("the API saw %d requests, want %d", len(got), len(cases))
	}
	for i, c := range cases {
		if got[i].file == nil || got[i].file.name != c.want {
			t.Errorf("%q was sent as %+v, want the name %q",
				c.in, got[i].file, c.want)
		}
		if _, has := got[i].fields["caption"]; has {
			t.Errorf("%q: a caption field was sent for an empty caption",
				c.in)
		}
	}
}

// The caption has a limit of its own, checked before anything is uploaded.
func TestCaptionIsCountedInCharacters(t *testing.T) {
	t.Parallel()
	b := (&bot{}).start(t)
	s := service(t, b, nil)

	atLimit := strings.Repeat("\u00e9", MaxCaptionRunes)
	err := s.SendDocument(t.Context(), "1", []byte("x"), "a.pdf", atLimit)
	if err != nil {
		t.Fatalf("a caption at the limit: %v", err)
	}

	err = s.SendDocument(t.Context(), "1", []byte("x"), "a.pdf",
		atLimit+"\u00e9")
	if !errors.Is(err, ErrTooLong) {
		t.Fatalf("a caption over the limit: err = %v, want ErrTooLong", err)
	}
	if n := len(b.seen()); n != 1 {
		t.Errorf("the API saw %d requests, want only the one at the limit",
			n)
	}
}

// A refusal comes back as an *APIError carrying Telegram's own words, and
// the error that reaches the caller names the chat it was for.
func TestARefusalCarriesTelegramsWords(t *testing.T) {
	t.Parallel()
	b := (&bot{reply: respond(http.StatusForbidden, `{"ok":false,`+
		`"error_code":403,"description":"Forbidden: bot was blocked by `+
		`the user"}`)}).start(t)
	s := service(t, b, nil)

	err := s.SendText(t.Context(), "123456789", "x")
	var api *APIError
	if !errors.As(err, &api) {
		t.Fatalf("err = %v, want an *APIError", err)
	}
	if api.Code != 403 ||
		api.Description != "Forbidden: bot was blocked by the user" ||
		api.RetryAfter != 0 {
		t.Errorf("APIError = %+v, want Telegram's 403 and nothing else",
			*api)
	}
	if !strings.Contains(err.Error(), "send to 123456789") {
		t.Errorf("err = %v, want it to name the chat", err)
	}
	if errors.Is(err, ErrNotConfigured) {
		t.Error("a refusal reads as ErrNotConfigured")
	}
}

// Flood control asks the caller to wait, and says for how long.
func TestFloodControlCarriesRetryAfter(t *testing.T) {
	t.Parallel()
	b := (&bot{reply: respond(http.StatusTooManyRequests, `{"ok":false,`+
		`"error_code":429,"description":"Too Many Requests: retry after `+
		`7","parameters":{"retry_after":7}}`)}).start(t)
	s := service(t, b, nil)

	err := s.SendText(t.Context(), "123456789", "x")
	var api *APIError
	if !errors.As(err, &api) || api.Code != 429 {
		t.Fatalf("err = %v, want an *APIError with code 429", err)
	}
	if api.RetryAfter != 7*time.Second {
		t.Errorf("RetryAfter = %s, want 7s", api.RetryAfter)
	}
	if !strings.Contains(err.Error(), "retry after 7s") {
		t.Errorf("err = %v, want it to say to retry after 7s", err)
	}
}

// Every shape an answer can take other than a success. What is refused is
// the status, or the code and description the answer gave, and a body that
// is not the API's is never repeated.
func TestAnswersThatAreNotASuccess(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		status int
		body   string
		code   int
		desc   string
	}{
		{"a refusal", 400, `{"ok":false,"error_code":400,` +
			`"description":"Bad Request: chat not found"}`,
			400, "Bad Request: chat not found"},
		{"a refusal under status 200", 200, `{"ok":false,` +
			`"error_code":400,"description":"Bad Request: x"}`,
			400, "Bad Request: x"},
		{"a refusal with no error code", 401,
			`{"ok":false,"description":"Unauthorized"}`,
			401, "Unauthorized"},
		{"a refusal with no description", 400,
			`{"ok":false,"error_code":400}`,
			400, "the answer carries no description"},
		{"an error page", 502,
			"<html><body>502 secret page text</body></html>",
			502, "Bad Gateway"},
		{"an empty body", 503, "", 503, "Service Unavailable"},
		{"success under a failing status", 500, accepted,
			500, "Internal Server Error"},
	} {
		b := (&bot{reply: respond(c.status, c.body)}).start(t)
		err := service(t, b, nil).SendText(t.Context(), "1", "x")

		var api *APIError
		if !errors.As(err, &api) {
			t.Errorf("%s: err = %v, want an *APIError", c.name, err)
			continue
		}
		if api.Code != c.code || api.Description != c.desc {
			t.Errorf("%s: APIError = %d %q, want %d %q",
				c.name, api.Code, api.Description, c.code, c.desc)
		}
		if strings.Contains(err.Error(), "secret page text") {
			t.Errorf("%s: the body was repeated:\n%v", c.name, err)
		}
	}
}

// A success that is not JSON is an error that is not an *APIError, since
// the API said nothing. A success with no message in it is still a send.
func TestSuccessesThatAreNotWhatTheAPISends(t *testing.T) {
	t.Parallel()
	b := (&bot{reply: respond(200, "<html>ok</html>")}).start(t)
	err := service(t, b, nil).SendText(t.Context(), "1", "x")
	var api *APIError
	if err == nil || errors.As(err, &api) ||
		!strings.Contains(err.Error(), "not JSON") {
		t.Errorf("err = %v, want an error saying the body is not JSON", err)
	}

	for _, body := range []string{`{"ok":true}`, `{"ok":true,"result":true}`} {
		b := (&bot{reply: respond(200, body)}).start(t)
		if err := service(t, b, nil).SendText(t.Context(), "1", "x"); err !=
			nil {
			t.Errorf("%s: %v", body, err)
		}
	}
}

// A redirect is not followed, because the request URL is the token. The
// place it pointed to sees nothing, and what comes back is a refusal that
// repeats neither the token nor where it pointed.
func TestARedirectIsNotFollowed(t *testing.T) {
	t.Parallel()
	elsewhere := (&bot{}).start(t)
	b := (&bot{reply: func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.srv.URL+r.URL.Path,
			http.StatusTemporaryRedirect)
	}}).start(t)

	err := service(t, b, nil).SendText(t.Context(), "123456789", "x")
	var api *APIError
	if !errors.As(err, &api) || api.Code != http.StatusTemporaryRedirect {
		t.Fatalf("err = %v, want an *APIError with code 307", err)
	}
	if n := len(elsewhere.seen()); n != 0 {
		t.Errorf("the redirect target saw %d requests, want none", n)
	}
	for _, leak := range []string{testutil.BotToken, elsewhere.srv.URL} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("err = %v, want it without %q", err, leak)
		}
	}
}

// No error carries the token, whichever way it arose: a connection that
// fails, where net/http would quote the whole URL, and an API that echoes
// the path back in its own description. The first leaves no *url.Error in
// the chain for a caller to print the URL from.
func TestTheTokenNeverReachesAnError(t *testing.T) {
	t.Parallel()
	down := (&bot{}).start(t)
	s := service(t, down, nil)
	down.srv.Close()

	err := s.SendText(t.Context(), "123456789", "x")
	if err == nil {
		t.Fatal("a send to a closed server succeeded")
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		t.Errorf("the chain holds a *url.Error, which carries the URL:\n%v",
			ue)
	}
	for _, leak := range []string{testutil.BotToken, "/bot", "AAFake"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("a refused connection: err = %v, want it without %q",
				err, leak)
		}
	}

	echo := (&bot{reply: func(w http.ResponseWriter, r *http.Request) {
		respond(400, `{"ok":false,"error_code":400,"description":`+
			`"Bad Request: no route for `+r.URL.Path+`"}`)(w, r)
	}}).start(t)
	err = service(t, echo, nil).SendText(t.Context(), "123456789", "x")
	if err == nil {
		t.Fatal("a refused send succeeded")
	}
	if strings.Contains(err.Error(), testutil.BotToken) ||
		!strings.Contains(err.Error(), upstream.Masked) {
		t.Errorf("an echoing API: err = %v, want the token masked", err)
	}
}

// A context cancelled before the send ends it with context.Canceled, and
// nothing reaches the API.
func TestSendUnderACancelledContext(t *testing.T) {
	t.Parallel()
	b := (&bot{}).start(t)
	s := service(t, b, nil)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := s.SendText(ctx, "123456789", "x")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if n := len(b.seen()); n != 0 {
		t.Errorf("the API saw %d requests, want none", n)
	}
	if strings.Contains(err.Error(), testutil.BotToken) {
		t.Errorf("err = %v, want it without the token", err)
	}
}

// A nil *Service answers every send with ErrNotConfigured instead of
// panicking.
func TestNilServiceIsNotConfigured(t *testing.T) {
	t.Parallel()
	var s *Service
	if err := s.SendText(t.Context(), "123", "x"); !errors.Is(err,
		ErrNotConfigured) {
		t.Errorf("SendText = %v, want ErrNotConfigured", err)
	}
	if err := s.SendDocument(t.Context(), "123", []byte("x"), "a.pdf",
		""); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("SendDocument = %v, want ErrNotConfigured", err)
	}
}

// Absent and broken are different answers: a deployment without the
// section runs without the channel, and one that filled it in halfway is
// told what is missing, without the token quoted back.
func TestNewTellsAbsentFromBroken(t *testing.T) {
	t.Parallel()
	for name, cfg := range map[string]*config.TelegramConfig{
		"nil":  nil,
		"zero": {},
	} {
		if s, err := New(cfg, zerolog.Nop()); s != nil ||
			!errors.Is(err, ErrNotConfigured) {
			t.Errorf("%s: New = %v, %v, want nil, ErrNotConfigured",
				name, s, err)
		}
	}

	_, err := New(&config.TelegramConfig{Token: testutil.BotToken},
		zerolog.Nop())
	if err == nil || errors.Is(err, ErrNotConfigured) ||
		!strings.Contains(err.Error(), "notification.telegram.timeout") {
		t.Errorf("a half-filled section: err = %v, want Validate's error "+
			"naming the missing keys", err)
	}
	if err != nil && strings.Contains(err.Error(), testutil.BotToken) {
		t.Errorf("err = %v, want it without the token", err)
	}

	// And a complete one does not dial.
	b := (&bot{}).start(t)
	service(t, b, nil)
	if n := len(b.seen()); n != 0 {
		t.Errorf("New made %d requests", n)
	}
}

// An empty API URL is the hosted API, and a stated one keeps its path
// prefix and loses its trailing slashes, so the method is appended to the
// same address either way.
func TestTheAPIURLForms(t *testing.T) {
	t.Parallel()
	s, err := New(&config.TelegramConfig{
		Token: testutil.BotToken, Timeout: time.Minute,
	}, zerolog.Nop())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if s.base != config.DefaultTelegramAPIURL {
		t.Errorf("base = %q, want %q", s.base, config.DefaultTelegramAPIURL)
	}

	b := (&bot{}).start(t)
	s = service(t, b, func(c *config.TelegramConfig) {
		c.APIURL = " " + b.srv.URL + "/tg// "
	})
	if err := s.SendText(t.Context(), "1", "x"); err != nil {
		t.Fatalf("SendText: %v", err)
	}
	want := "/tg/bot" + testutil.BotToken + "/sendMessage"
	if got := b.only(t).path; got != want {
		t.Errorf("path = %q, want %q", got, want)
	}
}

// The refusal reads as Telegram worded it, with the wait when there is one.
func TestAPIErrorText(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		err  *APIError
		want string
	}{
		{&APIError{Code: 400, Description: "Bad Request: chat not found"},
			"telegram: 400 Bad Request: chat not found"},
		{&APIError{Code: 429, Description: "Too Many Requests",
			RetryAfter: 3 * time.Second},
			"telegram: 429 Too Many Requests (retry after 3s)"},
	} {
		if got := c.err.Error(); got != c.want {
			t.Errorf("Error() = %q, want %q", got, c.want)
		}
	}
}
