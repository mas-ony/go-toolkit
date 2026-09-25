package config

// Tests for app_config.go.

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
		"app.name":             "svc",
		"app.version":          "2.3.4",
		"app.env":              "staging",
		"app.host":             "127.0.0.1",
		"app.port":             9090,
		"app.location":         "Asia/Jakarta",
		"app.reports_base_url": "https://reports.example",
		"app.upload_dir":       "/data/uploads",
	} {
		v.Set(key, val)
	}

	got := NewAppConfig(v)
	want := &AppConfig{
		Name:           "svc",
		Version:        "2.3.4",
		Env:            "staging",
		Host:           "127.0.0.1",
		Port:           9090,
		Location:       "Asia/Jakarta",
		ReportsBaseURL: "https://reports.example",
		UploadDir:      "/data/uploads",
	}
	if *got != *want {
		t.Errorf("NewAppConfig =\n%+v\nwant\n%+v", *got, *want)
	}
}

// The base URL is normalised on the way in, so two spellings of the same
// upstream reach Validate, String and a consumer as one string.
func TestNewAppConfigTrimsTheBaseURL(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		"https://reports.example",
		"https://reports.example/",
		"  https://reports.example//  ",
	} {
		v := viper.New()
		v.Set("app.reports_base_url", raw)
		if got := NewAppConfig(v).ReportsBaseURL; got !=
			"https://reports.example" {
			t.Errorf("%q normalised to %q", raw, got)
		}
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

func TestAppValidateAcceptsAValidConfig(t *testing.T) {
	t.Parallel()
	if err := validApp().Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

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

// The base URL is checked for SHAPE, because a consumer builds the
// upstream URL by concatenation and anything past the authority is
// silently prepended to every forwarded request.
func TestAppValidateBaseURLShape(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, url string
		ok        bool
	}{
		{"unset is fine", "", true},
		{"scheme and host", "https://reports.example", true},
		{"with a port", "http://reports.example:5019", true},
		{"no scheme", "reports.example", false},
		{"another scheme", "ftp://reports.example", false},
		{"no host", "https://", false},
		{"a path", "https://reports.example/api", false},
		{"a query", "https://reports.example?x=1", false},
		{"a fragment", "https://reports.example#top", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := validApp()
			cfg.ReportsBaseURL = c.url
			err := cfg.Validate()
			if c.ok && err != nil {
				t.Errorf("%q refused: %v", c.url, err)
			}
			if !c.ok && err == nil {
				t.Errorf("%q validated", c.url)
			}
		})
	}
}

// A credential in the base URL is refused, and — unlike every other
// branch — the offending value is NOT echoed. A service hands a
// validation error straight to a fatal log call, so echoing it would copy
// the password into exactly the place the check exists to keep it out of.
func TestAppValidateRefusesUserinfoWithoutEchoingIt(t *testing.T) {
	t.Parallel()
	const secret = "hunter2-do-not-log"

	cfg := validApp()
	cfg.ReportsBaseURL = "https://admin:" + secret + "@reports.example"
	err := cfg.Validate()
	if err == nil {
		t.Fatal("a base URL carrying a password validated")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("the error quotes the password:\n%v", err)
	}
	if strings.Contains(err.Error(), "admin") {
		t.Errorf("the error quotes the username:\n%v", err)
	}
	// It does offer the corrected form, which drops the userinfo.
	if !strings.Contains(err.Error(), "https://reports.example") {
		t.Errorf("the error should offer the corrected form:\n%v", err)
	}
}

// Every field has to appear, SEPARATED from its neighbours. The original
// format string had no space between ReportsBaseURL and UploadDir, so the
// two ran together into one unreadable token in every startup log line.
func TestAppStringSeparatesEveryField(t *testing.T) {
	t.Parallel()
	c := &AppConfig{
		Name:           "svc",
		Version:        "1.0.0",
		Env:            EnvProduction,
		Host:           "127.0.0.1",
		Port:           8080,
		Location:       "UTC",
		ReportsBaseURL: "https://reports.example",
		UploadDir:      "/data/uploads",
	}
	got := c.String()

	for _, want := range []string{
		"Name=svc", "Version=1.0.0", "Env=production",
		"Host=127.0.0.1", "Port=8080", "Location=UTC",
		"ReportsBaseURL=https://reports.example",
		"UploadDir=/data/uploads",
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

func TestAppStringHandlesANilReceiver(t *testing.T) {
	t.Parallel()
	var c *AppConfig
	if got := c.String(); got != "<nil AppConfig>" {
		t.Errorf("String = %q, want <nil AppConfig>", got)
	}
}
