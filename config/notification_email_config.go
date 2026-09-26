package config

// The notification.email.* section of config.yaml.
//
// Keys in this section, with their environment-variable spellings:
//
//	notification.email.host
//		NOTIFICATION_EMAIL_HOST
//	notification.email.port
//		NOTIFICATION_EMAIL_PORT
//	notification.email.tls
//		NOTIFICATION_EMAIL_TLS
//	notification.email.insecure
//		NOTIFICATION_EMAIL_INSECURE
//	notification.email.username
//		NOTIFICATION_EMAIL_USERNAME
//	notification.email.password
//		NOTIFICATION_EMAIL_PASSWORD
//	notification.email.from
//		NOTIFICATION_EMAIL_FROM
//	notification.email.timeout
//		NOTIFICATION_EMAIL_TIMEOUT
//	notification.email.async
//		NOTIFICATION_EMAIL_ASYNC
//
// Nine keys, and that is the whole section — NewEmailConfig below reads
// exactly these. The environment spelling holds only for a Viper built by
// NewViper; see the package documentation.
//
// The section nests under notification.* the way the fiber sections nest
// under fiber.*: a section of its own, which SuppliedSections routes to on
// the longer prefix, so a deployment that sends no mail supplies nothing
// here and has nothing here validated. Whether mail is sent at all is
// notification.channels; this section says where it goes and whether the
// request waits for it.
//
// The prefix also keeps these variables out of other stacks' way.
// MAIL_HOST, MAIL_PORT and SMTP_HOST are what several frameworks already
// export, and a section behind either spelling would be switched on by a
// variable meant for something else. NOTIFICATION_EMAIL_* is nobody else's.
//
// notification.email.username and notification.email.password belong in
// the environment and never in a file that is committed, the same as the
// database pair.

import (
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/spf13/viper"
)

// The values notification.email.tls accepts. Compare against these rather
// than bare literals; the email package does.
const (
	// EmailTLSStartTLS connects in clear text and upgrades with STARTTLS
	// before anything else is said. Conventionally port 587.
	EmailTLSStartTLS = "starttls"

	// EmailTLSImplicit speaks TLS from the first byte, which mail clients
	// label "SSL/TLS". Conventionally port 465.
	EmailTLSImplicit = "implicit"

	// EmailTLSNone never negotiates TLS. For a relay on the same host or a
	// closed network segment, and never with credentials to anything but
	// loopback; see EmailConfig.Validate.
	EmailTLSNone = "none"
)

// validEmailTLS is the allowlist checked by EmailConfig.Validate. The
// empty-struct value is the idiomatic set, the same shape validEnvs in
// app_config.go uses.
var validEmailTLS = map[string]struct{}{
	EmailTLSStartTLS: {},
	EmailTLSImplicit: {},
	EmailTLSNone:     {},
}

// loopbackHosts are the host spellings a credential may cross to in clear
// text. They are the three net/smtp's PlainAuth accepts, and the email
// package applies the same three when it logs in, so a configuration
// Validate passes is one the send path will not refuse on this ground.
var loopbackHosts = map[string]struct{}{
	"localhost": {},
	"127.0.0.1": {},
	"::1":       {},
}

