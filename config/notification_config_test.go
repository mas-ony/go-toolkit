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
		if got := NewNotificationConfig(v).Channels; !slices.Equal(got,
			c.want) {
			t.Errorf("%s: Channels = %q, want %q", c.name, got, c.want)
		}
	}
}

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
