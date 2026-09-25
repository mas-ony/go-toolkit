package config

// Tests for section.go.
//
// SuppliedSections reads os.Environ() on every call, so any test of it is
// exposed to whatever the machine running the suite happens to export — a
// CI runner with APP_ENV set would switch the app section on. The tests
// here therefore route on prefixes no real deployment uses ("zqunit",
// "zqunit.inner") and assert only about those, which keeps them
// independent of the environment and safe to run in parallel.
//
// Behaviour that genuinely depends on the environment is tested in
// section_integration_test.go, where t.Setenv controls it.

import (
	"bytes"
	"testing"

	"github.com/spf13/viper"
)

// yamlViper builds a Viper holding the given YAML, the way a file-backed
// instance would after ReadInConfig.
func yamlViper(t *testing.T, doc string) *viper.Viper {
	t.Helper()
	v := viper.New()
	v.SetConfigType("yaml")
	if err := v.ReadConfig(bytes.NewBufferString(doc)); err != nil {
		t.Fatalf("ReadConfig: %v", err)
	}
	return v
}

// ----------------------------------------------------------------------------
// sectionOf
// ----------------------------------------------------------------------------

// The longest matching prefix wins, which is what keeps a parent section
// from claiming the sections nested inside it: fiber.limiter.max begins
// with both "fiber" and "fiber.limiter", and only the second is its
// section.
func TestSectionOfPrefersTheLongestPrefix(t *testing.T) {
	t.Parallel()
	prefixes := map[string]string{
		"fiber":   "fiber",
		"limiter": "fiber.limiter",
		"app":     "app",
	}

	for _, c := range []struct {
		key, sep, want string
	}{
		{"fiber.limiter.max", ".", "limiter"},
		{"fiber.app_name", ".", "fiber"},
		{"app.port", ".", "app"},
		// The environment spelling routes the same way on underscores.
		{"FIBER_LIMITER_MAX", "_", ""}, // prefixes here are lowercase
	} {
		if got := sectionOf(prefixes, c.key, c.sep); got != c.want {
			t.Errorf("sectionOf(%q) = %q, want %q", c.key, got, c.want)
		}
	}
}

// A prefix has to match on a SEPARATOR boundary. Without that, "fiber"
// would claim "fiberx.anything" and "app" would claim "application.name",
// switching a section on for a key that is not its own.
func TestSectionOfRequiresASeparatorBoundary(t *testing.T) {
	t.Parallel()
	prefixes := map[string]string{"app": "app", "fiber": "fiber"}

	for _, key := range []string{
		"application.name", "appx", "fiberx.max", "fibers",
	} {
		if got := sectionOf(prefixes, key, "."); got != "" {
			t.Errorf("sectionOf(%q) = %q, want no section", key, got)
		}
	}
}

// An exact match counts, so a bare "fiber:" block with nothing under it
// supplies the section rather than nothing. What it holds is a question
// for the section's own Validate.
func TestSectionOfCountsAnExactMatch(t *testing.T) {
	t.Parallel()
	prefixes := map[string]string{"fiber": "fiber"}

	if got := sectionOf(prefixes, "fiber", "."); got != "fiber" {
		t.Errorf("sectionOf(fiber) = %q, want fiber", got)
	}
}

// ----------------------------------------------------------------------------
// Supplied
// ----------------------------------------------------------------------------

// NIL MEANS EVERY SECTION. A nil Supplied did not come from
// SuppliedSections, so it has no record of what any source held and cannot
// justify skipping anything — it must validate in full.
func TestNilSuppliedReportsEverySectionConfigured(t *testing.T) {
	t.Parallel()
	var s Supplied
	for _, name := range []string{"app", "database", "anything"} {
		if !s.Configured(name) {
			t.Errorf("a nil Supplied reports %q as not configured", name)
		}
	}
}

func TestSuppliedReportsOnlyWhatWasSupplied(t *testing.T) {
	t.Parallel()
	s := Supplied{"app": true}
	if !s.Configured("app") {
		t.Error("app should be configured")
	}
	if s.Configured("database") {
		t.Error("database should not be configured")
	}
}

// ----------------------------------------------------------------------------
// SuppliedSections — the file half
// ----------------------------------------------------------------------------

func TestSuppliedSectionsRoutesFileKeysByLongestPrefix(t *testing.T) {
	t.Parallel()
	v := yamlViper(t, `
zqunit:
  name: parent
  inner:
    max: 3
`)
	sections := []Section{
		{Name: "outer", Prefix: "zqunit", Value: AbsentSection{}},
		{Name: "inner", Prefix: "zqunit.inner", Value: AbsentSection{}},
		{Name: "unused", Prefix: "zqunused", Value: AbsentSection{}},
	}

	got := SuppliedSections(v, sections)

	if !got["outer"] {
		t.Error("outer should be supplied by zqunit.name")
	}
	// zqunit.inner.max belongs to inner, not to outer.
	if !got["inner"] {
		t.Error("inner should be supplied by zqunit.inner.max")
	}
	if got["unused"] {
		t.Error("a section with no keys was reported as supplied")
	}
}

// SuppliedSections always returns a non-nil map, which is what makes the
// nil check in Configured a reliable signal that a value came from
// somewhere else.
func TestSuppliedSectionsNeverReturnsNil(t *testing.T) {
	t.Parallel()
	got := SuppliedSections(viper.New(), []Section{
		{Name: "zqempty", Prefix: "zqempty"},
	})
	if got == nil {
		t.Fatal("SuppliedSections returned nil")
	}
	if got.Configured("zqempty") {
		t.Error("an empty Viper supplied a section")
	}
}

// ----------------------------------------------------------------------------
// AbsentSection
// ----------------------------------------------------------------------------

// The pair that makes the substitution work: it validates clean, so a
// deployment is not refused for a section it does not use, and it prints
// what it is, so the startup log does not show that section's zero values
// as though somebody had chosen them.
func TestAbsentSection(t *testing.T) {
	t.Parallel()
	var s SectionConfig = AbsentSection{}

	if err := s.Validate(); err != nil {
		t.Errorf("Validate = %v, want nil", err)
	}
	if got := s.String(); got != "<not configured>" {
		t.Errorf("String = %q, want <not configured>", got)
	}
}

// ----------------------------------------------------------------------------
// NewViper
// ----------------------------------------------------------------------------

// The key replacer is half of the environment contract. Viper exposes no
// getter for it, so this checks the one observable effect that does not
// need a real environment variable: a key set through the override layer
// is still read back under its dotted name, i.e. NewViper did not break
// ordinary lookups while wiring the replacer.
//
// The environment half — that APP_PORT actually overrides app.port — is
// asserted against real variables in the integration suite.
func TestNewViperKeepsOrdinaryLookupsWorking(t *testing.T) {
	t.Parallel()
	v := NewViper()
	v.Set("zqunit.some_key", "value")

	if got := v.GetString("zqunit.some_key"); got != "value" {
		t.Errorf("GetString = %q, want value", got)
	}
}

// Each call returns an independent instance, so two services — or two
// tests — building configuration in one process cannot see each other's
// keys.
func TestNewViperReturnsIndependentInstances(t *testing.T) {
	t.Parallel()
	a, b := NewViper(), NewViper()
	a.Set("zqunit.only_in_a", "x")

	if b.IsSet("zqunit.only_in_a") {
		t.Error("a key set on one instance is visible on the other")
	}
}
