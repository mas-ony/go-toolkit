package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mas-ony/go-toolkit/config"
	"github.com/mas-ony/go-toolkit/internal/upstream"
	"github.com/rs/zerolog"
)

// Service sends notifications through one Telegram bot.
//
// It holds no connection of its own: requests go out through an
// http.Client, which pools them. A Service is that client and settings,
// read-only after New and safe for concurrent use.
//
// It is also safe on a nil receiver, where SendText and SendDocument return
// ErrNotConfigured. That is what lets a caller keep a nil *Service when
// telegram is not among notification.channels and call it unconditionally,
// rather than guard every call site, including one in a detached goroutine
// where a panic would have nobody to catch it.
type Service struct {
	// base is the API root with no trailing slash. Every request goes to
	// base + "/bot" + token + "/" + method, which is the only place the
	// token is used.
	base  string
	token string

	client  *http.Client
	timeout time.Duration
	log     zerolog.Logger
}

// The most the Bot API takes in one message, counted in characters. SendText
// and SendDocument check them before making any request; see ErrTooLong for
// how the count is made.
const (
	MaxTextRunes    = 4096
	MaxCaptionRunes = 1024
)

// defaultFileName is what a document with a blank name is sent as. The
// Bot API needs a name to treat a part of the request as a file at all.
const defaultFileName = "document"

// formType is the content type of a text message's request.
const formType = "application/x-www-form-urlencoded"

// chatPattern is the two forms a chat is named in: a number, negative for a
// group or channel, and @username for a public channel or supergroup. The
// username is 5 to 32 letters, digits or underscores, starting with a
// letter.
//
// It is a shape check and not a lookup. A well-formed chat that does not
// exist is the API's to report, as a 400.
var chatPattern = regexp.MustCompile(
	`^(?:-?[0-9]+|@[A-Za-z][A-Za-z0-9_]{4,31})$`)

// ErrNotConfigured is returned by New when there is no bot to send as, and
// by SendText and SendDocument on a nil *Service.
//
// From New it is fatal wherever notification.channels includes telegram:
// the channel was chosen and its section is missing. From a send it means
// telegram is switched off, and it is a sentinel so a caller can tell that
// apart from a failure to deliver.
var ErrNotConfigured = errors.New("telegram: not configured")

// ErrInvalidChatID is returned by SendText and SendDocument for a chat that
// is neither a number nor @username.
//
// It is a sentinel because the cause is a bad stored value rather than a
// failure to deliver: a caller that retries failed sends should not retry
// this one, and one that reports per-recipient outcomes wants to say which
// of the two happened.
var ErrInvalidChatID = errors.New(
	"telegram: chat is neither a number nor @username")

// ErrTooLong is returned by SendText for a text over MaxTextRunes and by
// SendDocument for a caption over MaxCaptionRunes. Nothing was sent, and
// sending it again fails the same way.
//
// The count is of Unicode characters as Go counts them. Telegram may count
// in UTF-16 units, which makes a text of characters outside the basic
// multilingual plane, emoji for instance, longer to it than to this check.
// Such a text can pass here and be refused by the API, as an *APIError. The
// other way round cannot happen, so this check never turns away a message
// the API would take.
var ErrTooLong = errors.New("telegram: over the Bot API's length limit")

// APIError is the Bot API's refusal of a request: the code and description
// it answered with.
//
// The description is Telegram's own words, such as "Forbidden: bot was
// blocked by the user" or "Bad Request: chat not found", and is the part
// worth logging. Code is the error_code the answer carried, or the HTTP
// status when it carried none.
type APIError struct {
	Code        int
	Description string

	// RetryAfter is how long the API asks to wait before the request is
	// repeated, which it does with a 429 when a bot sends faster than it
	// allows. Zero when the answer asked for nothing.
	RetryAfter time.Duration
}

// Error reports the refusal as Telegram worded it.
func (e *APIError) Error() string {
	msg := fmt.Sprintf("telegram: %d %s", e.Code, e.Description)
	if e.RetryAfter > 0 {
		msg += fmt.Sprintf(" (retry after %s)", e.RetryAfter)
	}
	return msg
}

// answer is the envelope every Bot API method answers in. Only the parts
// this package reads are named.
type answer struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	ErrorCode   int             `json:"error_code"`
	Description string          `json:"description"`
	Parameters  struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
}

