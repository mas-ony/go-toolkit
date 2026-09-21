package logger

import (
	"io"
	"os"
	"time"

	"github.com/rs/zerolog"

	"github.com/mas-ony/go-toolkit/config"
)

// newTo is New with the destination supplied by the caller.
//
// It exists so the tests can assert level, format, caller and field
// behaviour against a buffer instead of swapping the process-wide
// os.Stdout descriptor, which is shared state and forces a whole test file
// to run sequentially. The one claim that genuinely needs the descriptor
// swap — that nothing leaks to stderr — is still tested through New.
//
// Unexported on purpose: the destination is a deployment decision, not a
// caller's, and exporting it would invite a component to log somewhere the
// container runtime does not collect.
func newTo(w io.Writer, env string) zerolog.Logger {
	if env == config.EnvDevelopment {
		console := zerolog.ConsoleWriter{
			Out:        w,
			TimeFormat: time.DateTime,
		}
		return zerolog.New(console).
			Level(zerolog.DebugLevel).
			With().
			Timestamp().
			Caller().
			Logger()
	}

	return zerolog.New(w).
		Level(zerolog.InfoLevel).
		With().
		Timestamp().
		Logger()
}

// New returns a zerolog.Logger configured for the given environment,
// writing to os.Stdout.
//
// Returned BY VALUE, which is how zerolog is meant to be used: a Logger is
// a small struct, copying is cheap, and .With() derives an independent
// child rather than mutating a shared parent. That is what lets a component
// tag itself once at construction —
//
//	log.With().Str("service", "service_name").Logger()
//
// — and have the tag appear on every later line without threading a field
// through each call. Children inherit level and writer from the parent, so
// the decisions made here apply process-wide.
//
// Both branches write to os.Stdout rather than os.Stderr. Container
// runtimes (Docker json-file driver, Kubernetes) capture stdout by default
// and route it to the log aggregator. Writing to stderr would split the log
// stream across two file descriptors, making correlation between access
// logs and application logs harder in tools like Loki or CloudWatch Logs.
//
// Development (env exactly config.EnvDevelopment):
//   - Output: zerolog.ConsoleWriter — colourised, human-readable lines.
//   - Level: Debug — every level from debug upward (debug, info, warn,
//     error, fatal, panic) is emitted; trace is suppressed. Trace is not
//     the development floor because zerolog's trace messages are typically
//     library-internal and produce very high volume output that obscures
//     application-level debug lines.
//   - Caller: included — every log entry shows "file:line" so errors can be
//     located immediately without a stack trace. The frame it reports is
//     the one that called the zerolog event method, so this is accurate
//     only while logging happens directly on the logger. Wrap it in a
//     helper — logError(log, err) — and every entry blames the helper.
//     zerolog's CallerSkipFrameCount exists for that case and is not set
//     here.
//   - TimeFormat: time.DateTime ("2006-01-02 15:04:05") for easy visual
//     scanning.
//   - ConsoleWriter re-parses each JSON line to colourise it, which zerolog
//     documents as unsuitable for production throughput. Development-only
//     use keeps that cost where it does not matter.
//   - NoColor is left at its zero value, so the ANSI codes are written
//     whatever the destination is — a terminal, a pipe, or a redirect into
//     a file. zerolog does no terminal detection of its own and there is
//     none here either, so redirecting the output produces a file with
//     escape sequences in it. Suppressing them means either setting NoColor
//     from an isatty check, which is a new direct dependency for a
//     development-only convenience, or letting the operator pipe through a
//     stripper. Neither is done; this says which of the two it is so nobody
//     reads the escapes as a bug.
//
// Staging / Production (all other env values):
//   - Output: raw JSON, suitable for log aggregators (Loki, CloudWatch,
//     Datadog, etc.) that parse structured log lines.
//   - Level: Info — debug and trace entries are suppressed to reduce log
//     volume.
//   - Caller: omitted — runtime.Callers is called on every log entry when
//     the caller field is enabled. The overhead is acceptable in
//     development but non-trivial under high-throughput production traffic.
//     Source paths are also development-time information that can expose
//     internal structure in environments where logs may be visible to
//     external operators.
//
// The env argument should be one of the constants defined in the config
// package (config.EnvDevelopment, config.EnvStaging, config.EnvProduction).
// An unrecognised value falls through to the production-safe JSON logger,
// so a misconfigured env does not accidentally enable debug output.
//
// The comparison is EXACT — not trimmed, not case-folded — and the
// asymmetry with how other packages here normalise their config strings is
// deliberate. Those normalise because a mis-cased value would fail loudly
// and unhelpfully. This one would fail the other way: a typo is the only
// thing standing between a production manifest and colourised debug
// output, which is a volume problem and a disclosure one, since the
// console branch prints source paths in its caller field.
//
// So the two directions cost different amounts. A development deployment
// spelled "Development" gets JSON at info level, notices within a minute
// that debug lines are missing, and fixes the manifest. A production
// deployment that drifts the other way is not noticed by anyone watching
// the service — only by whoever later reads the logs. Exact matching makes
// the second case unreachable and the first self-correcting.
//
// zerolog.SetGlobalLevel is deliberately not called here. Setting the
// global level would affect every zerolog logger in the process, library
// loggers included. Scoping the level to the returned Logger instance via
// .Level() keeps library log output unaffected by application log
// settings.
func New(env string) zerolog.Logger {
	return newTo(os.Stdout, env)
}
