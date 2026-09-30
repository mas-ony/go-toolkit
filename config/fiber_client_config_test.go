package config

// Tests for fiber_client_config.go.
//
// What the section promises: the section is optional, a stated base URL is
// one httpclient.New accepts, no message or log line carries a credential
// written into it, retries cannot be negative, every no-retry entry is a
// method the client could send, and the log line tells the three no-retry
// settings apart.

import (
	"strings"
	"testing"

	"github.com/spf13/viper"
)

// clientBase is a configuration the client section accepts, which each
// rule case changes one key at a time.
var clientBase = map[string]any{
	"fiber.client.base_url": "https://api.example",
	"fiber.client.retries":  2,
}

// buildClient adapts NewClientConfig to the constructor shape runRules
// takes.
func buildClient(v *viper.Viper) SectionConfig { return NewClientConfig(v) }

// TestClientValidate holds the section's rules. An absent base URL is a
// setting — the section is optional — while a stated one has to be usable;
// retries cannot be negative; and every no-retry entry has to be a method
// the client could send.
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
		{"a path prefix is allowed", map[string]any{
			"fiber.client.base_url": "https://api.example/api/v1"}, ""},
		// The credential is the token's job, and the base URL is logged.
		{"userinfo", map[string]any{
			"fiber.client.base_url": "https://svc:pw@api.example"},
			"must not carry userinfo"},
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

// No message and no log line carries a credential written into the base
// URL, including the unparseable ones, whose url.Parse errors would
// otherwise quote it.
func TestClientNeverPrintsAUserinfoPassword(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		"https://svc:hunter2@api.example",
		"https://svc:hunter2 x@api.example",
		// A "/" ends the authority early, and url.Parse then reports the
		// password as an invalid port.
		"https://svc:hunter2/x@api.example",
	} {
		c := &ClientConfig{BaseURL: raw}
		err := c.Validate()
		if err == nil {
			t.Errorf("%q validated", raw)
			continue
		}
		if strings.Contains(err.Error(), "hunter2") {
			t.Errorf("the error quotes the password:\n%v", err)
		}
		if strings.Contains(c.String(), "hunter2") {
			t.Errorf("String prints the password: %s", c.String())
		}
	}
}

// NoRetryMethods has three settings and String names each one, because
// nil and a stated empty list both render as [] and mean opposite things.
func TestClientStringNamesTheNoRetrySetting(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		methods []string
		want    string
	}{
		{nil, "NoRetryMethods=POST (transport default)"},
		{[]string{}, "NoRetryMethods=none (every method is replayed)"},
		{[]string{"POST", "PATCH"}, "NoRetryMethods=POST,PATCH"},
	} {
		got := (&ClientConfig{NoRetryMethods: c.methods}).String()
		if !strings.Contains(got, c.want) {
			t.Errorf("String = %q, want it to contain %q", got, c.want)
		}
	}
}

// From the environment an empty value reads as unset, so a lone comma is
// how the empty list is written there: it reaches splitList's comma path
// and comes back stated but empty, which replays every method. Set puts a
// bare string where an environment variable would.
func TestClientALoneCommaReplaysEveryMethod(t *testing.T) {
	t.Parallel()
	v := viper.New()
	v.Set("fiber.client.no_retry_methods", ",")
	c := NewClientConfig(v)
	if c.NoRetryMethods == nil || len(c.NoRetryMethods) != 0 {
		t.Fatalf("NoRetryMethods = %#v, want a stated empty list",
			c.NoRetryMethods)
	}
	if got := c.String(); !strings.Contains(got,
		"NoRetryMethods=none (every method is replayed)") {
		t.Errorf("String = %q, want the empty list named", got)
	}
}
