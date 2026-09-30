package config

// The one reader shared by every list-valued key in this package. It is here
// rather than in a section file because the section files are meant to be
// the authoritative list of ONE section's keys apiece.

import (
	"strings"

	"github.com/spf13/viper"
)

// splitList reads a list key that may arrive as a YAML sequence or as a
// single comma-separated string, and returns the elements either way.
//
// Every list-valued key reads through it: fiber.client.no_retry_methods
// from fiber_client_config.go, fiber.request_methods and
// fiber.trust_proxy_config.proxies from fiber_config.go,
// fiber.zerolog.fields, fiber.zerolog.messages, and fiber.zerolog.levels
// from fiber_zerolog_config.go — the last of those through parseLevels —
// and notification.channels from notification_config.go.
//
// The two shapes come from the two sources. From config.yaml the value is a
// real sequence, arrives as []interface{}, and GetStringSlice casts it
// correctly. From the environment Viper hands back one bare string, and
// cast.ToStringSlice splits that on WHITESPACE — so the obvious spelling
//
//	FIBER_ZEROLOG_FIELDS=status,method,latency
//
// yields a single element "status,method,latency". Nothing catches it, and
// what that costs depends on the key. For fiber.zerolog.fields the
// middleware ignores names it does not recognise and Validate only rejects
// an empty list, so the service runs with none of the requested fields and
// no error anywhere. For fiber.request_methods it is louder and no clearer:
// len() is 1, so Fiber does NOT fall back to DefaultMethods, and the one
// method its router knows is a string no client sends. Registering the
// first GET route panics at startup with "add: invalid http method GET",
// naming neither the key nor the comma, and a request that reaches the
// router anyway is answered 501 Not Implemented. That is why this helper
// exists rather than a comment telling operators to use spaces.
//
// A comma-separated string written in config.yaml as a plain scalar takes
// the same path as one from the environment, so the file accepts that
// spelling too.
//
// Detection is by comma rather than by asking Viper where the value came
// from, because Viper does not expose that. GetString on a sequence fails
// its cast and returns "", which contains no comma, so a file value falls
// through to GetStringSlice untouched. A whitespace-separated environment
// value has no comma either and is handled by the same fallback.
//
// The comma path drops empty elements, and the whitespace fallback never
// produces one, so neither string spelling can smuggle in a "". A YAML
// sequence is passed through as written, though: an empty string written
// there as an element survives, and it is the section's Validate that has
// to judge it. An absent key yields no elements at all.
func splitList(v *viper.Viper, key string) []string {
	raw := v.GetString(key)
	if !strings.Contains(raw, ",") {
		return v.GetStringSlice(key)
	}

	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
