package upstream

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Optional is a configuration section a deployment may leave out, as it
// may notification.email.* or notification.telegram.*: Configured reports
// whether any source supplied a setting for it, and Validate what makes a
// supplied one unusable.
type Optional interface {
	Configured() bool
	Validate() error
}

// Masked is what a credential is replaced with wherever one is kept out of
// text: the userinfo RedactUserinfo masks, each fragment ParseCause masks,
// and the bot token the telegram package masks out of an error. One
// spelling for all of them, so a log reader learns one mark and a search
// for it finds every place something was withheld.
const Masked = "***"

// MaxAnswerBytes is the most of another server's answer the toolkit keeps
// where it reads the answer for what it says rather than for what it
// carries: the body of a refusal, which httpclient quotes on its Error,
// and every answer from the Bot API, which telegram decodes. Both cap what
// they keep by it, so the two draw that line in the same place.
//
// A mebibyte is far more than an envelope, a proxy's error page or a Bot
// API answer needs, so a server that sends more is not sending one of
// those, and the cap is what keeps it from deciding how much memory a
// caller spends finding that out. How each package applies it is said
// where it does.
const MaxAnswerBytes = 1 << 20

// quotedFragment matches one double-quoted Go string literal, escaped
// characters included. That is the form strconv.Quote and the %q verb
// produce, and the form in which every url.Parse cause that repeats part
// of its input repeats it.
var quotedFragment = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)

// loopbackHosts are the host spellings a credential may cross to in clear
// text: the three net/smtp's PlainAuth accepts.
//
// Matched EXACTLY, as net/smtp matches them. "LOCALHOST" and "127.0.0.2"
// both reach the loopback interface, yet a credential in clear text to
// either is refused, because the rule is only worth having if every caller
// draws the line in the same place, and net/smtp draws it here.
var loopbackHosts = map[string]bool{
	"localhost": true,
	"127.0.0.1": true,
	"::1":       true,
}

// withoutURL returns err without the *url.Error that net/url and net/http
// wrap a failure in. That wrapper carries the whole URL, and the URL can
// carry a credential: userinfo, or a bot token in the path. What it wraps
// is returned as it is, so errors.Is still reaches a context error or a
// timeout underneath.
func withoutURL(err error) error {
	if ue, ok := errors.AsType[*url.Error](err); ok {
		return ue.Err
	}
	return err
}

// IsLoopback reports whether host is one of the three spellings a
// credential may cross to in clear text; see loopbackHosts.
//
// Configuration validation and the send path both ask it, so a
// configuration that validates is one the send path will not refuse on
// this ground, and the reverse.
func IsLoopback(host string) bool {
	return loopbackHosts[host]
}

// TrimBase returns raw without surrounding space or trailing slashes: the
// form somebody pastes from a browser, cut back to the one every URL is
// built on. ParseBaseURL reads a base through it, and so does every caller
// that builds on a base or on a path prefix without parsing it again, so
// the two are trimmed alike wherever they are read.
func TrimBase(raw string) string {
	return strings.TrimRight(strings.TrimSpace(raw), "/")
}

// IsSuccess reports whether an HTTP status is a 2xx, the one range an
// answer counts as a success in. httpclient and telegram both ask it of
// every answer, so the two draw the boundary in the same place.
func IsSuccess(code int) bool {
	return code >= 200 && code < 300
}

// NormaliseMethod returns the form an HTTP method is compared in: upper
// case, the spelling fiber.DefaultMethods lists every method in and the
// one Fiber's router looks the method of a route up in. config holds each
// fiber.client.no_retry_methods entry to that list in this form, and
// httpclient matches the method of every request against the entries in
// it, so "post" and "POST" are one setting in both places. config also
// refuses a fiber.request_methods entry that is not already in this form,
// since the router would never find it.
//
// Nothing is trimmed. A method with space around it names no method, and
// the configuration check refuses it by name rather than repairing it.
func NormaliseMethod(method string) string {
	return strings.ToUpper(method)
}

// Required is the error for a key left unset: "<key> is required",
// followed by why as written. why carries its own opening, ": " before a
// reason or a condition such as " when ...", and is empty where the
// requirement needs no reason of its own.
//
// config words every key it requires with it, CheckPort an absent port,
// and httpclient.New an absent base URL, so a setting nobody supplied is
// reported in one wording wherever it is read.
func Required(key, why string) error {
	return errors.New(key + " is required" + why)
}

// NotInitialised is the error for a section that was never built: a nil
// pointer to one, or a wrapper whose embedded middleware config is nil.
// section names what the section stands for, the prefix its keys share
// where the caller names keys, and a plain word where it names none.
//
// Every Validate in config opens with it, database.New refuses a nil app
// section with it and httpclient.New a nil client section, so a section
// nobody built is refused in one wording whichever of them is handed it.
func NotInitialised(section string) error {
	return errors.New(section + " config was not initialised")
}

// CheckPort reports a port key that is unset or outside 1..65535. Zero is
// the value an absent key reads as, so it is reported as required rather
// than as out of range.
//
// config asks it of app.port and notification.email.port, and database
// asks it of database.port before the number becomes text in a DSN, so a
// port is held to one range and refused in one wording wherever it is
// read.
func CheckPort(key string, port int) error {
	switch {
	case port == 0:
		return Required(key, "")
	case port < 0 || port > 65535:
		return fmt.Errorf("%s must be between 1 and 65535 (got %d)",
			key,
			port)
	}
	return nil
}

