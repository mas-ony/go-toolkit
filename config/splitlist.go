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
// no error anywhere. For fiber.request_methods it is worse: len() is 1, so
// Fiber does NOT fall back to DefaultMethods — it builds a router whose only
// registered method is a string no client will ever send, and answers 405 to
// everything. That is why this helper exists rather than a comment telling
// operators to use spaces.
//
// Detection is by comma rather than by asking Viper where the value came
// from, because Viper does not expose that. GetString on a sequence fails
// its cast and returns "", which contains no comma, so a file value falls
// through to GetStringSlice untouched. A whitespace-separated environment
// value has no comma either and is handled by the same fallback.
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
