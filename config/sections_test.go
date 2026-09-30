package config

// Contract tests every section is held to.
//
// Every section shares one shape — a constructor reading a Viper, a
// Validate, and a String safe to log — and each file's header makes the
// same promise: "N keys, and that is the whole section — NewXConfig below
// reads exactly these." A promise like that drifts without anyone
// noticing, because nothing reads a comment against the code it
// describes.
//
// So the tests here check it for every section at once, by parsing the
// package's own source. A key added to a constructor without its header
// line, a header line for a key nothing reads, a count that does not
// match, and an environment spelling that does not follow the rule all
// fail here, naming the file.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

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
	{"AppConfig", func(v *viper.Viper) SectionConfig {
		return NewAppConfig(v)
	}, (*AppConfig)(nil)},
	{"DatabaseConfig", func(v *viper.Viper) SectionConfig {
		return NewDatabaseConfig(v)
	}, (*DatabaseConfig)(nil)},
	{"AuthConfig", func(v *viper.Viper) SectionConfig {
		return NewAuthConfig(v)
	}, (*AuthConfig)(nil)},
	{"ClientConfig", func(v *viper.Viper) SectionConfig {
		return NewClientConfig(v)
	}, (*ClientConfig)(nil)},
	{"FiberConfig", func(v *viper.Viper) SectionConfig {
		return NewFiberConfig(v)
	}, (*FiberConfig)(nil)},
	{"JWTConfig", func(v *viper.Viper) SectionConfig {
		return NewJWTConfig(v)
	}, (*JWTConfig)(nil)},
	{"LimiterConfig", func(v *viper.Viper) SectionConfig {
		return NewLimiterConfig(v)
	}, (*LimiterConfig)(nil)},
	{"ListenConfig", func(v *viper.Viper) SectionConfig {
		return NewListenConfig(v)
	}, (*ListenConfig)(nil)},
	{"RecoverConfig", func(v *viper.Viper) SectionConfig {
		return NewRecoverConfig(v)
	}, (*RecoverConfig)(nil)},
	{"RequestIDConfig", func(v *viper.Viper) SectionConfig {
		return NewRequestIDConfig(v)
	}, (*RequestIDConfig)(nil)},
	{"SessionConfig", func(v *viper.Viper) SectionConfig {
		return NewSessionConfig(v)
	}, (*SessionConfig)(nil)},
	{"ZerologConfig", func(v *viper.Viper) SectionConfig {
		return NewZerologConfig(v)
	}, (*ZerologConfig)(nil)},
	{"NotificationConfig", func(v *viper.Viper) SectionConfig {
		return NewNotificationConfig(v)
	}, (*NotificationConfig)(nil)},
	{"EmailConfig", func(v *viper.Viper) SectionConfig {
		return NewEmailConfig(v)
	}, (*EmailConfig)(nil)},
	{"WhatsAppConfig", func(v *viper.Viper) SectionConfig {
		return NewWhatsAppConfig(v)
	}, (*WhatsAppConfig)(nil)},
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

// keysRead collects every string literal that is a whole key under prefix
// and is not half of a concatenation. That is the set of keys the code
// reads: a key reaches Viper as a literal argument, and a key named in an
// error message is either spaced or split across a "+".
func keysRead(f *ast.File, prefix string) map[string]bool {
	concatenated := map[*ast.BasicLit]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		if b, ok := n.(*ast.BinaryExpr); ok && b.Op == token.ADD {
			for _, side := range []ast.Expr{b.X, b.Y} {
				if lit, ok := side.(*ast.BasicLit); ok {
					concatenated[lit] = true
				}
			}
		}
		return true
	})

	read := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING || concatenated[lit] {
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

// TestEverySectionListsExactlyTheKeysItReads holds each header to the
// promise it makes: its count, its list and its spellings all have to
// agree with what the constructor reads.
func TestEverySectionListsExactlyTheKeysItReads(t *testing.T) {
	t.Parallel()

	// go test runs a package's tests in its own directory, so the pattern
	// sees exactly this package's section files.
	files, err := filepath.Glob("*_config.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != len(sections) {
		t.Errorf("%d section files but %d sections in the test table — "+
			"a new section needs an entry in both", len(files),
			len(sections))
	}

	for _, path := range files {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, path, nil,
				parser.ParseComments)
			if err != nil {
				t.Fatal(err)
			}

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
				want := strings.ToUpper(strings.ReplaceAll(key, ".", "_"))
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
		{"database.password", func(v *viper.Viper) SectionConfig {
			return NewDatabaseConfig(v)
		}},
		{"fiber.client.token", func(v *viper.Viper) SectionConfig {
			return NewClientConfig(v)
		}},
		{"fiber.jwt.secret", func(v *viper.Viper) SectionConfig {
			return NewJWTConfig(v)
		}},
		{"notification.email.password", func(v *viper.Viper) SectionConfig {
			return NewEmailConfig(v)
		}},
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
//
// Each file is parsed on its own, as the other contract tests do. That
// also keeps this test off parser.ParseDir, which is deprecated because it
// ignores build tags.
func TestTheSectionTableMatchesTheSource(t *testing.T) {
	t.Parallel()

	files, err := filepath.Glob("*_config.go")
	if err != nil {
		t.Fatal(err)
	}
	var inSource []string
	for _, path := range files {
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
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
	files, err := filepath.Glob("*_config.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
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
	files, err := filepath.Glob("*_config.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range files {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		// A header that names no section is reported by
		// TestEverySectionListsExactlyTheKeysItReads.
		m := sectionLine.FindStringSubmatch(headerOf(f, fset))
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
	files, err := filepath.Glob("*_integration_test.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		src := strings.TrimSuffix(f, "_integration_test.go") + ".go"
		if _, err := os.Stat(src); err != nil {
			t.Errorf("%s names no source file: there is no %s", f, src)
		}
	}
}
