package config

// Tests for fiber_requestid_config.go.
//
// What the section promises: the header is required and has to be a legal
// HTTP field name, and the log line reports the generator from the field
// rather than asserting the middleware's default.

import (
	"strings"
	"testing"

	"github.com/spf13/viper"
)

// buildRequestID adapts NewRequestIDConfig to the constructor shape
// runRules takes.
func buildRequestID(v *viper.Viper) SectionConfig {
	return NewRequestIDConfig(v)
}

// Every RFC 9110 token character is accepted and nothing else is: a space,
// a colon, a control byte or a non-ASCII rune would each split or end the
// header line.
func TestIsHTTPFieldName(t *testing.T) {
	t.Parallel()
	for _, s := range []string{"X-Request-ID", "X_Trace", "abc123",
		"!#$%&'*+-.^_`|~"} {
		if !isHTTPFieldName(s) {
			t.Errorf("isHTTPFieldName(%q) = false", s)
		}
	}
	// A space, a colon or a control byte would split or end the header.
	for _, s := range []string{"", "X Request", "X:ID", "X\r\nID",
		"(id)", "ü"} {
		if isHTTPFieldName(s) {
			t.Errorf("isHTTPFieldName(%q) = true", s)
		}
	}
}

// The header name reaches the wire verbatim, so it has to be a legal HTTP
// field name: a space or a colon in it would split or end the header on
// every response.
func TestRequestIDValidate(t *testing.T) {
	t.Parallel()
	runRules(t, map[string]any{"fiber.requestid.header": "X-Request-ID"},
		buildRequestID, []ruleCase{
			{"no header", map[string]any{
				"fiber.requestid.header": nil}, "fiber.requestid.header"},
			{"a space in the name", map[string]any{
				"fiber.requestid.header": "X Request ID"},
				"fiber.requestid.header"},
			{"a colon in the name", map[string]any{
				"fiber.requestid.header": "X-Request:ID"},
				"fiber.requestid.header"},
			{"another legal name", map[string]any{
				"fiber.requestid.header": "X-Correlation-ID"}, ""},
		})
}

// The generator is reported from the field rather than asserted, so a
// custom one assigned to the embedded Config shows up as such.
func TestRequestIDStringDerivesTheGenerator(t *testing.T) {
	t.Parallel()
	c := NewRequestIDConfig(withKeys(map[string]any{
		"fiber.requestid.header": "X-Request-ID"}, nil))
	if got := c.String(); !strings.Contains(got,
		"Generator=utils.SecureToken (middleware default)") {
		t.Errorf("String = %q, want the default generator named", got)
	}
	c.Generator = func() string { return "fixed" }
	if got := c.String(); !strings.Contains(got, "Generator=custom") {
		t.Errorf("String = %q, want Generator=custom", got)
	}
}
