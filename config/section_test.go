package config

// Tests for section.go: the machinery every section shares, and the
// contract every section is held to.
//
// SuppliedSections reads os.Environ() on every call, so any test of it is
// exposed to whatever the machine running the suite happens to export — a
// CI runner with APP_ENV set would switch the app section on. Its tests
// here therefore route on prefixes no real deployment uses ("zqunit",
// "zqunit.inner") and assert only about those, which keeps them
// independent of the environment and safe to run in parallel.
//
// Every section shares one shape — a constructor reading a Viper, a
// Validate, and a String safe to log — and each file's header makes the
// same promise: "N keys, and that is the whole section — NewXConfig below
// reads exactly these." A promise like that drifts without anyone
// noticing, because nothing reads a comment against the code it
// describes.
//
// So the contract tests check it for every section at once, by parsing
// the package's own source. A key added to a constructor without its
// header line, a header line for a key nothing reads, a count that does
// not match, and an environment spelling that does not follow the rule
// all fail here, naming the file. The section table they run over, and
// the rule runner every <name>_config_test.go shares, are declared here
// as well.
//
// Behaviour that genuinely depends on the environment is tested in
// section_integration_test.go, where t.Setenv controls it.

import (
	"bytes"
	"go/ast"
	"go/token"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/mas-ony/go-toolkit/internal/testutil"
	"github.com/spf13/viper"
)

// section describes one section for the table-driven tests below.
type section struct {
	// name is the section's type name: String on a nil receiver has to
	// print "<nil name>", and New<name> has to be its constructor.
	name string
	// build constructs the section from v.
	build func(v *viper.Viper) SectionConfig
	// nilValue is a typed nil of the section's pointer type, so a test can
	// reach its methods through a nil receiver.
	nilValue SectionConfig
}

// ruleCase is one rejection or acceptance a section's Validate is held to.
type ruleCase struct {
	// name labels the subtest.
	name string
	// overrides are the keys set on top of the base configuration; a nil
	// value removes the key instead, so a case can express "absent".
	overrides map[string]any
	// mention is a fragment the error must contain; empty means the case
	// must validate cleanly.
	mention string
}

var (
	// keyShape is a whole dotted key and nothing else. Error messages
	// contain keys too, but always with spaces around them.
	keyShape = regexp.MustCompile(`^[a-z0-9_]+(\.[a-z0-9_]+)+$`)

	// sectionLine names the section a header covers.
	sectionLine = regexp.MustCompile(`(?m)^The (\S+?)(?:\.\*)? section`)

	// tableRow is one key and its variable, on two lines.
	tableRow = regexp.MustCompile(`(?m)^\t(\S+)\n\t\t([A-Z0-9_]+)$`)

	// countWord is the "N keys, and that is the whole section" sentence.
	countWord = regexp.MustCompile(`(?m)^([A-Z][a-z]+(?:-[a-z]+)?) keys?,`)
)

// sections is every section in the package, one entry per *_config.go
// file. TestTheSectionTableMatchesTheSource fails when a constructor has
// no entry here, so a new section cannot escape the contract suite.
var sections = []section{
	{"AppConfig", asSection(NewAppConfig), (*AppConfig)(nil)},
	{"DatabaseConfig", asSection(NewDatabaseConfig), (*DatabaseConfig)(nil)},
	{"AuthConfig", asSection(NewAuthConfig), (*AuthConfig)(nil)},
	{"ClientConfig", asSection(NewClientConfig), (*ClientConfig)(nil)},
	{"FiberConfig", asSection(NewFiberConfig), (*FiberConfig)(nil)},
	{"JWTConfig", asSection(NewJWTConfig), (*JWTConfig)(nil)},
	{"LimiterConfig", asSection(NewLimiterConfig), (*LimiterConfig)(nil)},
	{"ListenConfig", asSection(NewListenConfig), (*ListenConfig)(nil)},
	{"RecoverConfig", asSection(NewRecoverConfig), (*RecoverConfig)(nil)},
	{"RequestIDConfig", asSection(NewRequestIDConfig),
		(*RequestIDConfig)(nil)},
	{"SessionConfig", asSection(NewSessionConfig), (*SessionConfig)(nil)},
	{"ZerologConfig", asSection(NewZerologConfig), (*ZerologConfig)(nil)},
	{"NotificationConfig", asSection(NewNotificationConfig),
		(*NotificationConfig)(nil)},
	{"EmailConfig", asSection(NewEmailConfig), (*EmailConfig)(nil)},
	{"WhatsAppConfig", asSection(NewWhatsAppConfig), (*WhatsAppConfig)(nil)},
	{"TelegramConfig", asSection(NewTelegramConfig), (*TelegramConfig)(nil)},
}

