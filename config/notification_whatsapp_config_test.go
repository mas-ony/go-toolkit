package config

// Tests for notification_whatsapp_config.go.
//
// The section has one boolean and no rule a boolean can break, so what is
// worth holding is that the key is read under its documented name and
// that both settings validate: an operator choosing either is making a
// trade, not a mistake.

import (
	"strings"
	"testing"

	"github.com/spf13/viper"
)

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

func TestWhatsAppValidatesEitherSetting(t *testing.T) {
	t.Parallel()
	for _, async := range []bool{true, false} {
		c := &WhatsAppConfig{Async: async}
		if err := c.Validate(); err != nil {
			t.Errorf("Async=%v refused: %v", async, err)
		}
	}
}

func TestWhatsAppString(t *testing.T) {
	t.Parallel()
	if got := (&WhatsAppConfig{Async: true}).String(); !strings.Contains(
		got, "Async=true") {
		t.Errorf("String = %q, want it to show Async=true", got)
	}
}
