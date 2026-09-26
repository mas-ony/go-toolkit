package config

// The notification.whatsapp.* section of config.yaml.
//
// Keys in this section, with their environment-variable spellings:
//
//	notification.whatsapp.async
//		NOTIFICATION_WHATSAPP_ASYNC
//
// One key, and that is the whole section — NewWhatsAppConfig below reads
// exactly that one. The environment spelling holds only for a Viper built
// by NewViper; see the package documentation.
//
// Whether WhatsApp is used at all is notification.channels. Nothing here
// configures the whatsapp package itself, which pairs a device on its
// first run and keeps the credentials in its own store: this section only
// decides how the application calls it.

import (
	"errors"
	"fmt"

	"github.com/spf13/viper"
)

// WhatsAppConfig holds how WhatsApp notifications are sent.
type WhatsAppConfig struct {
	// Async hands each send to a detached background goroutine instead of
	// running it on the request that asked for it. The trade either
	// setting makes is the same on every channel, so it is described
	// once, on NotificationConfig.
	//
	// What is particular to WhatsApp is the bound. The whatsapp package
	// adds no deadline of its own, so the Context forms of its sends are
	// the only way to limit one from here, and a background send has no
	// request to end it. Give it a deadline:
	//
	//	ctx, cancel := context.WithTimeout(
	//		context.WithoutCancel(reqCtx), time.Minute)
	//	defer cancel()
	//	err := svc.SendTextContext(ctx, phone, message)
	//
	// Without one, a send that stalls holds its goroutine for as long as
	// whatsmeow's own deadlines allow.
	Async bool
}

// NewWhatsAppConfig reads WhatsAppConfig fields from the provided Viper
// instance.
//
// Never returns an error: an absent key comes back as false, which is a
// valid setting.
//
// This function is the authoritative list of keys the
// notification.whatsapp.* section supports. A key present in config.yaml
// but missing here is dead weight — Viper never looks it up, so neither
// the file nor an environment variable can supply it.
func NewWhatsAppConfig(v *viper.Viper) *WhatsAppConfig {
	return &WhatsAppConfig{
		Async: v.GetBool("notification.whatsapp.async"),
	}
}

// Validate returns a joined error for every invalid or missing
// WhatsAppConfig field.
//
// Deliberately unchecked:
//
//   - Whether a device is PAIRED, or WhatsApp reachable. Nothing here
//     connects. The whatsapp package reports an unpaired device itself,
//     as ErrNotPaired, and a session that has expired fails per send and
//     is logged: the provider is allowed to be down without taking this
//     service's startup with it.
//   - Async. Both settings are meaningful and a bool has no invalid
//     value, so there is nothing to reject beyond a receiver that was
//     never built. What each setting costs is on NotificationConfig.
//
// That accounts for all fields, so this method has no checks beyond the
// nil guard, and returns nil directly rather than joining an empty list.
//
// The other Validate implementations open with `var errs []error` above
// their nil check so that the two statements read in the same order
// everywhere; there is nothing here to append to it, and a
// declared-but-never-appended slice would take three paragraphs to
// explain what one return says.
func (c *WhatsAppConfig) Validate() error {
	if c == nil {
		return errors.New("whatsapp config was not initialised")
	}
	return nil
}

// String returns a loggable representation of WhatsAppConfig.
//
// The pointer receiver means fmt only picks this up for a *WhatsAppConfig.
// Printing a value copy (%v on WhatsAppConfig, not &WhatsAppConfig)
// bypasses it and dumps the struct fields directly.
func (c *WhatsAppConfig) String() string {
	if c == nil {
		return "<nil WhatsAppConfig>"
	}
	return fmt.Sprintf("Async=%t",
		c.Async,
	)
}
