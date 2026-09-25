package config

// Tests for fiber_requestid_config.go.

import (
	"testing"

	"github.com/spf13/viper"
)

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

func buildRequestID(v *viper.Viper) SectionConfig {
	return NewRequestIDConfig(v)
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
