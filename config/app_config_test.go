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

// appBase is a configuration the app section accepts. Each rule case
// changes it one key at a time.
var appBase = map[string]any{
	"app.name":     "svc",
	"app.version":  "1.0.0",
	"app.env":      EnvProduction,
	"app.port":     8080,
	"app.location": "UTC",
}

// NewAppConfig is documented as the authoritative list of the section's
// keys, so every one of them has to be read — and under exactly the key the
// header comment names. A key that drifted would be dead weight that no
// file or variable could ever supply.
func TestNewAppConfigReadsEveryKey(t *testing.T) {
	t.Parallel()
	got := NewAppConfig(withKeys(map[string]any{
		"app.name":     "svc",
		"app.version":  "2.3.4",
		"app.env":      "staging",
		"app.host":     "127.0.0.1",
		"app.port":     9090,
		"app.location": "Asia/Jakarta",
	}, nil))
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

// TestAppValidate holds the section's rules, each beside the value that
// satisfies it. app.env is matched exactly, deliberately: logger.New
// treats every unrecognised value as production, so a typo that got
// through would silently turn debug logging off rather than failing at
// startup. app.port is required and held to 1..65535.
func TestAppValidate(t *testing.T) {
	t.Parallel()
	const env, port = "app.env", "app.port"
	const notAnEnv = "app.env must be one of"
	const outOfRange = "app.port must be between 1 and 65535"
	runRules(t, appBase, asSection(NewAppConfig), []ruleCase{
		{"development", map[string]any{env: EnvDevelopment}, ""},
		{"staging", map[string]any{env: EnvStaging}, ""},
		{"production", map[string]any{env: EnvProduction}, ""},
		{"an env capitalised", map[string]any{env: "Production"}, notAnEnv},
		{"an env in capitals", map[string]any{env: "PRODUCTION"}, notAnEnv},
		{"an env after a space", map[string]any{env: " production"},
			notAnEnv},
		{"an env abbreviated", map[string]any{env: "prod"}, notAnEnv},
		{"another abbreviated", map[string]any{env: "dev"}, notAnEnv},
		{"an empty env", map[string]any{env: ""}, notAnEnv},
		{"a zero port", map[string]any{port: 0}, "app.port is required"},
		{"a negative port", map[string]any{port: -1}, outOfRange},
		{"the lowest port", map[string]any{port: 1}, ""},
		{"the highest port", map[string]any{port: 65535}, ""},
		{"one past the highest", map[string]any{port: 65536}, outOfRange},
		{"far out of range", map[string]any{port: 70000}, outOfRange},
	})
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

// Zero is reported as "required" and not ALSO as out of range, the rule
// upstream.CheckPort states for the value an absent key reads as.
func TestAppValidateReportsAZeroPortOnce(t *testing.T) {
	t.Parallel()
	err := NewAppConfig(withKeys(appBase,
		map[string]any{"app.port": 0})).Validate()
	if err == nil || strings.Contains(err.Error(), "between") {
		t.Errorf("err = %v, want the port reported as required and "+
			"nothing more", err)
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
