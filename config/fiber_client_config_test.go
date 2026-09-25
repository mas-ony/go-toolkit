package config

// Tests for fiber_client_config.go.

import (
	"testing"

	"github.com/spf13/viper"
)

var clientBase = map[string]any{
	"fiber.client.base_url": "https://api.example",
	"fiber.client.retries":  2,
}

func buildClient(v *viper.Viper) SectionConfig { return NewClientConfig(v) }

func TestClientValidate(t *testing.T) {
	t.Parallel()
	runRules(t, clientBase, buildClient, []ruleCase{
		{"no base URL is allowed", map[string]any{
			"fiber.client.base_url": nil}, ""},
		{"not a URL", map[string]any{
			"fiber.client.base_url": "://"}, "fiber.client.base_url"},
		{"another scheme", map[string]any{
			"fiber.client.base_url": "ftp://api.example"},
			"fiber.client.base_url"},
		{"no host", map[string]any{
			"fiber.client.base_url": "https://"}, "fiber.client.base_url"},
		{"negative retries", map[string]any{
			"fiber.client.retries": -1}, "fiber.client.retries"},
		{"a method nothing sends", map[string]any{
			"fiber.client.no_retry_methods": "POST,FETCH"},
			"fiber.client.no_retry_methods"},
		// Method names are matched case-insensitively, which is the only
		// forgiving comparison in this section.
		{"lower-case methods are accepted", map[string]any{
			"fiber.client.no_retry_methods": "post,put"}, ""},
	})
}
