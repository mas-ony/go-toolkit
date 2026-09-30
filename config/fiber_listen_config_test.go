package config

// Tests for fiber_listen_config.go.
//
// What the section promises: a drain deadline that is stated and at least a
// second, a certificate and its key set together, a listener network and a
// TLS floor Fiber accepts, a socket mode that is a permission and a hint
// when it was written without its leading zero, no negative prefork
// setting, and under prefork a network it can bind and a master grace no
// shorter than the drain.

import (
	"strings"
	"testing"

	"github.com/spf13/viper"
)

// listenBase is a configuration the listen section accepts, which each
// rule case changes one key at a time.
var listenBase = map[string]any{
	"fiber.listen.shutdown_timeout": "10s",
}

// buildListen adapts NewListenConfig to the constructor shape runRules
// takes.
func buildListen(v *viper.Viper) SectionConfig { return NewListenConfig(v) }

// TestListenValidate holds the section's rules, each beside the value that
// satisfies it.
func TestListenValidate(t *testing.T) {
	t.Parallel()
	runRules(t, listenBase, buildListen, []ruleCase{
		{"no shutdown timeout", map[string]any{
			"fiber.listen.shutdown_timeout": nil},
			"fiber.listen.shutdown_timeout"},
		// 10 meant as seconds is 10ns, and a negative deadline has passed
		// before the drain begins: both drop every in-flight request.
		{"a bare-number shutdown timeout", map[string]any{
			"fiber.listen.shutdown_timeout": 10},
			"fiber.listen.shutdown_timeout must be at least 1s"},
		{"a negative shutdown timeout", map[string]any{
			"fiber.listen.shutdown_timeout": "-10s"},
			"fiber.listen.shutdown_timeout must be at least 1s"},
		// A certificate without its key, or the reverse, cannot serve TLS,
		// and Fiber does not say so: it serves plain HTTP instead.
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
		// Fiber panics at Listen on any version but 1.2 and 1.3.
		{"TLS 1.1", map[string]any{
			"fiber.listen.tls_min_version": 770},
			"fiber.listen.tls_min_version"},
		{"TLS 1.3", map[string]any{
			"fiber.listen.tls_min_version": 772}, ""},
		// Prefork binds through SO_REUSEPORT, which fasthttp implements
		// for tcp4 and tcp6 only.
		{"prefork on a dual-stack network", map[string]any{
			"fiber.listen.enable_prefork":                true,
			"fiber.listen.listener_network":              "tcp",
			"fiber.listen.prefork_shutdown_grace_period": "15s"},
			"tcp4 or tcp6"},
		// An unset grace is fasthttp's 5s, shorter than the 10s drain, so
		// the master would SIGKILL a worker that believes it has time.
		{"prefork with the default grace", map[string]any{
			"fiber.listen.enable_prefork": true},
			"fiber.listen.prefork_shutdown_grace_period"},
		{"prefork with a grace above the drain", map[string]any{
			"fiber.listen.enable_prefork":                true,
			"fiber.listen.prefork_shutdown_grace_period": "15s"}, ""},
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

// octalIntent recovers the octal mode a decimal number's digits spell, and
// declines when those digits are not an octal permission at all.
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
