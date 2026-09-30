package config

// Quoting a URL that may carry a credential, for the one section that holds
// such a URL: fiber.client.base_url. Like splitlist.go, it lives outside that
// section's file, so that each section file stays the authoritative list of
// its own keys and nothing else.
//
// A credential can reach an error message by two routes, and each helper
// closes one: redactUserinfo masks the URL as written, and urlParseCause
// strips the input out of what url.Parse reports when it cannot read it.

import (
	"errors"
	"net/url"
	"regexp"
	"strings"
)

// quotedFragment matches one double-quoted Go string literal, escaped
// characters included. That is the form strconv.Quote and the %q verb
// produce, and the form in which every url.Parse cause that repeats part
// of its input repeats it.
var quotedFragment = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)

// urlParseCause returns what url.Parse objected to, without any of the
// input it objected to. The result is a new error holding only text, so
// nothing should expect to unwrap it into a net/url type. A nil err
// returns nil.
//
// A *url.Error carries the whole raw URL, userinfo included, and wrapping
// it with %w would repeat in the same message the credential that
// redactUserinfo kept out of the rest of it. The cause it wraps names the
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
// So every quoted fragment is replaced with "***" whatever the cause, and
// the message keeps its shape, `invalid port "***" after host`, while
// losing what it quoted. Only a fragment repeated WITHOUT quotes would get
// through, and net/url repeats none that way.
func urlParseCause(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	if err == nil {
		return nil
	}
	return errors.New(quotedFragment.ReplaceAllString(err.Error(), `"***"`))
}

// redactUserinfo returns raw with any userinfo replaced by "***", so the
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
func redactUserinfo(raw string) string {
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
	return prefix + "***" + raw[at:]
}