// numberWords maps each count word a header may open with, "Zero" to
// "Thirty-nine", to its value, so the stated count can be compared with
// the number of keys the header lists.
var numberWords = func() map[string]int {
	words := strings.Fields("Zero One Two Three Four Five Six Seven Eight " +
		"Nine Ten Eleven Twelve Thirteen Fourteen Fifteen Sixteen " +
		"Seventeen Eighteen Nineteen Twenty")
	m := map[string]int{}
	for i, w := range words {
		m[w] = i
	}
	for i, w := range []string{"One", "Two", "Three", "Four", "Five",
		"Six", "Seven", "Eight", "Nine"} {
		m["Twenty-"+strings.ToLower(w)] = 21 + i
		m["Thirty-"+strings.ToLower(w)] = 31 + i
	}
	m["Thirty"] = 30
	return m
}()

// asSection adapts a section's constructor to the shape the table above
// and runRules take. A function returning *AppConfig is not a function
// returning SectionConfig, since Go converts the result but not the
// function type, so every constructor goes through this one adapter rather
// than a wrapper of its own.
func asSection[T SectionConfig](
	newSection func(*viper.Viper) T,
) func(*viper.Viper) SectionConfig {
	return func(v *viper.Viper) SectionConfig { return newSection(v) }
}

// headerOf returns the comment block that opens two lines below the
// package clause, after one blank line, which is where each section file
// documents its keys. Below the clause rather than above it, because a
// comment directly above "package config" is package documentation, and
// doc.go alone supplies that.
func headerOf(f *ast.File, fset *token.FileSet) string {
	pkgLine := fset.Position(f.Name.End()).Line
	for _, g := range f.Comments {
		if fset.Position(g.Pos()).Line == pkgLine+2 {
			return g.Text()
		}
	}
	return ""
}

// sectionNames returns the literals f hands upstream.NotInitialised: the
// name its Validate reports a section that was never built under.
func sectionNames(f *ast.File) []*ast.BasicLit {
	var names []*ast.BasicLit
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		fn, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || fn.Sel.Name != "NotInitialised" {
			return true
		}
		if pkg, ok := fn.X.(*ast.Ident); !ok || pkg.Name != "upstream" {
			return true
		}
		for _, arg := range call.Args {
			if lit, ok := arg.(*ast.BasicLit); ok {
				names = append(names, lit)
			}
		}
		return true
	})
	return names
}

// keysRead collects every string literal that is a whole key under prefix
// and is neither half of a concatenation nor a name from sectionNames.
// That is the set of keys the code reads: a key reaches Viper as a literal
// argument, a key named in an error message is either spaced or split
// across a "+", and the section's own prefix, handed to
// upstream.NotInitialised, is no key at all.
func keysRead(f *ast.File, prefix string) map[string]bool {
	notKeys := map[*ast.BasicLit]bool{}
	for _, lit := range sectionNames(f) {
		notKeys[lit] = true
	}
	ast.Inspect(f, func(n ast.Node) bool {
		if b, ok := n.(*ast.BinaryExpr); ok && b.Op == token.ADD {
			for _, side := range []ast.Expr{b.X, b.Y} {
				if lit, ok := side.(*ast.BasicLit); ok {
					notKeys[lit] = true
				}
			}
		}
		return true
	})

	read := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING || notKeys[lit] {
			return true
		}
		s := strings.Trim(lit.Value, "`\"")
		if keyShape.MatchString(s) &&
			(s == prefix || strings.HasPrefix(s, prefix+".")) {
			read[s] = true
		}
		return true
	})
	return read
}

