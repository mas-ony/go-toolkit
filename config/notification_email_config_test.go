package config

// Tests for notification_email_config.go.
//
// What the section promises: every required key reported by name; a port
// in range; a TLS mode from the allowlist, held to the two ports whose
// mode is fixed; a sender that parses as an address; credentials set as a
// pair, and sent in clear text to loopback alone; a timeout of at least a
// second; and a log line that says whether credentials are set without
// printing either of them.

import (
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
)

// emailBase is a configuration the email section accepts: a submission
// relay on 587 with STARTTLS and no login. Each rule case changes it one
// key at a time.
var emailBase = map[string]any{
	"notification.email.host":    "smtp.example.com",
	"notification.email.port":    587,
	"notification.email.tls":     "starttls",
	"notification.email.from":    "Example <noreply@example.com>",
	"notification.email.timeout": "30s",
}

// buildEmail adapts NewEmailConfig to the constructor shape runRules
// takes.
func buildEmail(v *viper.Viper) SectionConfig { return NewEmailConfig(v) }

// TestEmailValidate holds the section's rules, each beside the value that
// satisfies it: the required keys, the port range, the TLS modes and the
// two ports whose mode is fixed, the sender's shape, the credential pair,
// clear-text credentials only over loopback, and the timeout floor.
func TestEmailValidate(t *testing.T) {
	t.Parallel()
	runRules(t, emailBase, buildEmail, []ruleCase{
		{"no host", map[string]any{
			"notification.email.host": nil}, "notification.email.host"},
		{"no port", map[string]any{
			"notification.email.port": nil}, "notification.email.port"},
		{"a port out of range", map[string]any{
			"notification.email.port": 70000}, "between 1 and 65535"},
		{"a negative port", map[string]any{
			"notification.email.port": -25}, "between 1 and 65535"},
		{"no TLS mode", map[string]any{
			"notification.email.tls": nil},
			"notification.email.tls is required"},
		// "ssl" is what several frameworks call implicit TLS, and it is
		// refused rather than guessed at.
		{"an unknown TLS mode", map[string]any{
			"notification.email.tls": "ssl"},
			"notification.email.tls must be one of"},
		{"a TLS mode in capitals", map[string]any{
			"notification.email.tls": " STARTTLS "}, ""},
		{"465 without implicit TLS", map[string]any{
			"notification.email.port": 465}, "port 465"},
		{"465 with implicit TLS", map[string]any{
			"notification.email.port": 465,
			"notification.email.tls":  "implicit"}, ""},
		{"587 with implicit TLS", map[string]any{
			"notification.email.tls": "implicit"}, "port 587"},
		{"an internal relay on 25 in clear text", map[string]any{
			"notification.email.port": 25,
			"notification.email.tls":  "none"}, ""},
		// A misspelled mode reports itself and not a port mismatch.
		{"465 with a misspelled mode", map[string]any{
			"notification.email.port": 465,
			"notification.email.tls":  "implict"}, "must be one of"},
		{"no sender", map[string]any{
			"notification.email.from": nil},
			"notification.email.from is required"},
		{"a sender that is not an address", map[string]any{
			"notification.email.from": "noreply"},
			"notification.email.from is not an address"},
		{"a bare sender address", map[string]any{
			"notification.email.from": "noreply@example.com"}, ""},
		{"a username without a password", map[string]any{
			"notification.email.username": "app"}, "set together"},
		{"a password without a username", map[string]any{
			"notification.email.password": "s3cret"}, "set together"},
		{"credentials over TLS", map[string]any{
			"notification.email.username": "app",
			"notification.email.password": "s3cret"}, ""},
		{"credentials in clear text to a remote relay", map[string]any{
			"notification.email.port":     25,
			"notification.email.tls":      "none",
			"notification.email.username": "app",
			"notification.email.password": "s3cret"}, "clear text"},
		{"credentials in clear text over loopback", map[string]any{
			"notification.email.host":     "127.0.0.1",
			"notification.email.port":     25,
			"notification.email.tls":      "none",
			"notification.email.username": "app",
			"notification.email.password": "s3cret"}, ""},
		// Loopback is matched exactly, as the send path matches it, so a
		// capitalised spelling is refused though it reaches the same
		// interface.
		{"credentials in clear text to LOCALHOST", map[string]any{
			"notification.email.host":     "LOCALHOST",
			"notification.email.port":     25,
			"notification.email.tls":      "none",
			"notification.email.username": "app",
			"notification.email.password": "s3cret"}, "clear text"},
		{"no timeout", map[string]any{
			"notification.email.timeout": nil},
			"notification.email.timeout is required"},
		{"a bare-number timeout", map[string]any{
			"notification.email.timeout": 30}, "at least 1s"},
		{"a negative timeout", map[string]any{
			"notification.email.timeout": "-5s"}, "at least 1s"},
	})
}

