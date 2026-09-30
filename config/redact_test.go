package config

// Tests for redact.go.
//
// What the helpers promise: a URL quoted in an error or a log line shows no
// userinfo, whatever characters the credential holds, and the cause
// url.Parse reports keeps its meaning while repeating no part of the input.

import (
	"net/url"
	"strings"
	"testing"
)

// Userinfo is masked in every shape a credential can take, including the
// ones that make url.Parse fail, and a URL without one is left alone.
func TestRedactUserinfo(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]string{
		"https://reports.example":        "https://reports.example",
		"https://reports.example/api":    "https://reports.example/api",
		"https://u:p@reports.example":    "https://***@reports.example",
		"https://u:p@api.example/api/v1": "https://***@api.example/api/v1",
		"https://u@api.example":          "https://***@api.example",
		// A password may itself contain "@", "/" or a space.
		"https://u:p@ss@api.example": "https://***@api.example",
		"https://u:p/ss@api.example": "https://***@api.example",
		"https://u:p ss@api.example": "https://***@api.example",
		// An "@" in the query takes the host with it: the conservative
		// direction, as redactUserinfo documents.
		"https://api.example/x?to=a@b": "https://***@b",
		// With no scheme there is nothing to keep in front of it.
		"u:p@api.example": "***@api.example",
		"":                "",
	} {
		if got := redactUserinfo(raw); got != want {
			t.Errorf("redactUserinfo(%q) = %q, want %q", raw, got, want)
		}
	}
}

// The cause url.Parse reports survives, and no part of the input does:
// neither the copy url.Error carries nor a fragment the cause quotes. The
// port cases are the ones worth having: a "/", "?" or "#" inside the
// password ends the authority early, and the port error that follows
// quotes the password itself.
func TestURLParseCauseDropsTheInput(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		raw    string // a URL url.Parse refuses
		secret string // the part of it that must not be repeated
		cause  string // what the message still has to say
	}{
		{"https://admin:hunter2 x@reports.example", "hunter2",
			"invalid userinfo"},
		{"https://admin:hunter2/x@reports.example", "hunter2",
			"invalid port"},
		{"https://admin:hunter2?x@reports.example", "hunter2",
			"invalid port"},
		{"https://admin:hunter2#x@reports.example", "hunter2",
			"invalid port"},
		{"https://admin:hu%zzter2@reports.example", "zz",
			"invalid URL escape"},
	} {
		_, err := url.Parse(c.raw)
		if err == nil {
			t.Errorf("url.Parse(%q) succeeded", c.raw)
			continue
		}
		got := urlParseCause(err).Error()
		if strings.Contains(got, c.secret) {
			t.Errorf("urlParseCause(%q) kept %q: %q", c.raw, c.secret, got)
		}
		if !strings.Contains(got, c.cause) {
			t.Errorf("urlParseCause(%q) lost the cause: %q", c.raw, got)
		}
	}
	if urlParseCause(nil) != nil {
		t.Error("urlParseCause(nil) is not nil")
	}
}
