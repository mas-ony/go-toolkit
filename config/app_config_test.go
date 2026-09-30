package config

// Tests for app_config.go.
//
// What the section promises: every key read under its documented name,
// every problem reported at once, app.env matched exactly, an absent port
// reported once and an out-of-range one refused, and a log line whose
// fields stand apart.

import (
	"strings"
	"testing"

	"github.com/spf13/viper"
)

// validApp is an AppConfig that passes Validate, for tests that change one
// field at a time.
func validApp() *AppConfig {
	return &AppConfig{
		Name:     "svc",
		Version:  "1.0.0",
		Env:      EnvProduction,
		Port:     8080,
		Location: "UTC",
	}
}

// NewAppConfig is documented as the authoritative list of the section's
// keys, so every one of them has to be read — and under exactly the key the
// header comment names. A key that drifted would be dead weight that no
// file or variable could ever supply.
func TestNewAppConfigReadsEveryKey(t *testing.T) {
	t.Parallel()
	v := viper.New()
	for key, val := range map[string]any{
		"app.name":     "svc",
		"app.version":  "2.3.4",
		"app.env":      "staging",
		"app.host":     "127.0.0.1",
		"app.port":     9090,
		"app.location": "Asia/Jakarta",
	} {
		v.Set(key, val)
	}

	got := NewAppConfig(v)
	want := &AppConfig{
		Name:     "svc",
		Version:  "2.3.4",
		Env:      "staging",
		Host:     "127.0.0.1",
		Port:     9090,
		Location: "Asia/Jakarta",
	}
	if *got != *want {
		t.Errorf("NewAppConfig =\n%+v\nwant\n%+v", *got, *want)
	}
}

// Never an error: absent keys come back as zero values, which Validate
// rejects afterwards. Constructing from an empty Viper must not panic.
func TestNewAppConfigFromNothingIsAllZero(t *testing.T) {
	t.Parallel()
	got := NewAppConfig(viper.New())
	if *got != (AppConfig{}) {
		t.Errorf("NewAppConfig on an empty Viper = %+v, want zero", *got)
	}
}

// The fixture every other test changes one field of is itself valid, so a
// failure elsewhere is about the field that test changed.
func TestAppValidateAcceptsAValidConfig(t *testing.T) {
	t.Parallel()
	if err := validApp().Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

// A section that was never built is reported, not dereferenced.
func TestAppValidateRejectsANilReceiver(t *testing.T) {
	t.Parallel()
	var c *AppConfig
	if err := c.Validate(); err == nil {
		t.Error("a nil *AppConfig validated")
	}
}

// Every problem at once, so one restart surfaces the whole section.
func TestAppValidateReportsEveryMissingField(t *testing.T) {
	t.Parallel()
	err := (&AppConfig{}).Validate()
	if err == nil {
		t.Fatal("an empty AppConfig validated")
	}
	for _, want := range []string{
		"app.name", "app.version", "app.env", "app.port", "app.location",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error is missing %s:\n%v", want, err)
		}
	}
}

// Exact matching, deliberately: logger.New treats every unrecognised value
// as production, so a typo that got through would silently turn debug
// logging off rather than failing at startup.
func TestAppValidateMatchesEnvExactly(t *testing.T) {
	t.Parallel()
	for _, env := range []string{
		"Production", "PRODUCTION", " production", "prod", "dev", "",
	} {
		c := validApp()
		c.Env = env
		if err := c.Validate(); err == nil {
			t.Errorf("env %q validated; it should be refused", env)
		}
	}
	for _, env := range []string{
		EnvDevelopment, EnvStaging, EnvProduction,
	} {
		c := validApp()
		c.Env = env
		if err := c.Validate(); err != nil {
			t.Errorf("env %q refused: %v", env, err)
		}
	}
}

// Zero is reported as "required" and not ALSO as out of range, which is
// what the chained else-if is for.
func TestAppValidatePortBoundaries(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		port    int
		ok      bool
		mention string
	}{
		{0, false, "required"},
		{-1, false, "between 1 and 65535"},
		{1, true, ""},
		{65535, true, ""},
		{65536, false, "between 1 and 65535"},
		{70000, false, "between 1 and 65535"},
	} {
		cfg := validApp()
		cfg.Port = c.port
		err := cfg.Validate()

		if c.ok {
			if err != nil {
				t.Errorf("port %d refused: %v", c.port, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("port %d validated", c.port)
			continue
		}
		if !strings.Contains(err.Error(), c.mention) {
			t.Errorf("port %d: %v, want it to say %q",
				c.port, err, c.mention)
		}
		if c.port == 0 && strings.Contains(err.Error(), "between") {
			t.Error("a zero port was reported twice")
		}
	}
}

// Every field has to appear, SEPARATED from its neighbours. A missing space
// in the format string runs two fields together into one token on every
// startup log line, which is easy to write and hard to notice.
func TestAppStringSeparatesEveryField(t *testing.T) {
	t.Parallel()
	c := &AppConfig{
		Name:     "svc",
		Version:  "1.0.0",
		Env:      EnvProduction,
		Host:     "127.0.0.1",
		Port:     8080,
		Location: "UTC",
	}
	got := c.String()

	for _, want := range []string{
		"Name=svc", "Version=1.0.0", "Env=production",
		"Host=127.0.0.1", "Port=8080", "Location=UTC",
	} {
		// Each field must stand as its own whitespace-separated token.
		found := false
		for _, tok := range strings.Fields(got) {
			if tok == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%q is not a separate token in:\n%s", want, got)
		}
	}
}

// A nil section prints a placeholder instead of panicking inside the
// startup log line.
func TestAppStringHandlesANilReceiver(t *testing.T) {
	t.Parallel()
	var c *AppConfig
	if got := c.String(); got != "<nil AppConfig>" {
		t.Errorf("String = %q, want <nil AppConfig>", got)
	}
}
