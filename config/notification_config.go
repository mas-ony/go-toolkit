// notification_config.go covers the notification.* section of config.yaml.
//
// Keys in this section, with their environment-variable spellings:
//
//	notification.async
//		NOTIFICATION_ASYNC
//
// One key, and that is the whole section — NewNotificationConfig below reads
// exactly that one. See doc.go for how the environment spelling is derived and
// which tests hold it up.

package config

import (
	"errors"
	"fmt"

	"github.com/spf13/viper"
)

// NotificationConfig holds settings that control how outbound notifications
// are dispatched. It says nothing about the channel — email, SMS, a chat
// provider, a webhook — because the one decision it carries is the same
// whichever one an application wires up.
type NotificationConfig struct {
	// Async selects whether a notification is sent on the request goroutine
	// or handed off to a detached background goroutine.
	//
	//   false (synchronous) — the HTTP handler that creates or updates the
	//     record blocks until the send completes or fails. Saving is
	//     therefore at least as slow as the provider round-trip (network plus
	//     the provider's own servers), which is typically a second or more
	//     and can occasionally stall until it times out.
	//
	//   true (asynchronous) — the whole notification job runs in a background
	//     goroutine. The handler returns as soon as the database row is
	//     committed, so the user never waits on the provider. Delivery
	//     becomes fire-and-forget: a send that fails is logged but cannot be
	//     reflected in the HTTP response, and a notification still in flight
	//     when the process shuts down is dropped.
	//
	// Async gives up no guarantee that sync provides, PROVIDED the
	// application treats delivery as best-effort in both modes — every send
	// error logged rather than surfaced to the caller, and never allowed to
	// roll back the row. An application that does surface the error is making
	// a different trade, and this key is where it disappears.
	Async bool
}

// NewNotificationConfig reads NotificationConfig fields from the provided
// Viper instance.
//
// Never returns an error: absent keys and uncastable values both come back as
// zero values, which Validate rejects afterwards.
//
// This function is the authoritative list of keys the notification.* section
// supports. A key present in config.yaml but missing here is dead weight —
// Viper never looks it up, so neither the file nor an environment variable can
// supply it.
func NewNotificationConfig(v *viper.Viper) *NotificationConfig {
	return &NotificationConfig{
		Async: v.GetBool("notification.async"),
	}
}

// Validate returns a joined error for every invalid or missing
// NotificationConfig field.
//
// Deliberately unchecked:
//
//   - Whether the notification provider is REACHABLE. Nothing here dials it.
//     A session or credential that has expired fails per-send and is logged,
//     in both modes, which is the right place for it: the provider is allowed
//     to be down without taking this service's startup with it.
//   - Whether any service actually honours the flag. It is passed to each
//     notifying service by hand at wiring time; a path added tomorrow that
//     ignores it is invisible from here.
//   - Async. Both settings are meaningful and a bool has no invalid value, so
//     there is nothing to reject beyond a receiver that was never built. What
//     true COSTS is real — a send still in flight at shutdown is dropped, and
//     a failure cannot be reflected in the HTTP response — but that is an
//     operator's decision, and the trade each setting makes lives on the
//     Async field above, where it can be read by someone about to change it.
//
// That accounts for all fields, so this method has no checks beyond the nil
// guard, and returns nil directly rather than joining an empty list.
//
// The other Validate implementations open with `var errs []error` above their
// nil check so that the two statements read in the same order everywhere;
// there is nothing here to append to it, and a declared-but-never-appended
// slice would take three paragraphs to explain what one return says.
func (c *NotificationConfig) Validate() error {
	if c == nil {
		return errors.New("notification config was not initialised")
	}
	return nil
}

// String returns a loggable representation of NotificationConfig.
//
// The pointer receiver means fmt only picks this up for a *NotificationConfig.
// Printing a value copy (%v on NotificationConfig, not &NotificationConfig)
// bypasses it and dumps the struct fields directly.
func (c *NotificationConfig) String() string {
	if c == nil {
		return "<nil NotificationConfig>"
	}
	return fmt.Sprintf("Async=%t",
		c.Async,
	)
}