// EmailConfig holds the SMTP relay outbound mail is handed to, and the
// sender every message carries. String omits the credentials; see the
// note there on what it prints in their place.
type EmailConfig struct {
	// Host is the SMTP server's hostname or IP address. Required.
	//
	// Under starttls and implicit it is also the name the server's
	// certificate is checked against, so it has to be a name the
	// certificate was issued for. An IP address passes only a certificate
	// that lists that IP, which most relays' certificates do not.
	Host string

	// Port is the SMTP server's TCP port, 1 to 65535. Required.
	//
	//	587  submission, with tls: starttls
	//	465  submission, with tls: implicit
	//	25   relay between servers; for an internal relay, typically
	//	     with tls: none or starttls
	//
	// Validate holds 587 and 465 to those pairings. The mismatch on 465
	// does not fail, it HANGS: the client waits for a greeting while the
	// server waits for a TLS handshake, so every send sits out the whole
	// timeout and then reports a deadline that names neither key.
	Port int

	// TLS selects how the connection is secured: starttls, implicit or
	// none. Required, with no default and no derivation from Port,
	// because a relay on any port but the two above implies nothing and a
	// guess would be a mode nobody chose.
	//
	// starttls is strict. A server that does not offer STARTTLS fails the
	// send, and nothing carries on in clear text. The offer arrives before
	// the connection is encrypted, so anyone on the path can delete it,
	// and falling back when it is missing would hand exactly that person
	// the message and the password.
	//
	// Normalised to lowercase by NewEmailConfig.
	TLS string

	// Insecure skips verification of the server's certificate. The real
	// case is an internal relay with a self-signed or mismatched
	// certificate; anything reachable from outside should fix its
	// certificate instead.
	//
	// Inert under tls: none, which negotiates no TLS to skip the checks
	// of. As with fiber.client.insecure, that makes true a setting that
	// starts doing something the day tls changes and nobody revisits this
	// line.
	Insecure bool

	// Username authenticates to the relay, with PLAIN or LOGIN, whichever
	// the server offers. Set together with Password or not at all: a
	// relay that accepts mail by network position rather than by login is
	// a real deployment, and leaving both empty is how it is configured.
	//
	// Neither is trimmed, because a password may begin or end with a
	// space, and under tls: none both are refused unless Host is loopback.
	// See Validate.
	Username string

	// Password is Username's pair. Required when Username is set, and
	// never printed by String.
	Password string

	// From is the sender every message carries, a bare address or
	// "Name <address>". Required. The address is also the envelope
	// sender, which is where bounces go.
	//
	// A relay that authenticates usually holds From to the account: some
	// refuse any other address, others quietly rewrite it to the
	// account's own. On those it has to be the account itself or an
	// alias the account owns.
	From string

	// Timeout bounds one whole send: connecting, TLS, login, and the
	// upload of the message itself. Required, and at least one second.
	//
	// It is the only bound a send has when nothing else supplies one,
	// which is the normal case under Async: a send there runs on a
	// context detached from the request that asked for it. So size
	// it for the largest attachment over the slowest link rather than for
	// the handshake; 30s is a reasonable start.
	//
	// A bare number parses as NANOSECONDS, which the one-second floor in
	// Validate turns into a startup error. Always write a unit.
	Timeout time.Duration

	// Async hands each send to a detached background goroutine instead of
	// running it on the request that asked for it. The trade either
	// setting makes is the same on every channel, so it is described once,
	// on NotificationConfig.
	//
	// What is particular to mail is the bound: Timeout limits a send in
	// both modes, and under Async it is the only limit, since no request
	// is waiting to end it. The email package documentation says how to
	// hand a send to a goroutine without it reading freed request memory.
	Async bool
}

// NewEmailConfig reads EmailConfig fields from the provided Viper instance.
//
// Never returns an error: absent keys and uncastable values both come back
// as zero values, which Validate rejects afterwards.
//
// This function is the authoritative list of keys the notification.email.*
// section supports. A key present in config.yaml but missing here is dead
// weight — Viper never looks it up, so neither the file nor an environment
// variable can supply it.
//
// Host, TLS and From are trimmed on the way in, because a stray space in
// any of them is always a typo and never a value. Username and Password
// are deliberately not: silently editing a credential would produce a
// login failure this package caused.
func NewEmailConfig(v *viper.Viper) *EmailConfig {
	return &EmailConfig{
		Host: strings.TrimSpace(v.GetString("notification.email.host")),
		Port: v.GetInt("notification.email.port"),
		TLS: strings.ToLower(
			strings.TrimSpace(v.GetString("notification.email.tls"))),
		Insecure: v.GetBool("notification.email.insecure"),
		Username: v.GetString("notification.email.username"),
		Password: v.GetString("notification.email.password"),
		From:     strings.TrimSpace(v.GetString("notification.email.from")),
		Timeout:  v.GetDuration("notification.email.timeout"),
		Async:    v.GetBool("notification.email.async"),
	}
}

