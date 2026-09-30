package config

// Tests for fiber_recover_config.go.
//
// What the section promises: the stack-trace flag is read under its
// documented key and accepted either way, and a stack-trace handler is
// installed on a copy, so the section keeps describing what was loaded.

import (
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/spf13/viper"
)

// The flag is read under its documented key, both ways.
func TestRecoverReadsTheStackTraceFlag(t *testing.T) {
	t.Parallel()
	for _, want := range []bool{true, false} {
		v := viper.New()
		v.Set("fiber.recover.enable_stack_trace", want)
		if got := NewRecoverConfig(v).EnableStackTrace; got != want {
			t.Errorf("EnableStackTrace = %v, want %v", got, want)
		}
	}
}

// A boolean has no invalid value, so both settings validate.
func TestRecoverValidatesEitherSetting(t *testing.T) {
	t.Parallel()
	for _, on := range []bool{true, false} {
		v := viper.New()
		v.Set("fiber.recover.enable_stack_trace", on)
		if err := NewRecoverConfig(v).Validate(); err != nil {
			t.Errorf("enable_stack_trace=%v refused: %v", on, err)
		}
	}
}

// The handler is installed on a COPY. The section keeps describing what
// was loaded — so a configuration logged at startup is still accurate —
// and the returned value carries the handler it was given along with
// every loaded setting.
func TestWithStackTraceHandlerInstallsOnACopy(t *testing.T) {
	t.Parallel()
	v := viper.New()
	v.Set("fiber.recover.enable_stack_trace", true)
	r := NewRecoverConfig(v)

	called := false
	out := r.WithStackTraceHandler(func(fiber.Ctx, any) { called = true })

	if out.StackTraceHandler == nil {
		t.Fatal("the returned config has no handler")
	}
	out.StackTraceHandler(nil, "boom")
	if !called {
		t.Error("the returned handler is not the one that was passed")
	}
	if !out.EnableStackTrace {
		t.Error("the copy lost EnableStackTrace")
	}
	if r.StackTraceHandler != nil {
		t.Error("the section itself was modified")
	}
}