// withKeys returns a Viper holding base with overrides applied on top.
// A nil override deletes the key, so a case can express "absent".
func withKeys(base, overrides map[string]any) *viper.Viper {
	v := viper.New()
	for k, val := range base {
		if o, ok := overrides[k]; ok {
			if o != nil {
				v.Set(k, o)
			}
			continue
		}
		v.Set(k, val)
	}
	for k, val := range overrides {
		if _, inBase := base[k]; !inBase && val != nil {
			v.Set(k, val)
		}
	}
	return v
}

// runRules is the table runner every section's rule test shares.
//
// It first requires base to validate as it stands, so a case that fails
// is failing on its own override and not on a broken baseline. Each case
// then builds the section from base with its overrides applied, through
// the constructor, so the key name and the cast are exercised along with
// the rule, and checks the verdict: an empty mention must validate, any
// other must fail with an error containing it.
func runRules(t *testing.T, base map[string]any,
	build func(*viper.Viper) SectionConfig, cases []ruleCase) {
	t.Helper()

	if err := build(withKeys(base, nil)).Validate(); err != nil {
		t.Fatalf("the baseline itself does not validate: %v", err)
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			err := build(withKeys(base, c.overrides)).Validate()
			switch {
			case c.mention == "" && err != nil:
				t.Errorf("rejected: %v", err)
			case c.mention != "" && err == nil:
				t.Errorf("validated; want an error mentioning %q", c.mention)
			case c.mention != "" && !strings.Contains(err.Error(), c.mention):
				t.Errorf("error does not mention %q:\n%v", c.mention, err)
			}
		})
	}
}

// yamlViper builds a Viper holding the given YAML, the way a file-backed
// instance would after ReadInConfig. splitlist_test.go uses it too.
func yamlViper(t *testing.T, doc string) *viper.Viper {
	t.Helper()
	v := viper.New()
	v.SetConfigType("yaml")
	if err := v.ReadConfig(bytes.NewBufferString(doc)); err != nil {
		t.Fatalf("ReadConfig: %v", err)
	}
	return v
}

