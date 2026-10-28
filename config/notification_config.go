package config

// The notification.* section of config.yaml.
//
// Keys in this section, with their environment-variable spellings:
//
//	notification.channels
//		NOTIFICATION_CHANNELS
//
// One key, and that is the whole section — NewNotificationConfig below reads
// exactly that one. The environment spelling holds only for a Viper built by
// NewViper; see the package documentation.
//
// Each channel has a section of its own nested here and named after it:
// notification.whatsapp.* in notification_whatsapp_config.go,
// notification.email.* in notification_email_config.go and
// notification.telegram.* in notification_telegram_config.go. This key
// chooses which channels send; their sections say how.

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/mas-ony/go-toolkit/internal/strlist"
	"github.com/mas-ony/go-toolkit/internal/upstream"
	"github.com/spf13/viper"
)

// NotificationConfig chooses the channels notifications go out on.
//
// It is the switch for the channel sections, as AuthConfig is for the
// authentication ones: a channel's section is used, and validated, only
// while Enabled reports that channel.
//
// It holds the choice and nothing else. How a channel sends is in that
// channel's own section, so switching a channel off is one word here, and
// its section can stay where it is.
//
// Each channel section carries its own async key, because the channels do
// not cost the same: a WhatsApp document upload and an SMTP conversation
// are different waits, and a deployment may want one in the background
// and the other waited for. What async trades is the same on every
// channel:
//
//   - false, synchronous: the HTTP handler that creates or updates the
//     record blocks until the send completes or fails. Saving is
//     therefore at least as slow as the provider round-trip (network plus
//     the provider's own servers), which is typically a second or more
//     and can occasionally stall until it times out.
//   - true, asynchronous: the whole notification job runs in a background
//     goroutine. The handler returns as soon as the database row is
//     committed, so the user never waits on the provider. Delivery
//     becomes fire-and-forget: a send that fails is logged but cannot be
//     reflected in the HTTP response, and a notification still in flight
//     when the process shuts down is dropped.
//
// Apart from shutdown, async gives up no guarantee that sync provides,
// PROVIDED the application treats delivery as best-effort in both modes —
// every send error logged rather than surfaced to the caller, and never
// allowed to roll back the row. An application that does surface the error
// is making a different trade, and the async key is where it disappears.
//
// Shutdown is the one real difference. A graceful stop waits for the
// requests in flight, so a synchronous send finishes inside one of them.
// Nothing waits for a background goroutine unless the application tracks
// its sends, with a sync.WaitGroup for instance, and waits for them once
// the server has stopped accepting requests.
type NotificationConfig struct {
	// Channels lists the channels in use: ChannelWhatsApp, ChannelEmail,
	// ChannelTelegram, any of them together, or none. It is read as a YAML
	// sequence, or as a string separated by commas or by spaces, and
	// NewNotificationConfig lowercases and trims each entry. Empty means no
	// channel sends, which is also what a deployment without this section
	// gets.
	Channels []string
}

// The channels notification.channels accepts. Each is also the name of
// the section that configures it, nested under notification.
const (
	ChannelWhatsApp = "whatsapp"
	ChannelEmail    = "email"
	ChannelTelegram = "telegram"
)

// validChannels is the allowlist checked by NotificationConfig.Validate,
// in the order its error names them.
//
// Checked because a typo here is INVISIBLE at runtime. Enabled answers
// false for a channel spelled "emial", so no mail is ever sent, and the
// only symptom is a recipient who never hears about anything.
var validChannels = []string{ChannelWhatsApp, ChannelEmail, ChannelTelegram}

// NewNotificationConfig reads NotificationConfig fields from the provided
// Viper instance.
//
// Never returns an error: an absent key comes back as an empty list, which
// is a valid setting, and an entry that names no channel is left for
// Validate to reject.
//
// This function is the authoritative list of keys the notification.* section
// supports. A key present in config.yaml but missing here is dead weight —
// Viper never looks it up, so neither the file nor an environment variable can
// supply it. The channel sections nested under it read their own keys.
//
// Every entry is stored in the form a channel is compared in,
// strlist.Normalise's. Validate and Enabled apply it again to what they
// compare, because a NotificationConfig built as a struct literal never
// passed through this constructor.
func NewNotificationConfig(v *viper.Viper) *NotificationConfig {
	// Read through splitList, so NOTIFICATION_CHANNELS=whatsapp,email is
	// two channels rather than one named "whatsapp,email".
	raw := splitList(v, "notification.channels")
	channels := make([]string, len(raw))
	for i, ch := range raw {
		channels[i] = strlist.Normalise(ch)
	}
	return &NotificationConfig{Channels: channels}
}

// Enabled reports whether channel is one of Channels, ignoring case and
// surrounding spaces. It is safe on a nil receiver, which enables nothing.
//
// It is the branch a service takes before sending on a channel, and
// before validating that channel's section: an entry given twice is still
// one channel, and a channel not listed is off however completely its
// section is filled in.
func (c *NotificationConfig) Enabled(channel string) bool {
	if c == nil {
		return false
	}
	want := strlist.Normalise(channel)
	for _, ch := range c.Channels {
		if strlist.Normalise(ch) == want {
			return true
		}
	}
	return false
}

// Validate returns a joined error for every invalid or missing
// NotificationConfig field.
//
// Entries are checked, the LIST is not. An empty one is the setting for no
// notifications rather than a mistake, and an entry given twice changes
// nothing, since Enabled asks about membership.
//
// Deliberately unchecked:
//
//   - Whether a chosen channel is CONFIGURED. Validate sees one section,
//     and whether notification.email was supplied is another section's
//     answer. The application sees both, and answers it by validating a
//     chosen channel's section whether or not anything supplied it; the
//     email and telegram packages refuse an empty section on their own
//     account too, as ErrNotConfigured.
//   - Whether any service actually honours the choice. It is passed to each
//     notifying service by hand at wiring time; a path added tomorrow that
//     ignores it is invisible from here.
//
// Entries are normalised again here, for the same reason EmailConfig's TLS
// is: a NotificationConfig built as a struct literal never passed through
// NewNotificationConfig.
func (c *NotificationConfig) Validate() error {
	var errs []error
	if c == nil {
		return upstream.NotInitialised("notification")
	}

	for i, ch := range c.Channels {
		if !slices.Contains(validChannels, strlist.Normalise(ch)) {
			errs = append(errs, fmt.Errorf("notification.channels[%d] is "+
				"not a channel (got %q): want %s, since any other "+
				"spelling is never enabled and nothing reports it",
				i,
				ch,
				alternatives(validChannels)))
		}
	}

	return errors.Join(errs...)
}

// String returns a loggable representation of NotificationConfig.
//
// The pointer receiver means fmt only picks this up for a *NotificationConfig.
// Printing a value copy (%v on NotificationConfig, not &NotificationConfig)
// bypasses it and dumps the struct fields directly.
//
// An empty list prints as "(none)" rather than as nothing, so a startup
// line says in words that no channel sends. The parentheses mark it as a
// description, as they do for every label String prints in place of a
// value: "none" is not a channel Validate accepts.
func (c *NotificationConfig) String() string {
	if c == nil {
		return nilLabel("NotificationConfig")
	}
	channels := strings.Join(c.Channels, ",")
	if len(c.Channels) == 0 {
		channels = noneLabel
	}

	return fmt.Sprintf(
		"Channels=%s",
		channels,
	)
}
