package config

// Tests for fiber_listen_config.go.

import (
	"strings"
	"testing"

	"github.com/spf13/viper"
)

var listenBase = map[string]any{
	"fiber.listen.shutdown_timeout": "10s",
}

func buildListen(v *viper.Viper) SectionConfig { return NewListenConfig(v) }

func TestListenValidate(t *testing.T) {
	t.Parallel()
	runRules(t, listenBase, buildListen, []ruleCase{
		{"no shutdown timeout", map[string]any{
			"fiber.listen.shutdown_timeout": nil},
			"fiber.listen.shutdown_timeout"},
		// A certificate without its key, or the reverse, cannot serve TLS
		// and would otherwise surface only when the listener starts.
		{"a cert without a key", map[string]any{
			"fiber.listen.cert_file": "/tls/cert.pem"},
			"fiber.listen.cert"},
		{"a key without a cert", map[string]any{
			"fiber.listen.cert_key_file": "/tls/key.pem"},
			"fiber.listen.cert"},
		{"both is fine", map[string]any{
			"fiber.listen.cert_file":     "/tls/cert.pem",
			"fiber.listen.cert_key_file": "/tls/key.pem"}, ""},
		{"an unknown network", map[string]any{
			"fiber.listen.listener_network": "udp"},
			"fiber.listen.listener_network"},
		{"a mode above 0777", map[string]any{
			"fiber.listen.unix_socket_file_mode": 0o1000},
			"fiber.listen.unix_socket_file_mode"},
		{"negative recover threshold", map[string]any{
			"fiber.listen.prefork_recover_threshold": -1},
			"fiber.listen.prefork_recover_threshold"},
		{"negative recover interval", map[string]any{
			"fiber.listen.prefork_recover_interval": "-1s"},
			"fiber.listen.prefork_recover_interval"},
		{"negative grace period", map[string]any{
			"fiber.listen.prefork_shutdown_grace_period": "-1s"},
			"fiber.listen.prefork_shutdown_grace_period"},
	})
}

// A socket mode written in DECIMAL — 755 where 0o755 was meant — is the
// mistake the octal-intent hint exists for: YAML reads 755 as the number
// seven hundred and fifty-five, which is not the permission anyone
// wanted. The error has to say what was probably meant.
func TestListenValidateSuggestsTheOctalMode(t *testing.T) {
	t.Parallel()
	v := withKeys(listenBase, map[string]any{
		"fiber.listen.unix_socket_file_mode": 755})
	err := NewListenConfig(v).Validate()
	if err == nil {
		t.Fatal("a decimal 755 validated")
	}
	if !strings.Contains(err.Error(), "755") {
		t.Errorf("the error should suggest 0755:\n%v", err)
	}
}

func TestOctalIntent(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		got    uint32
		meant  uint32
		wanted bool
	}{
		{755, 0o755, true},
		{644, 0o644, true},
		{777, 0o777, true},
		{660, 0o660, true},
		// Digits an octal number cannot hold.
		{789, 0, false},
		// Octal, but above the largest permission.
		{1777, 0, false},
	} {
		meant, ok := octalIntent(c.got)
		if ok != c.wanted || (ok && meant != c.meant) {
			t.Errorf("octalIntent(%d) = (%#o, %v), want (%#o, %v)",
				c.got, meant, ok, c.meant, c.wanted)
		}
	}
}
