package config

// Tests for notification_whatsapp_config.go.
//
// The section has one boolean and no rule a boolean can break, so what is
// worth holding is that the key is read under its documented name, that
// both settings validate, since an operator choosing either is making a
// trade rather than a mistake, and that the log line shows which one is
// in force.

import (
	"strings"
	"testing"

	"github.com/spf13/viper"
)

// The key is read under its documented name, both ways, and an absent one
// reads as synchronous.
func TestWhatsAppReadsAsync(t *testing.T) {
	t.Parallel()
	for _, want := range []bool{true, false} {
		v := viper.New()
		v.Set("notification.whatsapp.async", want)
		if got := NewWhatsAppConfig(v).Async; got != want {
			t.Errorf("Async = %v, want %v", got, want)
		}
	}
	if NewWhatsAppConfig(viper.New()).Async {
		t.Error("an absent key should read as synchronous")
	}
}

// Neither setting is refused: each is a trade, as the header says.
func TestWhatsAppValidatesEitherSetting(t *testing.T) {
	t.Parallel()
	for _, async := range []bool{true, false} {
		c := &WhatsAppConfig{Async: async}
		if err := c.Validate(); err != nil {
			t.Errorf("Async=%v refused: %v", async, err)
		}
	}
}

// The log line shows the setting, since it decides whether a failed send
// can reach the caller at all.
func TestWhatsAppString(t *testing.T) {
	t.Parallel()
	got := (&WhatsAppConfig{Async: true}).String()
	if !strings.Contains(got, "Async=true") {
		t.Errorf("String = %q, want it to show Async=true", got)
	}
}