// CheckSection is the gate a section passes before a package builds on
// it. For a section nobody supplied, as Configured reports, it returns
// absent, the caller's sentinel for a deployment without that peer, and
// does not ask Validate, since there is nothing to validate. For a
// supplied one it returns what Validate returns, so a section filled in
// halfway fails loudly instead of reading as absent.
//
// email.New and telegram.New both open with it, so the two tell an absent
// section from a broken one by one rule. A nil pointer to one of config's
// sections reads as absent, since its Configured reports false for nil.
func CheckSection(cfg Optional, absent error) error {
	if !cfg.Configured() {
		return absent
	}
	return cfg.Validate()
}

// RedactUserinfo returns raw with any userinfo replaced by Masked, so the
// URL can be quoted in an error message or printed in a log line without
// the credential it may carry: "https://u:p@host/x" becomes
// "https://***@host/x".
//
// It works on the string rather than on a parsed *url.URL because the
// place it matters most is where url.Parse has FAILED, and there is then
// no parsed value to consult. A password with a space in it is enough to
// get there, and url.Parse puts its whole input into the error it returns.
//
// Conservative on purpose: the LAST "@" anywhere in raw is taken to end a
// userinfo, since a password may contain "@", "/" or "?" of its own. A URL
// whose path or query happens to contain an "@" therefore has everything
// before that "@" masked, host included. Guessing wrong that way costs a
// less readable message; guessing wrong the other way costs a password in
// the log.
func RedactUserinfo(raw string) string {
	at := strings.LastIndex(raw, "@")
	if at < 0 {
		return raw
	}
	// The scheme is kept when one precedes the "@", so the masked URL
	// still says which protocol it named.
	prefix := ""
	if i := strings.Index(raw[:at], "://"); i >= 0 {
		prefix = raw[:i+len("://")]
	}
	return prefix + Masked + raw[at:]
}

// ParseCause returns what url.Parse objected to, without any of the input
// it objected to. The result is a new error holding only text, so nothing
// should expect to unwrap it into a net/url type. A nil err returns nil.
//
// A *url.Error carries the whole raw URL, userinfo included, and wrapping
// it with %w would repeat in the same message the credential that
// RedactUserinfo kept out of the rest of it. The cause it wraps names the
// problem, but several causes also quote a fragment of the input, and the
// fragment can be the credential:
//
//   - "invalid port %q after host" quotes what follows the colon in what
//     url.Parse took to be the host. A "/", "?" or "#" inside a password
//     ends the authority early, so for "https://u:secret/x@host" the host
//     is "u:secret" and the quoted port is ":secret", the whole password.
//   - "invalid URL escape %q" quotes a malformed percent-escape: the "%"
//     and up to two characters after it, which may sit in the password.
//   - "invalid host: %w" wraps a net/netip error that quotes the address
//     between the brackets, and "invalid character %q in host name"
//     quotes a single byte.
//
// So every quoted fragment is replaced with Masked, quoted, whatever the
// cause, and the message keeps its shape, `invalid port "***" after host`,
// while losing what it quoted. Only a fragment repeated WITHOUT quotes
// would get through, and net/url repeats none that way.
func ParseCause(err error) error {
	err = withoutURL(err)
	if err == nil {
		return nil
	}
	return errors.New(quotedFragment.ReplaceAllLiteralString(err.Error(),
		strconv.Quote(Masked)))
}

// ParseBaseURL parses raw as the base URL of an HTTP service and returns
// it, or an error saying what makes it unusable. Surrounding space and
// trailing slashes are dropped first, through TrimBase, so the form
// somebody pastes from a browser is accepted.
//
// name is what the error calls the URL: the configuration key it was read
// from, or a plain description where the caller cannot know the source.
// userinfoAdvice ends the error for a URL carrying userinfo, which is
// refused because every URL built on the base would quote the credential;
// it says where the caller wants a credential put instead.
//
// Every error quotes raw through RedactUserinfo, and the one for a URL
// url.Parse cannot read reports the cause through ParseCause, so no error
// returned here repeats a credential the URL held.
func ParseBaseURL(name, raw, userinfoAdvice string) (*url.URL, error) {
	shown := RedactUserinfo(raw)
	u, err := url.Parse(TrimBase(raw))
	switch {
	case err != nil:
		return nil, fmt.Errorf("%s is not a valid URL (got %q): %w",
			name,
			shown,
			ParseCause(err))
	case u.Scheme != "http" && u.Scheme != "https":
		return nil, fmt.Errorf("%s must start with http:// or https:// "+
			"(got %q)",
			name,
			shown)
	// A scheme on its own clears the case above. Trimming takes both
	// slashes off "https://", leaving "https:", which parses as a scheme
	// with an empty host and an empty path, and a URL built on it,
	// "https:///items", fails every request in the transport with a
	// message that names neither the setting nor the reason.
	case u.Host == "":
		return nil, fmt.Errorf("%s names no host (got %q): every request "+
			"would be built against an address with nothing to dial",
			name,
			shown)
	case u.User != nil:
		return nil, fmt.Errorf("%s must not carry userinfo (got %q): %s",
			name,
			shown,
			userinfoAdvice)
	}
	return u, nil
}

// StepErr names the step of an exchange with another server that err
// ended, as "<step>: <err>".
//
// When ctx has ended too and err does not already say so, ctx's error is
// wrapped alongside, so a caller tests for context.DeadlineExceeded
// whichever layer noticed first. A deadline enforced through the
// connection surfaces as an I/O timeout that mentions no context, and
// without the second %w errors.Is could not tell it from a stalled server.
//
// A *url.Error around err is dropped first, for the reason withoutURL
// gives: net/http wraps every failure of a request in one, quoting the URL
// the request was sent to.
func StepErr(ctx context.Context, step string, err error) error {
	err = withoutURL(err)
	if cerr := ctx.Err(); cerr != nil && !errors.Is(err, cerr) {
		return fmt.Errorf("%s: %w: %w", step, cerr, err)
	}
	return fmt.Errorf("%s: %w", step, err)
}
