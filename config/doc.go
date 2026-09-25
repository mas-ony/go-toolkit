// Package config reads a service's configuration into typed sections, one
// per concern, and validates each before the service starts.
//
// Every section is a struct, a constructor that reads it out of a Viper
// instance, a Validate that reports everything wrong with it at once, and a
// String safe to log. The sections are independent: a service loads the
// ones it uses and none of the others.
//
//	v := config.NewViper()
//	v.SetConfigFile("config.yaml")
//	if err := v.ReadInConfig(); err != nil {
//		return err
//	}
//
//	app := config.NewAppConfig(v)
//	db := config.NewDatabaseConfig(v)
//	for _, s := range []config.SectionConfig{app, db} {
//		if err := s.Validate(); err != nil {
//			return err
//		}
//	}
//
// # One file per section, and each file is the whole list
//
// Each <name>_config.go file is the authoritative list of ONE section's
// keys. Its constructor reads exactly those keys and nothing else, so a key
// present in config.yaml but absent from the constructor is dead weight:
// Viper never looks it up, and neither the file nor an environment variable
// can supply it. The header comment of every file lists its keys beside
// their environment spelling, and that list and the constructor move
// together.
//
// Machinery no single section owns lives in section.go, and the one reader
// every list-valued key shares lives in splitlist.go.
//
// # How a key's environment spelling is derived
//
// This is the contract every section file refers to, and it is a contract
// about how Viper is CONFIGURED rather than about anything a section does.
// A key's environment variable is its dotted path, upper-cased, with every
// dot replaced by an underscore:
//
//	app.port                    APP_PORT
//	app.reports_base_url        APP_REPORTS_BASE_URL
//	fiber.limiter.max           FIBER_LIMITER_MAX
//
// That only holds if the Viper instance was built with an underscore key
// replacer and AutomaticEnv, which NewViper does and nothing else in this
// package does. A caller building its own instance with viper.New() gets
// the file and no environment override at all — every documented variable
// silently ignored, with the file's value or a zero in its place.
//
// NewViper exists for exactly that reason. Before it, the spelling was an
// agreement between this package's comments and an application's loader,
// which is not a thing a second application can be expected to discover.
//
// Environment variables override the file, key by key. An EMPTY variable is
// not a value: AllowEmptyEnv is left off, so an exported-but-empty name
// leaves the file's value in place rather than blanking it.
//
// # Sections load only when something supplied them
//
// A service rarely uses every section. SuppliedSections reports which ones
// some source actually provided a key for, and AbsentSection stands in for
// the rest — it validates clean and logs as "<not configured>", so a
// deployment that does not use a section is neither refused for it nor
// shown its zero values as though somebody had chosen them.
//
// Both sources are consulted, because AllKeys sees only the file:
// AutomaticEnv resolves a key when it is looked up rather than registering
// it. The environment half is the weaker of the two, since it matches a
// variable NAME rather than a key this package reads — see
// SuppliedSections for the consequence, which matters when this package is
// lifted into a deployment whose other software already exports APP_* or
// DATABASE_* variables.
//
// # What Validate does and does not check
//
// Every Validate is a pure function of its struct. None of them opens a
// file, dials a host or resolves a name, which is what lets the whole of
// validation run without a network or a filesystem.
//
// That leaves some checks deliberately to a second authority that runs
// later and knows more: time.LoadLocation for a zone name, the database
// driver for a port it cannot reach, a startup probe for a directory that
// has to exist and be writable. Each section's Validate says which checks
// it leaves where, and why that split is right for that key.
//
// Every Validate appends rather than returning early, and joins the
// results, so one restart surfaces every problem in a section at once.
//
// # String is for logs, and is written to be safe there
//
// A service typically logs every section on start. So every String omits
// or masks what should not reach a log aggregator — a password, a token, a
// credential embedded in a URL — and the Validate beside it refuses the
// shapes that would smuggle one past String.
//
// String uses a pointer receiver throughout, which has one consequence
// worth knowing: fmt only finds it on a *T. Printing a value copy bypasses
// it and dumps the fields directly, secrets included.
//
// # The middleware order the fiber sections assume
//
// Four sections configure middleware — zerolog, requestid, recover and
// limiter — and each one's documentation argues for its POSITION relative
// to the others. The positions only make sense as one stack, so here it
// is, outermost first:
//
//	zl := config.NewZerologConfig(v)
//	rid := config.NewRequestIDConfig(v)
//	rec := config.NewRecoverConfig(v)
//	lim := config.NewLimiterConfig(v)
//
//	app.Use(fiberzerolog.New(zl.WithLogger(log)))                  // 1
//	app.Use(requestid.New(*rid.Config))                            // 2
//	app.Use(fiberrecover.New(rec.WithStackTraceHandler(onPanic)))  // 3
//	app.Use(limiter.New(*lim.Config))                              // 4
//
// Each section embeds the middleware's own Config, which is why the value
// can be handed straight to New. Two things cannot be written in YAML — a
// logger and a panic handler — and those are installed on a copy by the
// With methods rather than stored on the section.
//
// Every request is logged because zerolog is outermost: a panic that
// recover converts, and a 429 the limiter answers, both come back as the
// value of zerolog's c.Next() and so still get a line with a status. The
// request id is readable in all of them because requestid runs before
// anything that could fail. A recovered panic's stack trace carries that
// id because recover sits inside requestid. A panic in the limiter is
// recovered, and a rejection is logged, because the limiter sits inside
// both.
//
// Nothing enforces this order — this package builds configuration and
// never sees the stack — and a service is free to choose another. What it
// cannot do is choose another and keep the guarantees above, which is why
// each section file says which of them its own position is load-bearing
// for.
//
// # What the tests hold in place
//
// sections_test.go holds every section to the same contract at once, by
// parsing this package's own source: each file's header lists exactly the
// keys its code reads, states the right count, and spells each variable
// the way NewViper derives it; every constructor survives an empty Viper;
// every nil receiver validates to an error and logs as "<nil T>"; and no
// String prints a password, token or signing secret. A new section that
// is missing from the test table fails there too.
//
// What is particular to each section — the rules its Validate enforces,
// and helpers such as Namespace and the With installers — is tested beside
// it, in <name>_config_test.go, which is the one-test-file-per-source-file
// layout the rest of this module uses. Every case is built through the
// section's own constructor from a Viper, so a passing case has exercised
// the key name and the cast as well as the rule. The contract suite checks
// that every section file has its test file, so a new section cannot be
// added without one.
//
// The environment is what a unit test cannot reach, so it is covered by
// the integration files, each named for the source file it exercises, as
// everywhere in this module. section_integration_test.go sets real process
// variables, reads a real config.yaml through NewViper, and asserts the
// spelling contract above end to end — that the documented variable is the
// one read, that it overrides the file, that an empty one does not, and
// that SuppliedSections sees a section supplied only through the
// environment. splitlist_integration_test.go reads a comma-separated list
// from a real variable, the case splitList was written for.
//
// There is deliberately no integration file per SECTION. Every section
// gets its environment behaviour from NewViper, so thirteen files each
// re-proving it would add runtime and nothing else; the contract suite
// instead checks that every section's documented spellings follow the
// rule the section integration tests prove.
//
// Those tests use t.Setenv and so cannot run in parallel. That is the
// point of keeping them apart: the rest of the suite can.
//
//	go test -tags integration -run Integration ./config
//
// It needs no server and no configuration of its own.
package config
