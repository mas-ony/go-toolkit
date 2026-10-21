package config

// The notification.telegram.* section of config.yaml.
//
// Keys in this section, with their environment-variable spellings:
//
//	notification.telegram.token
//		NOTIFICATION_TELEGRAM_TOKEN
//	notification.telegram.api_url
//		NOTIFICATION_TELEGRAM_API_URL
//	notification.telegram.timeout
//		NOTIFICATION_TELEGRAM_TIMEOUT
//	notification.telegram.async
//		NOTIFICATION_TELEGRAM_ASYNC
//
// Four keys, and that is the whole section — NewTelegramConfig below reads
// exactly these. The environment spelling holds only for a Viper built by
// NewViper; see the package documentation.
//
// The section nests under notification.* like the other channels, and the
// prefix does a second job here. Tools that talk to Telegram export
// TELEGRAM_BOT_TOKEN or TELEGRAM_TOKEN for themselves, so a section
// prefixed telegram would be switched on by a variable meant for something
// else. NOTIFICATION_TELEGRAM_* is nobody else's. Whether Telegram is used
// at all is notification.channels; this section says which bot sends, and
// whether the request waits for it.
//
// notification.telegram.token belongs in the environment and never in a
// file that is committed, the same as the database pair.

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/mas-ony/go-toolkit/internal/upstream"
	"github.com/spf13/viper"
)

// DefaultTelegramAPIURL is the hosted Bot API, which an empty
// notification.telegram.api_url stands for.
//
// NewTelegramConfig leaves the field empty and the telegram package applies
// this, so a TelegramConfig that nothing supplied stays equal to the zero
// value, which is how Configured, and through it telegram.New, tells "not
// configured" from "configured halfway".
const DefaultTelegramAPIURL = "https://api.telegram.org"

// telegramToken is the shape BotFather issues: the bot's numeric id, a
// colon, then the secret.
//
// It checks the shape and nothing more. Whether Telegram issued this token,
// or has since revoked it, is only ever answered by a send.
var telegramToken = regexp.MustCompile(`^[0-9]+:[A-Za-z0-9_-]+$`)

// TelegramConfig holds the bot Telegram notifications are sent as, and how
// its requests are made. String omits the token; see the note there on what
// it prints in its place.
type TelegramConfig struct {
	// Token is the bot's credential, as BotFather issues it: the bot's
	// numeric id, a colon, then a secret of letters, digits, underscores
	// and hyphens. Required.
	//
	// It is part of the URL of every request, which is how the Bot API
	// takes it, so whatever logs request URLs between here and the API
	// holds it. The telegram package keeps it out of the errors and log
	// lines it writes, and Validate never quotes it.
	//
	// Trimmed by NewTelegramConfig, because a stray space or newline (a
	// secret read from a file ends in one) is always a typo and never part
	// of a token.
	Token string

	// APIURL is the root of the Bot API. Empty means DefaultTelegramAPIURL,
	// the hosted API, which is what nearly every deployment wants. Set it
	// for a Bot API server of your own, or for a proxy in front of the
	// hosted one.
	//
	// Written as scheme://host[:port][/path]. A path prefix is kept and a
	// trailing slash is dropped, because the telegram package appends
	// /bot<token>/<method>. A query or fragment would swallow that suffix,
	// and userinfo would reach the log through String, so Validate refuses
	// all three.
	//
	// http:// is accepted only for a host on loopback, matched exactly as
	// notification.email.host is. The token is in every request path, so a
	// plain-text hop would carry it in the clear.
	APIURL string

	// Timeout bounds one whole send: connecting, TLS, the request and its
	// answer, and for a document the upload. Required, and at least one
	// second.
	//
	// It is the only bound a send has when nothing else supplies one, which
	// is the normal case under Async: a send there runs on a context
	// detached from the request that asked for it. So size it for the
	// largest document over the slowest link rather than for a text
	// message; 30s is a reasonable start.
	//
	// A bare number parses as NANOSECONDS, which the one-second floor in
	// Validate turns into a startup error. Always write a unit.
	Timeout time.Duration

	// Async hands each send to a detached background goroutine instead of
	// running it on the request that asked for it. The trade either setting
	// makes is the same on every channel, so it is described once, on
	// NotificationConfig.
	//
	// What is particular to Telegram is the bound: Timeout limits a send in
	// both modes, and under Async it is the only limit, since no request is
	// waiting to end it. The telegram package documentation says how to
	// hand a send to a goroutine without it reading freed request memory.
	Async bool
}

// NewTelegramConfig reads TelegramConfig fields from the provided Viper
// instance.
//
// Never returns an error: absent keys and uncastable values both come back
// as zero values, which Validate rejects afterwards, except APIURL, where
// empty is a setting of its own.
//
// This function is the authoritative list of keys the
// notification.telegram.* section supports. A key present in config.yaml
// but missing here is dead weight — Viper never looks it up, so neither
// the file nor an environment variable can supply it.
//
// Token and APIURL are trimmed on the way in, because a stray space in
// either is always a typo and never a value.
func NewTelegramConfig(v *viper.Viper) *TelegramConfig {
	return &TelegramConfig{
		Token: strings.TrimSpace(
			v.GetString("notification.telegram.token")),
		APIURL: strings.TrimSpace(
			v.GetString("notification.telegram.api_url")),
		Timeout: v.GetDuration("notification.telegram.timeout"),
		Async:   v.GetBool("notification.telegram.async"),
	}
}

