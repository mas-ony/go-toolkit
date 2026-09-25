//go:build integration

package config

// Integration tests for section.go: the environment contract NewViper
// establishes, and the environment half of SuppliedSections.
//
// Twelve section files spell their keys' environment variables, and none
// of those spellings works unless the Viper was built by NewViper. Until
// these tests, nothing held that up — and the contract fails invisibly: a
// Viper built without the key replacer ignores every documented variable,
// substitutes the file's value or a zero, and reports nothing.
//
// The app section is the vehicle here, because it is small and every
// deployment has one. What is under test is the wiring, which is the same
// for all thirteen sections; the contract suite in sections_test.go
// separately checks that each section's documented spellings follow the
// rule these tests prove.
//
// These set REAL process variables with t.Setenv and read a real
// config.yaml through NewViper, so each assertion is about what a
// deployment actually experiences. t.Setenv forbids t.Parallel, which is
// why they live apart from the unit suite: everything there can run
// concurrently, and nothing here can.
//
//	go test -tags integration -run Integration ./config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/viper"
)

// loadFile writes body to a config.yaml in a temporary directory and reads
// it through NewViper, the way a service would at startup.
func loadFile(t *testing.T, body string) *viper.Viper {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	v := NewViper()
	v.SetConfigFile(path)
	if err := v.ReadInConfig(); err != nil {
		t.Fatalf("ReadInConfig: %v", err)
	}
	return v
}

const baseFile = `
app:
  name: from-file
  version: 1.0.0
  env: production
  port: 8080
  location: UTC
  reports_base_url: https://file.example
`

// The documented variable overrides the file, key by key, and leaves the
// keys it does not name alone.
func TestIntegrationEnvironmentOverridesTheFile(t *testing.T) {
	t.Setenv("APP_PORT", "9191")
	t.Setenv("APP_NAME", "from-env")

	app := NewAppConfig(loadFile(t, baseFile))

	if app.Port != 9191 {
		t.Errorf("Port = %d, want 9191 from APP_PORT", app.Port)
	}
	if app.Name != "from-env" {
		t.Errorf("Name = %q, want from-env from APP_NAME", app.Name)
	}
	// Untouched keys still come from the file.
	if app.Version != "1.0.0" {
		t.Errorf("Version = %q, want the file's 1.0.0", app.Version)
	}
}

// The spelling rule on a key that already contains an underscore:
// app.reports_base_url becomes APP_REPORTS_BASE_URL. The replacer only
// turns DOTS into underscores, so the underscore inside the key segment
// survives and the two are indistinguishable in the variable name — which
// is exactly what the documented spelling says.
func TestIntegrationUnderscoredKeyHasTheDocumentedSpelling(t *testing.T) {
	t.Setenv("APP_REPORTS_BASE_URL", "https://env.example")

	app := NewAppConfig(loadFile(t, baseFile))
	if app.ReportsBaseURL != "https://env.example" {
		t.Errorf("ReportsBaseURL = %q, want the value of "+
			"APP_REPORTS_BASE_URL", app.ReportsBaseURL)
	}
}

// A key the file does not mention at all is still read from the
// environment. This is the case AllKeys cannot see, and the reason
// SuppliedSections consults both sources.
func TestIntegrationEnvironmentSuppliesAKeyTheFileLacks(t *testing.T) {
	t.Setenv("APP_UPLOAD_DIR", "/srv/uploads")

	app := NewAppConfig(loadFile(t, baseFile))
	if app.UploadDir != "/srv/uploads" {
		t.Errorf("UploadDir = %q, want /srv/uploads from the "+
			"environment", app.UploadDir)
	}
}

// An EMPTY variable is not a value. AllowEmptyEnv is off, so an
// exported-but-empty name leaves the file's value in place instead of
// blanking it — which is what lets a deployment template export every
// variable with an empty default without wiping the file.
func TestIntegrationAnEmptyVariableDoesNotBlankTheFile(t *testing.T) {
	t.Setenv("APP_NAME", "")

	app := NewAppConfig(loadFile(t, baseFile))
	if app.Name != "from-file" {
		t.Errorf("Name = %q, want the file's value — an empty "+
			"variable blanked it", app.Name)
	}
}

// The test NewViper exists for. A Viper built with viper.New() — the
// obvious thing to reach for — reads the file and ignores the environment
// entirely, with no error. If this ever starts passing the environment
// through, NewViper's reason to exist has changed and its documentation
// should say so.
func TestIntegrationAPlainViperIgnoresTheEnvironment(t *testing.T) {
	t.Setenv("APP_PORT", "9191")

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(baseFile), 0o600); err != nil {
		t.Fatal(err)
	}

	plain := viper.New()
	plain.SetConfigFile(path)
	if err := plain.ReadInConfig(); err != nil {
		t.Fatalf("ReadInConfig: %v", err)
	}

	if got := NewAppConfig(plain).Port; got != 8080 {
		t.Fatalf("a plain viper read Port = %d; the premise of NewViper "+
			"no longer holds", got)
	}

	// And through NewViper the same variable is honoured.
	if got := NewAppConfig(loadFile(t, baseFile)).Port; got != 9191 {
		t.Errorf("through NewViper Port = %d, want 9191", got)
	}
}

// A section supplied ONLY through the environment has to count as
// supplied, or a deployment that ships no config.yaml would load every
// section as absent and validate none of them.
func TestIntegrationSuppliedSectionsSeesTheEnvironment(t *testing.T) {
	t.Setenv("ZQINTEG_ONLY_IN_ENV", "yes")

	v := loadFile(t, baseFile)
	got := SuppliedSections(v, []Section{
		{Name: "app", Prefix: "app", Value: AbsentSection{}},
		{Name: "zqinteg", Prefix: "zqinteg", Value: AbsentSection{}},
		{Name: "zqnone", Prefix: "zqnone", Value: AbsentSection{}},
	})

	if !got["app"] {
		t.Error("app should be supplied by the file")
	}
	if !got["zqinteg"] {
		t.Error("zqinteg is supplied only by ZQINTEG_ONLY_IN_ENV and " +
			"was not seen")
	}
	if got["zqnone"] {
		t.Error("a section nothing supplied was reported as supplied")
	}
}

// And an empty variable does not switch a section on, for the same
// reason it does not blank a value: it is not evidence of anything.
func TestIntegrationAnEmptyVariableDoesNotSupplyASection(t *testing.T) {
	t.Setenv("ZQEMPTY_KEY", "")

	got := SuppliedSections(viper.New(), []Section{
		{Name: "zqempty", Prefix: "zqempty", Value: AbsentSection{}},
	})
	if got["zqempty"] {
		t.Error("an exported-but-empty variable switched a section on")
	}
}
