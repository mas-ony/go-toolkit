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
// notification.whatsapp.* in notification_whatsapp_config.go and
// notification.email.* in notification_email_config.go. This key chooses
// which channels send; their sections say how.

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/viper"
)

// The channels notification.channels accepts. Each is also the name of
// the section that configures it, nested under notification.
const (
	ChannelWhatsApp = "whatsapp"
	ChannelEmail    = "email"
)

// validChannels is the allowlist checked by NotificationConfig.Validate.
//
// Checked because a typo here is INVISIBLE at runtime. Enabled answers
// false for a channel spelled "emial", so no mail is ever sent, and the
// only symptom is a recipient who never hears about anything.
var validChannels = map[string]struct{}{
	ChannelWhatsApp: {},
	ChannelEmail:    {},
}

// normaliseChannel is the form a channel is compared in.
func normaliseChannel(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// NotificationConfig chooses the channels notifications go out on.
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
// Async gives up no guarantee that sync provides, PROVIDED the application
// treats delivery as best-effort in both modes — every send error logged
// rather than surfaced to the caller, and never allowed to roll back the
// row. An application that does surface the error is making a different
// trade, and the async key is where it disappears.
type NotificationConfig struct {
	// Channels lists the channels in use: ChannelWhatsApp, ChannelEmail,
	// both, or neither. It is read as a YAML sequence or as a
	// comma-separated string alike, and NewNotificationConfig lowercases
	// and trims each entry. Empty means no channel sends, which is also
	// what a deployment without this section gets.
	Channels []string
}

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
func NewNotificationConfig(v *viper.Viper) *NotificationConfig {
	// Read through splitList, so NOTIFICATION_CHANNELS=whatsapp,email is
	// two channels rather than one named "whatsapp,email".
	raw := splitList(v, "notification.channels")
	channels := make([]string, len(raw))
	for i, ch := range raw {
		channels[i] = normaliseChannel(ch)
	}
	return &NotificationConfig{Channels: channels}
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
//     answer. The application asks at wiring time instead, where
//     email.New fails for a channel chosen without its section.
//   - Whether any service actually honours the choice. It is passed to each
//     notifying service by hand at wiring time; a path added tomorrow that
//     ignores it is invisible from here.
//
// Entries are normalised again here, for the same reason EmailConfig's TLS
// is: a NotificationConfig built as a struct literal never passed through
// NewNotificationConfig.
func (c *NotificationConfig) Validate() error {
	// errs is declared before the nil check only so the two statements read in
	// the same order in all Validate implementations; the nil check is what
	// must come first, since every line after it dereferences c.
	var errs []error
	if c == nil {
		return errors.New("notification config was not initialised")
	}

	for i, ch := range c.Channels {
		if _, ok := validChannels[normaliseChannel(ch)]; !ok {
			errs = append(errs, fmt.Errorf("notification.channels[%d] is "+
				"not a channel (got %q): want %s or %s, since any other "+
				"spelling is never enabled and nothing reports it",
				i,
				ch,
				ChannelWhatsApp,
				ChannelEmail))
		}
	}

	return errors.Join(errs...)
}

// Enabled reports whether channel is one of Channels, ignoring case and
// surrounding spaces. It is safe on a nil receiver, which enables nothing.
func (c *NotificationConfig) Enabled(channel string) bool {
	if c == nil {
		return false
	}
	want := normaliseChannel(channel)
	for _, ch := range c.Channels {
		if normaliseChannel(ch) == want {
			return true
		}
	}
	return false
}

// String returns a loggable representation of NotificationConfig.
//
// The pointer receiver means fmt only picks this up for a *NotificationConfig.
// Printing a value copy (%v on NotificationConfig, not &NotificationConfig)
// bypasses it and dumps the struct fields directly.
//
// An empty list prints as "none" rather than as nothing, so a startup line
// says in words that no channel sends.
func (c *NotificationConfig) String() string {
	if c == nil {
		return "<nil NotificationConfig>"
	}
	channels := strings.Join(c.Channels, ",")
	if len(c.Channels) == 0 {
		channels = "none"
	}

	return fmt.Sprintf(
		"Channels=%s",
		channels,
	)
}
