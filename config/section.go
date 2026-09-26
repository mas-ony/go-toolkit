package config

// The machinery every section shares and no single section owns: the
// interface a section satisfies, the list entry pairing a section with the
// key prefix its keys are nested behind, the presence tracking that lets a
// service load only the sections it uses, the placeholder standing in for
// the ones it does not, and the Viper constructor all of it assumes.
//
// The per-section files are the authoritative list of ONE section's keys
// apiece. Nothing about a particular section belongs here.

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/viper"
)

// NewViper returns a Viper instance configured the way every section in
// this package assumes.
//
// Two settings, and each is what one half of the documented contract rests
// on. The key replacer turns a dotted key into its environment spelling —
// app.reports_base_url becomes APP_REPORTS_BASE_URL — and AutomaticEnv makes
// an environment variable of that name override the file.
//
// A caller that builds its own instance with viper.New() loses both, and
// what it loses is invisible: every documented variable is ignored, the
// file's value or a zero stands in for it, and nothing reports that the
// override was never wired. That is why the configuration lives here rather
// than being left for each application to reproduce.
//
// Reading the file is left to the caller — SetConfigFile, then ReadInConfig
// — because where configuration lives is a deployment decision. AllowEmptyEnv
// is left at its default of off, so an exported-but-empty variable does not
// blank a value the file supplies; SuppliedSections relies on that too.
func NewViper() *viper.Viper {
	v := viper.New()
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()
	return v
}

// SectionConfig is what every configuration section implements, and
// requiring both halves in one interface is what lets an application
// validate and log its sections from a single list. A section that can be
// logged can be validated and the reverse, so neither use can carry an entry
// the other cannot.
type SectionConfig interface {
	fmt.Stringer
	Validate() error
}

// Section pairs a configuration section with the key it is logged under and
// the config.yaml prefix its keys are nested behind.
//
// Prefix is what routes a loaded key back to its section, and it belongs on
// this struct rather than in a second table beside the application's section
// list for the reason the whole design turns on: two lists drift, and a
// prefix that drifts moves a section from configured to absent without
// moving anything an operator can see.
type Section struct {
	Name   string
	Prefix string
	Value  SectionConfig
}

// Supplied names the sections some source actually supplied a key for.
//
// NIL MEANS EVERY SECTION. SuppliedSections always returns a map, so a nil
// one is evidence of a value that did not come from it — a test's literal, a
// caller's — which has no record of what any source held and therefore
// cannot justify skipping anything. A nil map reports every section as
// configured, so such a value is validated in full.
type Supplied map[string]bool

// sectionOf returns the section whose prefix is the longest one key begins
// with. sep separates a prefix from the rest of a key: "." for a Viper key,
// "_" for an environment variable name.
//
// An exact match counts, so a section written into config.yaml with nothing
// under it — a bare "fiber:" — supplies that section rather than nothing.
// The block is there, and what it holds is a question for the section's own
// Validate.
func sectionOf(prefixes map[string]string, key, sep string) string {
	name, longest := "", ""
	for section, prefix := range prefixes {
		if key != prefix && !strings.HasPrefix(key, prefix+sep) {
			continue
		}
		if len(prefix) > len(longest) {
			name, longest = section, prefix
		}
	}
	return name
}

// Configured reports whether some source supplied a key for the named
// section. See the type's own documentation for why a nil map means every
// section rather than none.
func (s Supplied) Configured(name string) bool {
	return s == nil || s[name]
}

// SuppliedSections names every section some source supplied at least one key
// for.
//
// Both sources are consulted, and both have to be. AllKeys covers the file
// and nothing else, because AutomaticEnv resolves a key when it is looked up
// rather than registering it — so by that measure a deployment that ships no
// config.yaml supplies nothing, and every section would load as absent.
//
// A key is routed to the section with the LONGEST matching prefix, which is
// what keeps a parent from claiming the sections nested inside it:
// fiber.limiter.max begins with both "fiber" and "fiber.limiter", and only
// the second one is its section, just as notification.email.host belongs
// to the email section and not to notification.
//
// The environment half is the same routing over the same prefixes in their
// environment spelling, and it is the WEAKER of the two, because it matches
// a variable NAME rather than a key this package reads. A DATABASE_URL
// exported for something else marks the database section supplied; the
// section is then validated and reports the keys it actually wants, which is
// loud and wrong rather than quiet and wrong. It also routes on underscores,
// where the file routes on dots — so a fiber key spelled listen_something,
// or a notification key spelled email_something or whatsapp_something,
// would be read as the nested section's from the environment and as the
// parent's from the file. Neither exists; the fix if one is added is to
// name it so it does not collide, because the environment spelling cannot
// represent the difference.
//
// That weakness is worth knowing before this package is lifted into a new
// deployment. Of the sections whose prefixes carry no fiber. component,
// app, database, and notification match the short, common spellings
// APP_*, DATABASE_*, and NOTIFICATION_*, so a variable another stack
// already exports switches one of them on. The channel sections are
// nested under notification to stay out of that: their variables are
// NOTIFICATION_EMAIL_* and NOTIFICATION_WHATSAPP_*, rather than the MAIL_*
// or SMTP_* that several frameworks export.
//
// An empty variable is not a value. AllowEmptyEnv is off, so Viper would not
// read one either, and treating it as evidence would make an exported-but-
// empty name switch a section on that nothing can then fill in.
func SuppliedSections(v *viper.Viper, sections []Section) Supplied {
	keys := make(map[string]string, len(sections))
	envs := make(map[string]string, len(sections))
	for _, s := range sections {
		keys[s.Name] = s.Prefix
		envs[s.Name] = strings.ToUpper(
			strings.ReplaceAll(s.Prefix, ".", "_"))
	}

	supplied := make(Supplied, len(sections))
	for _, key := range v.AllKeys() {
		if name := sectionOf(keys, key, "."); name != "" {
			supplied[name] = true
		}
	}
	for _, entry := range os.Environ() {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || value == "" {
			continue
		}
		if section := sectionOf(envs, name, "_"); section != "" {
			supplied[section] = true
		}
	}
	return supplied
}

// AbsentSection stands in for a section no source supplied a key for.
//
// It validates clean and prints what it is, and that pair is the whole
// mechanism: ONE substitution in the application's section list both spares
// validation a section this deployment does not use and keeps the startup
// line from reporting that section's zero values as though somebody had
// chosen them. Neither the validation loop nor the logging needs to know the
// rule, which is what keeps them from disagreeing about it.
//
// The zero values a section holds when nothing supplied it are not worth
// printing: an expiration of 0s and a max of 0 read as settings somebody
// chose, and are the absence of any setting at all.
type AbsentSection struct{}

func (AbsentSection) String() string { return "<not configured>" }

func (AbsentSection) Validate() error { return nil }