// The longest matching prefix wins, which is what keeps a parent section
// from claiming the sections nested inside it: fiber.limiter.max begins
// with both "fiber" and "fiber.limiter", and only the second is its
// section. The environment spelling routes the same way, on underscores
// against upper-case prefixes.
func TestSectionOfPrefersTheLongestPrefix(t *testing.T) {
	t.Parallel()
	keys := map[string]string{
		"fiber":   "fiber",
		"limiter": "fiber.limiter",
		"app":     "app",
	}
	envs := map[string]string{
		"fiber":   "FIBER",
		"limiter": "FIBER_LIMITER",
		"app":     "APP",
	}

	for _, c := range []struct {
		prefixes       map[string]string
		key, sep, want string
	}{
		{keys, "fiber.limiter.max", ".", "limiter"},
		{keys, "fiber.app_name", ".", "fiber"},
		{keys, "app.port", ".", "app"},
		{envs, "FIBER_LIMITER_MAX", "_", "limiter"},
		{envs, "FIBER_APP_NAME", "_", "fiber"},
		// Prefixes are compared exactly as given, so a variable name never
		// matches a prefix still in its dotted, lower-case spelling. That
		// is why SuppliedSections converts them first.
		{keys, "FIBER_LIMITER_MAX", "_", ""},
	} {
		if got := sectionOf(c.prefixes, c.key, c.sep); got != c.want {
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

// A map from SuppliedSections answers for exactly the sections it names.
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

// File keys reach the section with the longest matching prefix, so a
// nested block supplies the nested section and its parent's own keys
// supply the parent, and a section with no keys stays unsupplied.
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

// A prefix is matched in lower case, the case Viper reports file keys in,
// so one written with capitals still claims the keys under it rather than
// leaving its section to load as absent and skip validation.
func TestSuppliedSectionsIgnoresPrefixCase(t *testing.T) {
	t.Parallel()
	v := yamlViper(t, "zqcase:\n  key: x\n")
	got := SuppliedSections(v, []Section{
		{Name: "zqcase", Prefix: "ZQCase", Value: AbsentSection{}},
	})
	if !got["zqcase"] {
		t.Error("a prefix written with capitals did not claim its keys")
	}
}

// Keys registered in code count as supplied, as SuppliedSections warns: a
// default given with SetDefault, and a variable bound with BindEnv whether
// or not it is set, both switch their section on. The bound variable's
// name is one no machine exports, so the environment half cannot be what
// switched it on.
func TestSuppliedSectionsCountsKeysRegisteredInCode(t *testing.T) {
	t.Parallel()
	v := viper.New()
	v.SetDefault("zqdefault.key", "x")
	if err := v.BindEnv("zqbound.key", "ZQBOUND_NEVER_EXPORTED"); err != nil {
		t.Fatal(err)
	}

	got := SuppliedSections(v, []Section{
		{Name: "zqdefault", Prefix: "zqdefault", Value: AbsentSection{}},
		{Name: "zqbound", Prefix: "zqbound", Value: AbsentSection{}},
	})
	if !got["zqdefault"] {
		t.Error("a key given only a default did not supply its section")
	}
	if !got["zqbound"] {
		t.Error("a key bound to an unset variable did not supply its " +
			"section")
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

// EnvName spells a key the way NewViper's replacer and AutomaticEnv do:
// upper-cased, every dot an underscore, and every underscore already in a
// segment kept, so a variable name cannot say where a segment ended.
//
// That the variable it names is the one NewViper reads is asserted
// against real variables in the integration suite.
func TestEnvName(t *testing.T) {
	t.Parallel()
	for key, want := range map[string]string{
		"app.port":                         "APP_PORT",
		"database.max_open_conns":          "DATABASE_MAX_OPEN_CONNS",
		"fiber.trust_proxy_config.proxies": "FIBER_TRUST_PROXY_CONFIG_PROXIES",
		"Fiber.Limiter":                    "FIBER_LIMITER",
		"":                                 "",
	} {
		if got := EnvName(key); got != want {
			t.Errorf("EnvName(%q) = %q, want %q", key, got, want)
		}
	}
}

// TestEverySectionListsExactlyTheKeysItReads holds each header to the
// promise it makes: its count, its list and its spellings all have to
// agree with what the constructor reads. The section it names is also the
// one Validate names when the section was never built.
func TestEverySectionListsExactlyTheKeysItReads(t *testing.T) {
	t.Parallel()

	files := testutil.Glob(t, "*_config.go")
	if len(files) != len(sections) {
		t.Errorf("%d section files but %d sections in the test table — "+
			"a new section needs an entry in both", len(files),
			len(sections))
	}

	for _, path := range files {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			f, fset := testutil.ParseFile(t, path)
			header := headerOf(f, fset)
			if header == "" {
				t.Fatal("no header comment below the package clause, " +
					"after one blank line")
			}
			m := sectionLine.FindStringSubmatch(header)
			if m == nil {
				t.Fatal("the header does not name its section")
			}
			prefix := m[1]

			listed := map[string]bool{}
			for _, row := range tableRow.FindAllStringSubmatch(header, -1) {
				key, env := row[1], row[2]
				listed[key] = true
				want := EnvName(key)
				if env != want {
					t.Errorf("%s is listed as %s, but NewViper derives %s",
						key, env, want)
				}
			}
			if len(listed) == 0 {
				t.Fatal("the header lists no keys")
			}

			read := keysRead(f, prefix)
			for key := range listed {
				if !read[key] {
					t.Errorf("%s is in the header but nothing reads it "+
						"— dead weight no file or variable can supply", key)
				}
			}
			for key := range read {
				if !listed[key] {
					t.Errorf("%s is read but missing from the header", key)
				}
			}
			names := sectionNames(f)
			if len(names) == 0 {
				t.Error("Validate never reports the section as never " +
					"built through upstream.NotInitialised")
			}
			for _, lit := range names {
				if name := strings.Trim(lit.Value, "`\""); name != prefix {
					t.Errorf("Validate reports the section as %q, not by "+
						"its prefix %q", name, prefix)
				}
			}

			c := countWord.FindStringSubmatch(header)
			if c == nil {
				t.Fatal("the header does not state how many keys it has")
			}
			n, ok := numberWords[c[1]]
			if !ok {
				t.Fatalf("unrecognised count word %q", c[1])
			}
			if n != len(listed) {
				t.Errorf("the header says %s keys and lists %d",
					c[1], len(listed))
			}
		})
	}
}

// Every constructor is documented to never fail: an absent key comes back
// as a zero value for Validate to reject. So building every section from
// an empty Viper must not panic, and what it returns must be loggable.
func TestEverySectionConstructsFromNothing(t *testing.T) {
	t.Parallel()
	for _, s := range sections {
		t.Run(s.name, func(t *testing.T) {
			t.Parallel()
			got := s.build(viper.New())
			if got == nil {
				t.Fatal("the constructor returned nil")
			}
			if got.String() == "" {
				t.Error("String on a zero-valued section is empty")
			}
			// Validate may reject it — most sections have required keys —
			// but must not panic doing so.
			_ = got.Validate()
		})
	}
}

// Configured is the question a package asks before it refuses a section
// as absent, and every section that has it answers by one rule: a nil
// section and one no key reached report false, and a single setting is
// enough to report true. A section that gains the method fails here until
// present holds a case for it, so none of them answers untested.
func TestEveryConfiguredTellsAbsentFromPresent(t *testing.T) {
	t.Parallel()
	type configurable interface{ Configured() bool }
	present := map[string]configurable{
		"DatabaseConfig": &DatabaseConfig{Host: "db.internal"},
		"EmailConfig":    &EmailConfig{Async: true},
		"TelegramConfig": &TelegramConfig{Async: true},
	}
	for _, s := range sections {
		absent, ok := s.nilValue.(configurable)
		if !ok {
			continue
		}
		t.Run(s.name, func(t *testing.T) {
			t.Parallel()
			if absent.Configured() {
				t.Error("a nil section reports itself configured")
			}
			if s.build(viper.New()).(configurable).Configured() {
				t.Error("a section no key reached reports itself " +
					"configured")
			}
			one, ok := present[s.name]
			if !ok {
				t.Fatal("no one-setting case for this section")
			}
			if !one.Configured() {
				t.Error("a section with one setting reports itself absent")
			}
		})
	}
}

// A nil *T has to validate to an error rather than panic, and log as
// "<nil T>" rather than panic. A service building its validation list
// from sections it has not finished wiring reaches exactly this.
func TestEverySectionIsNilSafe(t *testing.T) {
	t.Parallel()
	for _, s := range sections {
		t.Run(s.name, func(t *testing.T) {
			t.Parallel()
			if err := s.nilValue.Validate(); err == nil {
				t.Error("a nil receiver validated")
			}
			if got, want := s.nilValue.String(),
				"<nil "+s.name+">"; got != want {
				t.Errorf("String = %q, want %q", got, want)
			}
		})
	}
}

// String is what every section is logged through on start, so no secret
// in this package may appear in it. A deployment ships its logs
// somewhere broader than its secrets are allowed to go.
func TestStringNeverPrintsASecret(t *testing.T) {
	t.Parallel()
	const secret = "s3cret-value-that-must-not-be-logged-0123456789"

	for _, c := range []struct {
		key   string
		build func(v *viper.Viper) SectionConfig
	}{
		{"database.password", asSection(NewDatabaseConfig)},
		{"fiber.client.token", asSection(NewClientConfig)},
		{"fiber.jwt.secret", asSection(NewJWTConfig)},
		{"notification.email.password", asSection(NewEmailConfig)},
		{"notification.telegram.token", asSection(NewTelegramConfig)},
	} {
		t.Run(c.key, func(t *testing.T) {
			t.Parallel()
			v := viper.New()
			v.Set(c.key, secret)
			got := c.build(v).String()
			if strings.Contains(got, secret) {
				t.Errorf("String prints the value of %s:\n%s", c.key, got)
			}
			// Nor any long run of it, which a truncating mask would leak.
			if strings.Contains(got, secret[:12]) {
				t.Errorf("String prints part of %s:\n%s", c.key, got)
			}
		})
	}
}

// The test table has to stay in step with the files, or a new section
// silently escapes every test above. This is checked by NAME, so an entry
// for a section that was renamed fails too.
func TestTheSectionTableMatchesTheSource(t *testing.T) {
	t.Parallel()

	var inSource []string
	for _, path := range testutil.Glob(t, "*_config.go") {
		f, _ := testutil.ParseFile(t, path)
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Recv != nil ||
				!strings.HasPrefix(fn.Name.Name, "New") ||
				!strings.HasSuffix(fn.Name.Name, "Config") {
				continue
			}
			inSource = append(inSource,
				strings.TrimPrefix(fn.Name.Name, "New"))
		}
	}

	var inTable []string
	for _, s := range sections {
		inTable = append(inTable, s.name)
	}
	slices.Sort(inSource)
	slices.Sort(inTable)
	if !slices.Equal(inSource, inTable) {
		t.Errorf("constructors in source:\n  %v\nsections in the test "+
			"table:\n  %v", inSource, inTable)
	}
}

// Each section file has its own test file. The contract suite above covers
// what every section shares; what one section enforces is tested beside it,
// in the file named for it, which is where the rest of this module keeps
// its tests. A section added without one fails here.
func TestEverySectionFileHasItsOwnTestFile(t *testing.T) {
	t.Parallel()
	for _, f := range testutil.Glob(t, "*_config.go") {
		test := strings.TrimSuffix(f, ".go") + "_test.go"
		if _, err := os.Stat(test); err != nil {
			t.Errorf("%s has no %s", f, test)
		}
	}
}

// A section file is named for its prefix with dots as underscores, which
// is how a reader finds fiber.limiter in fiber_limiter_config.go without
// opening every file. The type inside takes the last component alone —
// LimiterConfig, EmailConfig — and is not checked here, since the section
// table above already names every one.
func TestEverySectionFileIsNamedForItsPrefix(t *testing.T) {
	t.Parallel()
	for _, path := range testutil.Glob(t, "*_config.go") {
		// A header that names no section is reported by
		// TestEverySectionListsExactlyTheKeysItReads.
		m := sectionLine.FindStringSubmatch(
			headerOf(testutil.ParseFile(t, path)))
		if m == nil {
			continue
		}
		want := strings.ReplaceAll(m[1], ".", "_") + "_config.go"
		if path != want {
			t.Errorf("%s holds the %s.* section and belongs in %s",
				path, m[1], want)
		}
	}
}

// An integration file is named for the source file whose behaviour it
// exercises, like every other test file in this module. One named for the
// package instead — config_integration_test.go — pairs with nothing, and
// becomes a place tests accumulate without anyone deciding where they
// belong. Not every source file needs one; every one that exists needs its
// source file.
func TestEveryIntegrationFileNamesASourceFile(t *testing.T) {
	t.Parallel()
	for _, f := range testutil.Glob(t, "*_integration_test.go") {
		src := strings.TrimSuffix(f, "_integration_test.go") + ".go"
		if _, err := os.Stat(src); err != nil {
			t.Errorf("%s names no source file: there is no %s", f, src)
		}
	}
}
