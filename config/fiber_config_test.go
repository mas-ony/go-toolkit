package config

// Tests for fiber_config.go.
//
// Most of the section's thirty-six keys map straight onto the framework's
// own Config and have no rule to break. What Validate adds is one value
// refused outright (get_only), a shape check on each request_methods
// entry (an upper-case token), and three pairings between keys, each a
// combination that starts cleanly and then misbehaves in a way nothing
// reports.

import (
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/spf13/viper"
)

// buildFiber adapts NewFiberConfig to the constructor shape runRules takes.
func buildFiber(v *viper.Viper) SectionConfig { return NewFiberConfig(v) }

// manyMethods builds a comma-separated list of n distinct method names,
// the form an environment variable would carry.
func manyMethods(n int) string {
	m := make([]string, n)
	for i := range m {
		m[i] = "M" + strings.Repeat("X", i/26) + string(rune('A'+i%26))
	}
	return strings.Join(m, ",")
}

// TestFiberValidate holds the section's rules. The base is empty on
// purpose: every key here has a working default, so each case adds only
// the keys its rule is about.
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
		// A YAML sequence is never split, so two methods written as one
		// entry stay one entry, which can never match a request.
		{"two methods in one entry", map[string]any{
			"fiber.request_methods": []any{"GET", "PUT,POST"}},
			"fiber.request_methods[1]"},
		{"an empty method", map[string]any{
			"fiber.request_methods": []any{"GET", ""}},
			"fiber.request_methods[1]"},
		// Fiber upper-cases a route's method before the lookup, so a
		// lower-case entry is never found.
		{"a lower-case method", map[string]any{
			"fiber.request_methods": []any{"GET", "post"}},
			"fiber.request_methods[1] must be upper-case"},
		// Custom methods are what the field exists to add.
		{"a custom method", map[string]any{
			"fiber.request_methods": []any{"GET", "HEAD", "PURGE"}}, ""},
	})
}

// request_methods reads through splitList, so the comma spelling an
// operator reaches for in an environment variable yields the methods they
// meant. Without it, the router's only method would be the whole string,
// and the first route the service registered would panic at startup.
func TestFiberRequestMethodsAcceptTheCommaForm(t *testing.T) {
	t.Parallel()
	v := viper.New()
	v.Set("fiber.request_methods", "GET,POST,PUT")
	got := NewFiberConfig(v).RequestMethods
	if strings.Join(got, "|") != "GET|POST|PUT" {
		t.Errorf("RequestMethods = %q, want [GET POST PUT]", got)
	}
}

// An empty method list is Fiber's full default, so the log line says so
// in words rather than printing [], which reads as no methods at all.
func TestFiberStringNamesTheDefaultMethods(t *testing.T) {
	t.Parallel()
	got := NewFiberConfig(viper.New()).String()
	if !strings.Contains(got, "RequestMethods=(fiber.DefaultMethods)") {
		t.Errorf("String = %q, want the default method list named", got)
	}
	v := viper.New()
	v.Set("fiber.request_methods", "GET,HEAD")
	if got := NewFiberConfig(v).String(); !strings.Contains(got,
		"RequestMethods=[GET HEAD]") {
		t.Errorf("String = %q, want the stated list", got)
	}
}

// The premise both request_methods checks rest on, held against Fiber
// itself: registering a route for a method the list does not name panics,
// whether the entry meant to hold it is two methods run together or the
// right method in lower case.
func TestFiberPanicsOnARouteOutsideRequestMethods(t *testing.T) {
	t.Parallel()
	for _, methods := range [][]string{{"GET,POST"}, {"get", "post"}} {
		app := fiber.New(fiber.Config{RequestMethods: methods})
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("RequestMethods=%q: registering a GET route "+
						"did not panic", methods)
				}
			}()
			app.Get("/", func(fiber.Ctx) error { return nil })
		}()
	}
}
