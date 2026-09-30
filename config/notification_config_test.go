package config

// Tests for notification_config.go.
//
// What the section promises: the key is read in every shape a list
// arrives in, a channel is recognised however it is capitalised, and a
// name that is no channel fails at startup instead of switching nothing
// on in silence.

import (
	"slices"
	"strings"
	"testing"

	"github.com/spf13/viper"
)

// The key reaches Viper as a YAML sequence from the file and as a string
// from the environment, and both read as the same normalised list. An
// absent key reads as an empty list: no channels, rather than an error.
func TestNotificationReadsChannels(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		val  any
		want []string
	}{
		{"a YAML sequence", []any{"WhatsApp", " email "},
			[]string{"whatsapp", "email"}},
		{"a comma-separated string", "whatsapp, EMAIL",
			[]string{"whatsapp", "email"}},
		{"a space-separated string", "whatsapp email",
			[]string{"whatsapp", "email"}},
		{"one channel", "email", []string{"email"}},
		{"absent", nil, []string{}},
	} {
		v := viper.New()
		if c.val != nil {
			v.Set("notification.channels", c.val)
		}
		got := NewNotificationConfig(v).Channels
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: Channels = %q, want %q", c.name, got, c.want)
		}
	}
}

// Every list of known channels validates, empty, repeated or oddly cased
// alike. Each unknown entry is reported by its index and its spelling, and
// the valid entries beside it are not reported at all.
func TestNotificationValidate(t *testing.T) {
	t.Parallel()
	for _, ok := range [][]string{
		nil,
		{},
		{ChannelWhatsApp},
		{ChannelEmail},
		{ChannelWhatsApp, ChannelEmail},
		{"email", "email"},
		{" EMAIL ", "WhatsApp"},
	} {
		if err := (&NotificationConfig{Channels: ok}).Validate(); err != nil {
			t.Errorf("Channels=%q refused: %v", ok, err)
		}
	}

	err := (&NotificationConfig{
		Channels: []string{"email", "sms", "emial", ""},
	}).Validate()
	if err == nil {
		t.Fatal("unknown channels validated")
	}
	for _, want := range []string{
		`notification.channels[1] is not a channel (got "sms")`,
		`notification.channels[2] is not a channel (got "emial")`,
		`notification.channels[3] is not a channel (got "")`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v\nwant it to report %s", err, want)
		}
	}
	if strings.Contains(err.Error(), "[0]") {
		t.Errorf("err = %v, reports the valid entry too", err)
	}
}

// Enabled matches by normalised name, and enables nothing on a nil or an
// empty configuration.
func TestNotificationEnabled(t *testing.T) {
	t.Parallel()
	var none *NotificationConfig
	if none.Enabled(ChannelEmail) {
		t.Error("a nil config enabled a channel")
	}

	c := &NotificationConfig{Channels: []string{" EMAIL "}}
	for channel, want := range map[string]bool{
		ChannelEmail:    true,
		" Email ":       true,
		ChannelWhatsApp: false,
		"":              false,
	} {
		if got := c.Enabled(channel); got != want {
			t.Errorf("Enabled(%q) = %t, want %t", channel, got, want)
		}
	}
	if (&NotificationConfig{}).Enabled(ChannelWhatsApp) {
		t.Error("an empty list enabled a channel")
	}
}

// No channels is logged as "none" in words, whether the list is nil or
// empty, so the startup line cannot be read as a list that failed to
// print.
func TestNotificationString(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		channels []string
		want     string
	}{
		{[]string{"whatsapp", "email"}, "Channels=whatsapp,email"},
		{nil, "Channels=none"},
		{[]string{}, "Channels=none"},
	} {
		got := (&NotificationConfig{Channels: c.channels}).String()
		if got != c.want {
			t.Errorf("String = %q, want %q", got, c.want)
		}
	}
}