// Validate returns a joined error for every invalid or missing EmailConfig
// field.
//
// Deliberately unchecked:
//
//   - Whether the relay is REACHABLE, or accepts these credentials.
//     Nothing here dials it. A relay that is down, or a password that has
//     expired, fails per send and is logged, which is the right place for
//     it: mail is allowed to be down without taking this service's
//     startup with it.
//   - Whether Host matches the relay's certificate. Only a handshake can
//     say, and the first send is the first handshake.
//   - Insecure. A bool has no invalid value; the field says what true
//     costs.
//   - Async, for the same reason. NotificationConfig says what each
//     setting costs.
//
// Host and TLS are normalised again here, for the same reason kind() in
// database_config.go re-lowercases the driver: an EmailConfig built as a
// struct literal never passed through NewEmailConfig.
//
// Every check appends rather than returning early, so one restart surfaces
// every notification.email.* problem at once.
func (c *EmailConfig) Validate() error {
	// errs is declared before the nil check only so the two statements read
	// in the same order in all Validate implementations; the nil check is
	// what must come first, since every line after it dereferences c.
	var errs []error
	if c == nil {
		return errors.New("notification.email config was not initialised")
	}

	host := strings.TrimSpace(c.Host)
	mode := strings.ToLower(strings.TrimSpace(c.TLS))

	if host == "" {
		errs = append(errs, errors.New(
			"notification.email.host is required"))
	}
	if c.Port == 0 {
		errs = append(errs, errors.New(
			"notification.email.port is required"))
	} else if c.Port < 0 || c.Port > 65535 {
		errs = append(errs, fmt.Errorf("notification.email.port must be "+
			"between 1 and 65535 (got %d)",
			c.Port))
	}

	_, known := validEmailTLS[mode]
	switch {
	case mode == "":
		errs = append(errs, errors.New(`notification.email.tls is `+
			`required: use "starttls" (port 587), "implicit" (port 465) `+
			`or "none". It has no default, because a relay on any other `+
			`port implies nothing and a guess would be a mode nobody chose`))
	case !known:
		errs = append(errs, fmt.Errorf("notification.email.tls must be one "+
			"of: starttls, implicit, none (got %q)",
			c.TLS))
	}

	// The two ports whose convention is fixed are held to it, and only
	// once the mode is known to be valid, so a misspelled mode reports
	// itself rather than a mismatch it caused. 465 is the one that
	// matters: the wrong pairing there waits on a greeting the server will
	// never send, and 587's fails at once but with a TLS error that names
	// neither key.
	if known {
		switch {
		case c.Port == 465 && mode != EmailTLSImplicit:
			errs = append(errs, fmt.Errorf("notification.email.port 465 "+
				"carries implicit TLS, so notification.email.tls must be "+
				"implicit (got %q): anything else waits for a greeting the "+
				"server never sends, and every send hangs until the timeout",
				c.TLS))
		case c.Port == 587 && mode == EmailTLSImplicit:
			errs = append(errs, errors.New("notification.email.port 587 "+
				"carries STARTTLS, so notification.email.tls cannot be "+
				"implicit: the server's greeting arrives in clear text and "+
				"fails the handshake on every send — use starttls"))
		}
	}

	if c.From == "" {
		errs = append(errs, errors.New(
			"notification.email.from is required"))
	} else if _, err := mail.ParseAddress(c.From); err != nil {
		errs = append(errs, fmt.Errorf("notification.email.from is not an "+
			`address (got %q): write "noreply@example.com" or `+
			`"Name <noreply@example.com>"`,
			c.From))
	}

	// The pair is checked on its own terms first: one half without the
	// other is a login that cannot succeed, and it would otherwise surface
	// as an authentication failure on the first send.
	creds := c.Username != "" || c.Password != ""
	if (c.Username == "") != (c.Password == "") {
		errs = append(errs, errors.New("notification.email.username and "+
			"notification.email.password are set together or not at all"))
	}
	// Credentials over a connection that is never encrypted are refused
	// here rather than left to the send path, which refuses them too —
	// but only on the first send, possibly hours after startup, and in a
	// goroutine whose error nobody is waiting for.
	_, local := loopbackHosts[host]
	if creds && mode == EmailTLSNone && !local {
		errs = append(errs, fmt.Errorf("notification.email.tls is none, "+
			"so the credentials would cross to %s in clear text, which the "+
			"send path refuses: use starttls or implicit, or a relay on "+
			"loopback",
			host))
	}

	if c.Timeout == 0 {
		errs = append(errs, errors.New("notification.email.timeout is "+
			"required: it is the only bound a send has once it runs "+
			"detached from a request, and without one a relay that stops "+
			"answering holds that send open indefinitely"))
	} else if c.Timeout < time.Second {
		errs = append(errs, fmt.Errorf("notification.email.timeout must be "+
			"at least 1s (got %s): no SMTP conversation completes in less, "+
			"and if this was meant as seconds, write the unit — a bare "+
			"number is nanoseconds",
			c.Timeout))
	}

	return errors.Join(errs...)
}

// String returns a loggable representation of EmailConfig.
//
// The pointer receiver means fmt only picks this up for a *EmailConfig.
// Printing a value copy (%v on EmailConfig, not &EmailConfig) bypasses it
// and dumps the struct fields directly — the password included.
//
// The credentials are reported as PRESENT OR NOT rather than printed, as
// fiber.client.token is. Their absence is legal, so a line that showed
// nothing at all could not tell a relay that accepts mail by network
// position from a password that never reached the process.
func (c *EmailConfig) String() string {
	if c == nil {
		return "<nil EmailConfig>"
	}

	auth := "(none)"
	if c.Username != "" || c.Password != "" {
		auth = "(set)"
	}

	return fmt.Sprintf("Host=%s "+
		"Port=%d "+
		"TLS=%s "+
		"Insecure=%t "+
		"Auth=%s "+
		"From=%s "+
		"Timeout=%s "+
		"Async=%t",
		c.Host,
		c.Port,
		c.TLS,
		c.Insecure,
		auth,
		c.From,
		c.Timeout,
		c.Async,
	)
}
