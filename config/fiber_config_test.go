package config

// Tests for fiber_config.go.
//
// Most of the section's thirty-six keys map straight onto the framework's
// own Config and have no rule to break. What Validate adds are four
// cross-checks between keys, each one a combination that starts cleanly
// and then misbehaves in a way nothing reports.

import (
	"strings"
	"testing"

	"github.com/spf13/viper"
)

func buildFiber(v *viper.Viper) SectionConfig { return NewFiberConfig(v) }

func TestFiberValidate(t *testing.T) {
	t.Parallel()
	runRules(t, map[string]any{}, buildFiber, []ruleCase{
		// fasthttp enforces this before the router, so every POST, PUT
		// and DELETE would answer 405 with no handler ever running.
		{"get_only", map[string]any{"fiber.get_only": true},
			"fiber.get_only"},
		// Trusting the proxy with nothing trusted reads the forwarded
		// headers from nobody.
		{"trust_proxy with nothing trusted", map[string]any{
			"fiber.trust_proxy": true}, "fiber.trust_proxy"},
		{"trust_proxy with loopback", map[string]any{
			"fiber.trust_proxy":                 true,
			"fiber.trust_proxy_config.loopback": true}, ""},
		{"trust_proxy with a named proxy", map[string]any{
			"fiber.trust_proxy":                true,
			"fiber.trust_proxy_config.proxies": "10.0.0.1"}, ""},
		// A proxy header is only read when the proxy is trusted.
		{"proxy_header without trust_proxy", map[string]any{
			"fiber.proxy_header": "X-Forwarded-For"},
			"fiber.proxy_header"},
		{"proxy_header with trust_proxy", map[string]any{
			"fiber.proxy_header":                "X-Forwarded-For",
			"fiber.trust_proxy":                 true,
			"fiber.trust_proxy_config.loopback": true}, ""},
		// The unmatched-route lookahead is built from a 64-bit method
		// mask and abandoned entirely above that.
		{"skip_unmatched_routes with 65 methods", map[string]any{
			"fiber.skip_unmatched_routes": true,
			"fiber.request_methods":       manyMethods(65)},
			"fiber.skip_unmatched_routes"},
		{"skip_unmatched_routes with 64 methods", map[string]any{
			"fiber.skip_unmatched_routes": true,
			"fiber.request_methods":       manyMethods(64)}, ""},
	})
}

// manyMethods builds a comma-separated list of n distinct method names,
// the form an environment variable would carry.
func manyMethods(n int) string {
	m := make([]string, n)
	for i := range m {
		m[i] = "M" + strings.Repeat("X", i/26) + string(rune('A'+i%26))
	}
	return strings.Join(m, ",")
}

// request_methods reads through splitList, so the comma spelling an
// operator reaches for in an environment variable yields the methods they
// meant. Without it, the router's only method would be the whole string
// and every request would answer 405.
func TestFiberRequestMethodsAcceptTheCommaForm(t *testing.T) {
	t.Parallel()
	v := viper.New()
	v.Set("fiber.request_methods", "GET,POST,PUT")
	got := NewFiberConfig(v).RequestMethods
	if strings.Join(got, "|") != "GET|POST|PUT" {
		t.Errorf("RequestMethods = %q, want [GET POST PUT]", got)
	}
}