// New returns a Service for the bot cfg describes.
//
// It does not dial. A token Telegram has revoked, or a network that cannot
// reach it, fail per send rather than at startup, where a notification
// outage would stop a service whose main job is something else.
//
// ErrNotConfigured means there is nothing to send with: cfg is nil or holds
// no setting at all, as config.TelegramConfig.Configured reports, which is
// what a deployment without notification.telegram.* produces. Call New only
// when notification.channels includes telegram, and treat that error as
// fatal there. Any other problem is Validate's joined error, so a section
// filled in halfway fails loudly instead of quietly switching the channel
// off. upstream.CheckSection draws that line, as it does for email.New.
func New(cfg *config.TelegramConfig, log zerolog.Logger) (*Service, error) {
	if err := upstream.CheckSection(cfg, ErrNotConfigured); err != nil {
		return nil, err
	}

	base := upstream.TrimBase(cfg.APIURL)
	if base == "" {
		base = config.DefaultTelegramAPIURL
	}
	return &Service{
		base:    base,
		token:   strings.TrimSpace(cfg.Token),
		timeout: cfg.Timeout,
		log:     log,
		client: &http.Client{
			// The token is in the URL, so a redirect would carry it to
			// wherever the redirect points. The 3xx is handed back and
			// reported instead; see decode.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

// checkSend resolves the chat a send goes to, chatID trimmed, and refuses
// a send that is present but malformed, before any request is built.
//
// ok is false when there is nothing to send: the chat is empty once
// trimmed, or the payload is, which empty reports. Both are decided before
// anything is checked, so a send with nothing in it is a silent no-op
// whatever the chat or the caption holds. Otherwise the chat has to be a
// number or an @username, and value, the text or the caption as field
// names it, has to fit the Bot API's limit for it. SendText and
// SendDocument differ in what an empty send is and agree on the rest.
func checkSend(
	chatID string,
	empty bool,
	field, value string,
	limit int,
) (chat string, ok bool, err error) {
	chat = strings.TrimSpace(chatID)
	if chat == "" || empty {
		return "", false, nil
	}
	if !chatPattern.MatchString(chat) {
		return "", false, fmt.Errorf("%w: %q", ErrInvalidChatID, chatID)
	}
	if n := utf8.RuneCountInString(value); n > limit {
		return "", false, fmt.Errorf("%w: the %s is %d characters "+
			"against a limit of %d", ErrTooLong, field, n, limit)
	}
	return chat, true, nil
}

// scrub returns err unchanged unless its text carries the token. That can
// happen only if something between here and the API put the request URL
// into an answer or an error, and then the token is masked out of the
// text with upstream.Masked, the mark every other credential is replaced
// with. The chain under it is dropped, because nothing in it can be
// trusted to be clean either.
func (s *Service) scrub(err error) error {
	msg := err.Error()
	if !strings.Contains(msg, s.token) {
		return err
	}
	return errors.New(strings.ReplaceAll(msg, s.token, upstream.Masked))
}

// decode turns one answer into the message id it carries or the refusal it
// is.
//
// Anything that is not a 2xx is a refusal, whatever it says. A body that is
// not JSON on a non-2xx, a proxy's error page for instance, is reported by
// its status alone and never echoed: it is text from a server this package
// has no reason to trust with the log, and it may repeat the request URL.
// A 3xx is a redirect that was not followed, reported the same way, without
// its Location.
func decode(status int, raw []byte) (int64, error) {
	if status >= 300 && status < 400 {
		return 0, &APIError{
			Code:        status,
			Description: "redirect not followed: the token is in the URL",
		}
	}

	var a answer
	if err := json.Unmarshal(raw, &a); err != nil {
		if !upstream.IsSuccess(status) {
			return 0, &APIError{
				Code:        status,
				Description: http.StatusText(status),
			}
		}
		return 0, fmt.Errorf("unexpected answer (status %d): "+
			"the body is not JSON", status)
	}

	switch {
	case !a.OK:
		code := a.ErrorCode
		if code == 0 {
			code = status
		}
		desc := a.Description
		if desc == "" {
			desc = "the answer carries no description"
		}
		return 0, &APIError{
			Code:        code,
			Description: desc,
			RetryAfter: time.Duration(a.Parameters.RetryAfter) *
				time.Second,
		}
	case !upstream.IsSuccess(status):
		return 0, &APIError{Code: status, Description: http.StatusText(status)}
	}

	// A message that was sent is sent whether or not its id can be read,
	// so a result of another shape is not a failure.
	var sent struct {
		MessageID int64 `json:"message_id"`
	}
	_ = json.Unmarshal(a.Result, &sent)
	return sent.MessageID, nil
}

// roundTrip makes one request and returns the id of the message it sent.
//
// The timeout is applied to the context, not to the client, so an expiry
// surfaces as context.DeadlineExceeded and not as a timeout error that
// mentions no context.
func (s *Service) roundTrip(
	ctx context.Context,
	method, contentType string,
	body []byte,
) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.base+"/bot"+s.token+"/"+method, bytes.NewReader(body))
	if err != nil {
		return 0, upstream.StepErr(ctx, "request", err)
	}
	req.Header.Set("Content-Type", contentType)

	resp, err := s.client.Do(req)
	if err != nil {
		return 0, upstream.StepErr(ctx, "request", err)
	}
	defer resp.Body.Close()

	// No more of the answer is read than upstream.MaxAnswerBytes. A real
	// one is a few hundred bytes, so a server that sends more is not the
	// Bot API.
	raw, err := io.ReadAll(io.LimitReader(resp.Body,
		upstream.MaxAnswerBytes))
	if err != nil {
		return 0, upstream.StepErr(ctx, "answer", err)
	}
	return decode(resp.StatusCode, raw)
}

// post is roundTrip with the token masked out of whatever it returns, the
// one place an error leaves the request path.
func (s *Service) post(
	ctx context.Context,
	method, contentType string,
	body []byte,
) (int64, error) {
	id, err := s.roundTrip(ctx, method, contentType, body)
	if err != nil {
		return 0, s.scrub(err)
	}
	return id, nil
}

// compose builds the multipart body sendDocument takes: the chat, the
// caption when there is one, and the file under the field "document".
func compose(
	chat, caption, name string,
	data []byte,
) (contentType string, body []byte, err error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if err := w.WriteField("chat_id", chat); err != nil {
		return "", nil, err
	}
	if caption != "" {
		if err := w.WriteField("caption", caption); err != nil {
			return "", nil, err
		}
	}
	part, err := w.CreateFormFile("document", name)
	if err != nil {
		return "", nil, err
	}
	if _, err := part.Write(data); err != nil {
		return "", nil, err
	}
	if err := w.Close(); err != nil {
		return "", nil, err
	}
	return w.FormDataContentType(), buf.Bytes(), nil
}

// SendText sends a plain-text message to one chat.
//
// Nothing to send is not an error. A chat that is empty once trimmed, or
// text that is blank, returns nil without a request. Both are checked
// before ctx is, so the answer does not depend on timing. A chat that is
// present but malformed is different, because silence would lose the only
// sign that the stored value needs fixing: it returns ErrInvalidChatID,
// and a text over MaxTextRunes returns ErrTooLong, both also without a
// request.
//
// notification.telegram.timeout and ctx both bound the send, and whichever
// ends first ends it; the error then unwraps to context.DeadlineExceeded
// or context.Canceled. A refusal from the API is an *APIError.
//
// A returned error names the chat, which puts an id wherever a caller logs
// it. That is deliberate: without it the line cannot say which notification
// failed. The success line logged here names it too.
//
// Safe for concurrent use, and on a nil receiver; see Service.
func (s *Service) SendText(ctx context.Context, chatID, text string) error {
	if s == nil {
		return ErrNotConfigured
	}
	chat, ok, err := checkSend(chatID, strings.TrimSpace(text) == "",
		"text", text, MaxTextRunes)
	if err != nil || !ok {
		return err
	}

	form := url.Values{"chat_id": {chat}, "text": {text}}
	id, err := s.post(ctx, "sendMessage", formType, []byte(form.Encode()))
	if err != nil {
		return fmt.Errorf("telegram: send to %s: %w", chat, err)
	}

	s.log.Info().
		Str("to", chat).
		Int64("message_id", id).
		Msg("telegram: message sent")
	return nil
}

// SendDocument sends a file to one chat, with an optional caption shown
// beneath it.
//
// data is the raw file. filename is shown to the recipient as the
// document's name and should carry its extension ("report.pdf"), which is
// also how Telegram decides how to show it. A blank one is sent as
// "document" rather than as a nameless part, which the API would not take
// for a file.
//
// The size of data is not checked. The hosted API takes up to 50 MB and a
// Bot API server of your own takes more, so the refusal is the API's to
// make, as an *APIError. The upload is a network round trip proportional to
// the size, which makes this the slowest thing here; pass a context that
// bounds it, and size notification.telegram.timeout for it.
//
// The empty cases are SendText's. A chat that is empty once trimmed, or no
// data, returns nil without a request. A caption over MaxCaptionRunes
// returns ErrTooLong. Filename and caption are otherwise unchecked: a
// notification that arrives badly labelled beats one that does not arrive.
//
// Everything else is as described on SendText.
func (s *Service) SendDocument(
	ctx context.Context,
	chatID string,
	data []byte,
	filename, caption string,
) error {
	if s == nil {
		return ErrNotConfigured
	}
	chat, ok, err := checkSend(chatID, len(data) == 0,
		"caption", caption, MaxCaptionRunes)
	if err != nil || !ok {
		return err
	}

	name := strings.TrimSpace(filename)
	if name == "" {
		name = defaultFileName
	}
	contentType, body, err := compose(chat, caption, name, data)
	if err != nil {
		return fmt.Errorf("telegram: compose: %w", err)
	}
	id, err := s.post(ctx, "sendDocument", contentType, body)
	if err != nil {
		return fmt.Errorf("telegram: send document to %s: %w", chat, err)
	}

	s.log.Info().
		Str("to", chat).
		Str("file", name).
		Int64("message_id", id).
		Msg("telegram: document sent")
	return nil
}
