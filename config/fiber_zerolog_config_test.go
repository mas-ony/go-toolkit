package config

// Tests for fiber_zerolog_config.go.

import (
	"testing"

	fiberzerolog "github.com/gofiber/contrib/v3/zerolog"
	"github.com/rs/zerolog"
	"github.com/spf13/viper"
)

// fatal and panic are refused by name, because zerolog's Fatal exits and
// Panic panics — as a REQUEST log level, either would end the process on
// the first response in its class.
func TestParseLevel(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]zerolog.Level{
		"trace": zerolog.TraceLevel, "debug": zerolog.DebugLevel,
		"info": zerolog.InfoLevel, "warn": zerolog.WarnLevel,
		"error": zerolog.ErrorLevel, " WARN ": zerolog.WarnLevel,
	} {
		got, err := parseLevel(name)
		if err != nil || got != want {
			t.Errorf("parseLevel(%q) = (%v, %v), want %v", name, got,
				err, want)
		}
	}
	for _, name := range []string{"fatal", "panic", "loud", ""} {
		if _, err := parseLevel(name); err == nil {
			t.Errorf("parseLevel(%q) accepted", name)
		}
	}
}

var zerologBase = map[string]any{
	"fiber.zerolog.fields": "status,method,latency",
}

func buildZerolog(v *viper.Viper) SectionConfig { return NewZerologConfig(v) }

// Messages and levels are PER CLASS — success, client error, server
// error — so when either is set it must name exactly three. Two would
// leave a class with the middleware's default and nobody the wiser.
func TestZerologValidate(t *testing.T) {
	t.Parallel()
	runRules(t, zerologBase, buildZerolog, []ruleCase{
		// An empty list is not the middleware's default list; it is a
		// request log with no fields at all.
		{"no fields", map[string]any{
			"fiber.zerolog.fields": nil}, "fiber.zerolog.fields"},
		{"three messages", map[string]any{
			"fiber.zerolog.messages": "ok,client error,server error"}, ""},
		{"two messages", map[string]any{
			"fiber.zerolog.messages": "ok,failed"},
			"fiber.zerolog.messages"},
		{"a blank message", map[string]any{
			"fiber.zerolog.messages": []any{"ok", " ", "failed"}},
			"fiber.zerolog.messages"},
		{"three levels", map[string]any{
			"fiber.zerolog.levels": "info,warn,error"}, ""},
		{"two levels", map[string]any{
			"fiber.zerolog.levels": "info,error"},
			"fiber.zerolog.levels"},
		// Fatal exits the process after writing: as a REQUEST log level
		// the first response in that class would end the service.
		{"a fatal level", map[string]any{
			"fiber.zerolog.levels": "info,warn,fatal"}, "fatal"},
		{"an unknown level", map[string]any{
			"fiber.zerolog.levels": "info,warn,loud"},
			"fiber.zerolog.levels"},
	})
}

// The request-id field name follows the same casing the middleware uses
// for every other field, so a service writing its own lines under the
// same key stays joinable with the request log.
func TestZerologRequestIDField(t *testing.T) {
	t.Parallel()
	if got := NewZerologConfig(viper.New()).RequestIDField(); got !=
		fiberzerolog.FieldRequestID {
		t.Errorf("default = %q, want %q", got, fiberzerolog.FieldRequestID)
	}
	v := viper.New()
	v.Set("fiber.zerolog.fields_snake_case", true)
	if got := NewZerologConfig(v).RequestIDField(); got != "request_id" {
		t.Errorf("snake case = %q, want request_id", got)
	}
}

// WithLogger installs the logger on a copy, leaving the section to keep
// describing what was loaded.
func TestWithLoggerInstallsOnACopy(t *testing.T) {
	t.Parallel()
	z := NewZerologConfig(withKeys(zerologBase, nil))
	before := z.String()

	out := z.WithLogger(zerolog.Nop())
	if out.Logger == nil {
		t.Error("the returned config carries no logger")
	}
	if len(out.Fields) != 3 {
		t.Errorf("the copy has %d fields, want 3", len(out.Fields))
	}
	if z.String() != before {
		t.Error("WithLogger modified the section")
	}
}
