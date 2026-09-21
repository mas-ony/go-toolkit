// Package logger constructs the application's structured logger. It wraps
// zerolog and makes one decision — human-readable console output in
// development, machine-readable JSON everywhere else.
//
// It is a constructor and nothing more. There is no logger type, no
// interface, no global, and no Init: New returns a zerolog.Logger by value
// and the caller owns it from there. Everything this package knows is
// spent at that one call.
//
//	log := logger.New(cfg.App.Env)
//	log.Info().Str("addr", addr).Msg("listening")
//
// A component tags itself once and the tag rides along afterwards, which
// is what a value-returning constructor buys:
//
//	log := log.With().Str("service", "billing").Logger()
//
// # The environment string
//
// New takes a plain string rather than a config value, and compares it
// against config.EnvDevelopment. The constant is imported rather than
// redeclared here: the config package is this module's vocabulary for
// deployment settings, the same way database.New takes a
// config.DatabaseConfig, and a second declaration of the same three
// strings would be a drift hazard needing a test to police it.
//
// A service holding an env string from somewhere else passes it directly;
// one holding a config.AppConfig passes cfg.Env.
//
// The comparison is exact. New says why at length; the short version is
// that the two ways of getting it wrong cost different amounts, and only
// one of them is noticed by the person who made the mistake.
//
// Any value that is not development falls through to the JSON logger.
// That direction is chosen on purpose: a misconfigured env produces
// production-safe output rather than accidentally enabling debug logging
// and caller paths in a place logs may be visible to someone outside the
// team.
//
// # What differs between the two shapes
//
//	                development            everything else
//	format         ConsoleWriter, colour   JSON
//	level          Debug                   Info
//	caller         file:line on each line  omitted
//	destination    os.Stdout               os.Stdout
//
// Each row is argued at New, where it is set. What is worth stating at
// this level is that only the first three differ: both shapes write to
// stdout, because a container runtime collects it by default and splitting
// a log stream across two descriptors makes access logs and application
// logs hard to correlate afterwards.
//
// Trace is below the floor in both. zerolog's trace messages are usually
// library-internal and high volume, and they bury the application's own
// debug lines rather than adding to them.
//
// # What this package does not touch
//
// zerolog.SetGlobalLevel is never called. The level is scoped to the
// returned Logger with .Level(), so a library logging through its own
// zerolog instance keeps whatever level it chose. Setting the global would
// reach every logger in the process, which is a larger claim than a
// constructor is entitled to make.
//
// Nothing is sampled, nothing is buffered, and no hook is installed. A
// deployment that wants sampling or an error reporter derives a child from
// the returned logger rather than asking for it here.
//
// # What the tests hold in place
//
// All of it, with no server and no external service. There is no
// integration suite in this package because there is nothing to integrate
// with: the only boundary a logger crosses is a file descriptor, and
// logger_test.go crosses it directly — it swaps os.Stdout and os.Stderr
// around New to prove that every environment writes to the first and
// nothing leaks to the second.
//
// The rest runs against a buffer through the unexported newTo, which is
// what keeps those tests independent of process-wide state and free to run
// in parallel. Level floors, verbosity boundaries, the console and JSON
// shapes, caller attribution, child-logger inheritance, the untouched
// global level, and the mis-cased environment names that must NOT reach
// the console branch are each pinned there.
package logger