// Configured reports whether any notification.telegram.* setting reached
// this section. A nil receiver reports false.
//
// It is the question telegram.New asks first, and the rule is the one
// DatabaseConfig.Configured applies: a section nothing supplied holds no
// bot to send as, and is refused as not configured rather than with an
// error for every key it lacks. It asks whether ANY field is set, not
// whether the section is usable; that is Validate's question.
func (c *TelegramConfig) Configured() bool { return configured(c) }

// Validate returns a joined error for every invalid or missing
// TelegramConfig field.
//
// Deliberately unchecked:
//
//   - Whether the token is VALID, or the API reachable. Nothing here dials.
//     A revoked token, or a network that cannot reach Telegram, fails per
//     send and is logged: a notification channel is allowed to be down
//     without taking this service's startup with it.
//   - Async. Both settings are meaningful and a bool has no invalid value.
//     NotificationConfig says what each costs.
//
// No message quotes the token, whatever is wrong with it, and none quotes
// the API URL as written: each shows it with any userinfo masked, and a
// URL that does not parse has the fragments the parser quotes masked too.
// The configuration error is printed to a terminal and pasted into
// tickets, so neither credential may be in it.
//
// Token and APIURL are trimmed again here, for the same reason
// EmailConfig's TLS is lowercased again: a TelegramConfig built as a struct
// literal never passed through NewTelegramConfig.
//
// Every check appends rather than returning early, so one restart surfaces
// every notification.telegram.* problem at once.
func (c *TelegramConfig) Validate() error {
	// errs is declared before the nil check only so the two statements read
	// in the same order in all Validate implementations; the nil check is
	// what must come first, since every line after it dereferences c.
	var errs []error
	if c == nil {
		return errors.New("notification.telegram config was not initialised")
	}

	switch token := strings.TrimSpace(c.Token); {
	case token == "":
		errs = append(errs, errors.New(
			"notification.telegram.token is required"))
	case !telegramToken.MatchString(token):
		errs = append(errs, errors.New("notification.telegram.token is not "+
			"a bot token (the value is withheld): want what BotFather "+
			"issues, the bot's numeric id, a colon, then letters, digits, "+
			"underscores or hyphens"))
	}

	// A STATED API URL is checked, an absent one is not: empty is the
	// hosted API. upstream.ParseBaseURL holds it to the rules
	// fiber.client.base_url is held to, and two more come from the token
	// living in the path. Trimming never adds or removes a "?" or "#", so
	// the value as written answers whether it carries one.
	if c.APIURL != "" {
		u, err := upstream.ParseBaseURL("notification.telegram.api_url",
			c.APIURL, loggedCredential)
		switch {
		case err != nil:
			errs = append(errs, err)
		case strings.ContainsAny(c.APIURL, "?#"):
			errs = append(errs, fmt.Errorf("notification.telegram.api_url "+
				"must not carry a query or fragment (got %q): the method "+
				"and the token are appended to its path, and either would "+
				"swallow them",
				upstream.RedactUserinfo(c.APIURL)))
		case u.Scheme == "http" && !upstream.IsLoopback(u.Hostname()):
			errs = append(errs, fmt.Errorf("notification.telegram.api_url "+
				"is http://, so the token, which is part of every request "+
				"path, would cross to %s in clear text: use https, or a "+
				"server on loopback",
				u.Hostname()))
		}
	}

	err := checkSendTimeout("notification.telegram.timeout", c.Timeout,
		"an API", "request to the API")
	if err != nil {
		errs = append(errs, err)
	}

	return errors.Join(errs...)
}

// String returns a loggable representation of TelegramConfig.
//
// The pointer receiver means fmt only picks this up for a *TelegramConfig.
// Printing a value copy (%v on TelegramConfig, not &TelegramConfig)
// bypasses it and dumps the struct fields directly, the token included.
//
// The token is reported as PRESENT OR NOT rather than printed, as
// fiber.client.token is. Its absence is an error, but String is also what
// logs a section that failed validation, and a line that showed nothing at
// all could not tell a token that never reached the process from one that
// did.
//
// An empty APIURL prints as the address it stands for, so the startup line
// says which API the bot will talk to. Any userinfo is masked, for a
// String called on a configuration that did not start.
func (c *TelegramConfig) String() string {
	if c == nil {
		return "<nil TelegramConfig>"
	}

	token := setOrNone(c.Token != "")
	api := upstream.RedactUserinfo(c.APIURL)
	if api == "" {
		api = DefaultTelegramAPIURL + " (default)"
	}

	return fmt.Sprintf("Token=%s "+
		"APIURL=%s "+
		"Timeout=%s "+
		"Async=%t",
		token,
		api,
		c.Timeout,
		c.Async,
	)
}
