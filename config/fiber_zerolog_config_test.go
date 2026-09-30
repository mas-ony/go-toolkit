package config

// Tests for fiber_zerolog_config.go.
//
// What the section promises: a field list that is stated, the comma
// spelling of it from the environment, level names from a short allowlist
// that leaves out the ones that end the process or drop a class, per-class
// lists of exactly three when set, a logger installed on a copy, and a
// request-id key that matches the one the middleware writes.

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"slices"
	"testing"

	fiberzerolog "github.com/gofiber/contrib/v3/zerolog"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/requestid"
	"github.com/rs/zerolog"
	"github.com/spf13/viper"
)

// zerologBase is a configuration the zerolog section accepts, which each
// rule case changes one key at a time.
var zerologBase = map[string]any{
	"fiber.zerolog.fields": "status,method,latency",
}

// buildZerolog adapts NewZerologConfig to the constructor shape runRules
// takes.
func buildZerolog(v *viper.Viper) SectionConfig { return NewZerologConfig(v) }

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

// The comma spelling an operator reaches for in an environment variable is
// three fields, not one field named "status,method,latency" that the
// middleware would silently ignore. Set puts a bare string where an
// environment variable would.
func TestZerologFieldsFromEnvironment(t *testing.T) {
	t.Parallel()
	v := viper.New()
	v.Set("fiber.zerolog.fields", "status,method,latency")
	got := NewZerologConfig(v).Fields
	if !slices.Equal(got, []string{"status", "method", "latency"}) {
		t.Errorf("Fields = %q, want three fields", got)
	}
}

// RequestIDField's snake-case spelling mirrors a constant the middleware
// keeps unexported, so it is checked against a line the middleware really
// writes: requestid inside zerolog, one request, and the key read back out
// of the JSON.
func TestZerologRequestIDFieldMatchesTheMiddleware(t *testing.T) {
	t.Parallel()
	for _, snake := range []bool{false, true} {
		v := viper.New()
		v.Set("fiber.zerolog.fields", "requestId")
		v.Set("fiber.zerolog.fields_snake_case", snake)
		z := NewZerologConfig(v)

		var buf bytes.Buffer
		app := fiber.New()
		app.Use(fiberzerolog.New(z.WithLogger(zerolog.New(&buf))))
		app.Use(requestid.New())
		app.Get("/", func(c fiber.Ctx) error {
			return c.SendStatus(fiber.StatusNoContent)
		})

		resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/",
			nil))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()

		var line map[string]any
		if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
			t.Fatalf("snake=%t: the log line is not JSON: %v\n%s", snake,
				err, buf.String())
		}
		key := z.RequestIDField()
		if id, _ := line[key].(string); id == "" {
			t.Errorf("snake=%t: no %q field in the middleware's line:\n%s",
				snake, key, buf.String())
		}
	}
}