// NewEmailConfig is documented as the authoritative list of the section's
// keys, so every one of them has to be read under the name the header
// comment gives it.
func TestNewEmailConfigReadsEveryKey(t *testing.T) {
	t.Parallel()
	v := viper.New()
	for key, val := range map[string]any{
		"notification.email.host":     "smtp.example.com",
		"notification.email.port":     465,
		"notification.email.tls":      "implicit",
		"notification.email.insecure": true,
		"notification.email.username": "app",
		"notification.email.password": "s3cret",
		"notification.email.from":     "App <app@example.com>",
		"notification.email.timeout":  "45s",
		"notification.email.async":    true,
	} {
		v.Set(key, val)
	}

	got := NewEmailConfig(v)
	want := &EmailConfig{
		Host:     "smtp.example.com",
		Port:     465,
		TLS:      "implicit",
		Insecure: true,
		Username: "app",
		Password: "s3cret",
		From:     "App <app@example.com>",
		Timeout:  45 * time.Second,
		Async:    true,
	}
	if *got != *want {
		t.Errorf("NewEmailConfig =\n%+v\nwant\n%+v", *got, *want)
	}
}

// Identifiers lose stray whitespace on the way in and credentials keep
// theirs, because a password may begin or end with a space.
func TestNewEmailConfigTrimsIdentifiersButNotCredentials(t *testing.T) {
	t.Parallel()
	v := viper.New()
	v.Set("notification.email.host", " smtp.example.com ")
	v.Set("notification.email.tls", " StartTLS ")
	v.Set("notification.email.from", " noreply@example.com ")
	v.Set("notification.email.username", " app ")
	v.Set("notification.email.password", " s3cret ")

	got := NewEmailConfig(v)
	if got.Host != "smtp.example.com" || got.TLS != "starttls" ||
		got.From != "noreply@example.com" {
		t.Errorf("identifiers not normalised: %+v", *got)
	}
	if got.Username != " app " || got.Password != " s3cret " {
		t.Errorf("credentials were edited: %q %q", got.Username,
			got.Password)
	}
}

// A struct literal never passes through NewEmailConfig, so Validate has to
// normalise what it compares: a padded loopback host is still loopback.
func TestEmailValidateNormalisesAStructLiteral(t *testing.T) {
	t.Parallel()
	c := &EmailConfig{
		Host:     " localhost ",
		Port:     25,
		TLS:      "None",
		Username: "app",
		Password: "s3cret",
		From:     "noreply@example.com",
		Timeout:  30 * time.Second,
	}
	if err := c.Validate(); err != nil {
		t.Errorf("rejected: %v", err)
	}
}

// Credentials are reported as present or not, and never printed — the
// username included, which is often the mailbox itself.
func TestEmailStringReportsCredentialsWithoutPrintingThem(t *testing.T) {
	t.Parallel()
	with := (&EmailConfig{Username: "app-user", Password: "pw-value"}).
		String()
	if !strings.Contains(with, "Auth=(set)") {
		t.Errorf("String = %q, want Auth=(set)", with)
	}
	for _, secret := range []string{"app-user", "pw-value"} {
		if strings.Contains(with, secret) {
			t.Errorf("String prints %q: %s", secret, with)
		}
	}
	if without := (&EmailConfig{}).String(); !strings.Contains(without,
		"Auth=(none)") {
		t.Errorf("String = %q, want Auth=(none)", without)
	}
}
