package config

// Tests for fiber_limiter_config.go.

import (
	"testing"
	"time"

	"github.com/spf13/viper"
)

var limiterBase = map[string]any{
	"fiber.limiter.expiration": "1m",
	"fiber.limiter.max":        100,
	"fiber.limiter.strategy":   StrategyFixed,
}

func buildLimiter(v *viper.Viper) SectionConfig { return NewLimiterConfig(v) }

func TestLimiterValidate(t *testing.T) {
	t.Parallel()
	runRules(t, limiterBase, buildLimiter, []ruleCase{
		{"no expiration", map[string]any{
			"fiber.limiter.expiration": nil}, "fiber.limiter.expiration"},
		// Below a second the window rounds to nothing in the storage the
		// middleware keys it by.
		{"a sub-second window", map[string]any{
			"fiber.limiter.expiration": "500ms"},
			"fiber.limiter.expiration"},
		{"no maximum", map[string]any{
			"fiber.limiter.max": nil}, "fiber.limiter.max"},
		{"a negative maximum", map[string]any{
			"fiber.limiter.max": -5}, "fiber.limiter.max"},
		// Required, not defaulted: an empty strategy would install the
		// middleware's own fixed window anyway — a counting rule that
		// neither the file nor the startup log would show anyone.
		{"no strategy", map[string]any{
			"fiber.limiter.strategy": nil}, "fiber.limiter.strategy"},
		{"an unknown strategy", map[string]any{
			"fiber.limiter.strategy": "leaky"}, "fiber.limiter.strategy"},
		{"fixed", map[string]any{
			"fiber.limiter.strategy": StrategyFixed}, ""},
		{"sliding", map[string]any{
			"fiber.limiter.strategy": StrategySliding}, ""},
		// Skipping both counts nothing, which is a limiter that never
		// limits.
		{"skipping both kinds of request", map[string]any{
			"fiber.limiter.skip_failed_requests":     true,
			"fiber.limiter.skip_successful_requests": true},
			"fiber.limiter.skip"},
	})
}

// The strategy name has to survive the round trip through the
// middleware's own handler type, which is how Validate confirms the
// strategy it asked for is the one that was installed.
func TestLimiterStrategyRoundTrip(t *testing.T) {
	t.Parallel()
	for _, name := range []string{StrategyFixed, StrategySliding} {
		if got := strategyName(limiterMiddlewareFor(name)); got != name {
			t.Errorf("strategyName(limiterMiddlewareFor(%q)) = %q",
				name, got)
		}
	}
	if got := strategyName(nil); got != "" {
		t.Errorf("strategyName(nil) = %q, want empty", got)
	}
}

// Durations are read with GetDuration, so the file can say "1m" and the
// environment can say "60s" and both mean the same thing. This pins that
// the cast is the one in use, not a GetInt that would read "1m" as zero.
func TestDurationsAcceptGoDurationSyntax(t *testing.T) {
	t.Parallel()
	c := NewLimiterConfig(withKeys(limiterBase, map[string]any{
		"fiber.limiter.expiration": "90s"}))
	if c.Expiration != 90*time.Second {
		t.Errorf("Expiration = %v, want 90s", c.Expiration)
	}
}
