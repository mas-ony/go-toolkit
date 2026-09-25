package logger

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/mas-ony/go-toolkit/config"
)

// nonDevelopmentEnvs is every input that must produce the JSON logger.
//
// The list deliberately includes malformed values. The doc comment's promise
// is that an unrecognised env falls through to the production-safe branch,
// so a typo in a deployment manifest cannot switch a production process into
// colourised debug output — which would be both a volume problem and a
// disclosure one, since ConsoleWriter's caller field prints source paths.
var nonDevelopmentEnvs = []string{
	config.EnvStaging,
	config.EnvProduction,
	"",
	"Development", // wrong case: the comparison is exact
	"dev",         // abbreviation
	"DEVELOPMENT",
	"prod",
	"garbage",
}

// Two capture helpers, and which one a test uses is a real distinction.
//
// emit builds the logger through newTo, against a buffer. Everything about
// format, level, caller and field behaviour is observable there, and a buffer
// is not shared state, so those tests run in parallel.
//
// captureNew swaps the process-wide os.Stdout and os.Stderr around a call to
// New itself. That is the only way to check the claim New's doc actually makes
// about descriptors, and it is shared state, so the one test using it does NOT
// call t.Parallel().

// emit runs f against a logger built for env and returns what it wrote.
func emit(env string, f func(zerolog.Logger)) string {
	var buf bytes.Buffer
	f(newTo(&buf, env))
	return buf.String()
}

// captureNew swaps os.Stdout and os.Stderr for pipes, builds a logger for env
// through the exported New, runs f against it, and returns everything each
// stream received.
//
// Both streams are captured, not just stdout, because "goes to stdout" is
// only half the claim. The other half — that nothing leaks to stderr and
// splits the log across two descriptors — is only checkable by watching the
// descriptor that should stay silent.
func captureNew(
	t *testing.T,
	env string,
	f func(zerolog.Logger),
) (stdout, stderr string) {
	t.Helper()

	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}

	// Drain both pipes concurrently. A pipe buffer is finite, and a test that
	// wrote more than it holds would deadlock against itself rather than fail.
	var wg sync.WaitGroup
	var outBuf, errBuf bytes.Buffer
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(&outBuf, outR) }()
	go func() { defer wg.Done(); _, _ = io.Copy(&errBuf, errR) }()

	origOut, origErr := os.Stdout, os.Stderr
	func() {
		// Restore inside a defer so a panic in f cannot leave the process with
		// its stdout pointing at a closed pipe, which would take every later
		// test down with it.
		defer func() {
			os.Stdout, os.Stderr = origOut, origErr
			_ = outW.Close()
			_ = errW.Close()
		}()
		os.Stdout, os.Stderr = outW, errW
		f(New(env))
	}()

	wg.Wait()
	_ = outR.Close()
	_ = errR.Close()
	return outBuf.String(), errBuf.String()
}

// callerLine reports the line its own call site sits on, so a test can name
// the line below it without counting the statements in between.
func callerLine() int {
	_, _, line, _ := runtime.Caller(1)
	return line
}

// subtestName substitutes a word for the empty env, which would otherwise
// produce a subtest with no name at all and a -run pattern nobody can write.
func subtestName(env string) string {
	if env == "" {
		return "empty"
	}
	return env
}

func splitLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

// TestDevelopmentUsesConsoleWriter checks the four things that distinguish the
// development branch: console format, debug level, caller, and a scannable
// timestamp.
func TestDevelopmentUsesConsoleWriter(t *testing.T) {
	t.Parallel()

	out := emit(config.EnvDevelopment, func(l zerolog.Logger) {
		l.Debug().Str("key", "value").Msg("hello from development")
	})

	if out == "" {
		t.Fatal("no output; a debug entry must be emitted in development")
	}
	if json.Valid([]byte(out)) {
		t.Errorf("output parsed as JSON, want console format:\n%s", out)
	}
	if !strings.Contains(out, "hello from development") {
		t.Errorf("message missing from output:\n%s", out)
	}
	if !strings.Contains(out, "value") {
		t.Errorf("structured field missing from output:\n%s", out)
	}
	if !strings.Contains(out, "logger_test.go:") {
		t.Errorf(
			"caller missing; development entries carry file:line:\n%s",
			out)
	}

	// time.DateTime, per the ConsoleWriter configuration.
	stamp := regexp.MustCompile(`\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}`)
	if !stamp.MatchString(out) {
		t.Errorf("timestamp is not in the scannable %q format:\n%s",
			time.DateTime, out)
	}
}

// TestNonDevelopmentEmitsStructuredJSON is the production contract, checked
// against every env spelling that must reach it.
//
// The caller assertion is the one with teeth. It is not a style preference:
// runtime.Callers runs on every entry when the field is enabled, and the
// source paths it emits are internal structure that may be visible to
// whoever operates the log aggregator.
func TestNonDevelopmentEmitsStructuredJSON(t *testing.T) {
	t.Parallel()

	for _, env := range nonDevelopmentEnvs {
		t.Run(subtestName(env), func(t *testing.T) {
			t.Parallel()

			out := emit(env, func(l zerolog.Logger) {
				l.Info().Str("service", "billing").Msg("started")
			})

			line := strings.TrimSpace(out)
			if line == "" {
				t.Fatal("no output; info entries must be emitted everywhere")
			}

			var entry map[string]any
			if err := json.Unmarshal([]byte(line), &entry); err != nil {
				t.Fatalf("output is not valid JSON (%v):\n%s", err, line)
			}
			if got := entry["level"]; got != "info" {
				t.Errorf("level: got %v, want \"info\"", got)
			}
			if got := entry["message"]; got != "started" {
				t.Errorf("message: got %v, want \"started\"", got)
			}
			if got := entry["service"]; got != "billing" {
				t.Errorf("service: got %v, want \"billing\"", got)
			}
			if _, ok := entry["caller"]; ok {
				t.Error("caller is present; it must be omitted outside dev")
			}
			ts, ok := entry["time"].(string)
			if !ok {
				t.Fatalf("time: got %v, want an RFC 3339 string",
					entry["time"])
			}
			if _, err := time.Parse(time.RFC3339, ts); err != nil {
				t.Errorf("time %q is not RFC 3339: %v", ts, err)
			}
		})
	}
}

// TestLevelFloorPerEnvironment states the two levels directly, which is both
// the clearest form of the assertion and the one that still holds if the
// output format ever changes.
func TestLevelFloorPerEnvironment(t *testing.T) {
	t.Parallel()

	tests := []struct {
		env  string
		want zerolog.Level
	}{
		{config.EnvDevelopment, zerolog.DebugLevel},
		{config.EnvStaging, zerolog.InfoLevel},
		{config.EnvProduction, zerolog.InfoLevel},
		{"garbage", zerolog.InfoLevel},
	}

	for _, tc := range tests {
		if got := New(tc.env).GetLevel(); got != tc.want {
			t.Errorf(
				"New(%q).GetLevel(): got %v, want %v",
				tc.env,
				got,
				tc.want)
		}
	}
}

// TestVerbosityBoundaries walks the level either side of each floor.
//
// Development stops at Debug rather than Trace on purpose: trace entries are
// mostly library-internal and drown the application lines they are supposed
// to sit next to. That is a judgement call in the doc comment, so it gets a
// test — otherwise "why not Trace?" gets re-litigated by whoever needs one
// trace line.
func TestVerbosityBoundaries(t *testing.T) {
	t.Parallel()

	debug := func(l zerolog.Logger) { l.Debug().Msg("x") }
	trace := func(l zerolog.Logger) { l.Trace().Msg("x") }
	info := func(l zerolog.Logger) { l.Info().Msg("x") }
	warn := func(l zerolog.Logger) { l.Warn().Msg("x") }
	fail := func(l zerolog.Logger) { l.Error().Msg("x") }

	tests := []struct {
		name    string
		env     string
		emit    func(zerolog.Logger)
		wantOut bool
	}{
		{"development emits debug", config.EnvDevelopment, debug, true},
		{"development suppresses trace", config.EnvDevelopment, trace, false},
		{"production suppresses debug", config.EnvProduction, debug, false},
		{"production suppresses trace", config.EnvProduction, trace, false},
		{"production emits info", config.EnvProduction, info, true},
		{"production emits warn", config.EnvProduction, warn, true},
		{"production emits error", config.EnvProduction, fail, true},
		{"staging suppresses debug", config.EnvStaging, debug, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out := emit(tc.env, tc.emit)
			if got := out != ""; got != tc.wantOut {
				t.Errorf(
					"output produced: got %t, want %t (output: %q)",
					got,
					tc.wantOut,
					out)
			}
		})
	}
}

// TestEverythingGoesToStdout guards the container-logging decision, and is the
// one test here that has to go through the exported New.
//
// Docker's json-file driver and Kubernetes both capture stdout by default.
// Splitting the stream across two descriptors is not a cosmetic problem: it
// breaks correlation between access logs and application logs in a log
// aggregator, which is precisely what someone reaches for when an error line
// appears.
//
// No t.Parallel(), here or in its subtests: swapping os.Stdout is
// process-wide, and two tests doing it at once would capture each other's
// output.
func TestEverythingGoesToStdout(t *testing.T) {
	envs := []string{
		config.EnvDevelopment,
		config.EnvStaging,
		config.EnvProduction,
	}

	for _, env := range envs {
		t.Run(env, func(t *testing.T) {
			stdout, stderr := captureNew(t, env, func(l zerolog.Logger) {
				l.Info().Msg("info line")
				l.Warn().Msg("warn line")
				l.Error().Msg("error line")
			})
			if stdout == "" {
				t.Error("nothing was written to stdout")
			}
			if stderr != "" {
				t.Errorf(
					"stderr received output, splitting the log stream:\n%s",
					stderr)
			}
		})
	}
}

// TestGlobalLevelIsUntouched backs the note about zerolog.SetGlobalLevel.
//
// The global level is process-wide and would silence library loggers too.
// Nothing about calling New should change it, and if an edit does, the
// symptom is a third-party library going quiet — which nobody would trace
// back to this constructor.
//
// No t.Parallel(): the assertion is about process-wide state, so a parallel
// test mutating it would make this flake rather than fail.
func TestGlobalLevelIsUntouched(t *testing.T) {
	before := zerolog.GlobalLevel()
	t.Cleanup(func() { zerolog.SetGlobalLevel(before) })

	envs := append([]string{config.EnvDevelopment}, nonDevelopmentEnvs...)
	for _, env := range envs {
		_ = New(env)
		if got := zerolog.GlobalLevel(); got != before {
			t.Fatalf(
				"New(%q) changed the global level from %v to %v",
				env,
				before,
				got)
		}
	}
}

// TestChildLoggerInheritsLevelAndWriter covers the pattern the doc comment
// recommends: tag once at construction, and the tag rides every later line.
//
// Both halves are asserted. The writer must be inherited (the child's output
// still reaches the captured stdout) and so must the level (a Debug call on
// a production child is still suppressed), because a child that quietly
// reset either would reintroduce debug volume one component at a time.
func TestChildLoggerInheritsLevelAndWriter(t *testing.T) {
	t.Parallel()

	out := emit(config.EnvProduction, func(l zerolog.Logger) {
		child := l.With().Str("service", "billing").Logger()
		child.Debug().Msg("suppressed")
		child.Info().Msg("emitted")
	})

	lines := splitLines(out)
	if len(lines) != 1 {
		t.Fatalf(
			"got %d lines, want 1 (debug must be suppressed):\n%s",
			len(lines),
			out)
	}

	var entry map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if entry["service"] != "billing" {
		t.Errorf("service tag missing from the child's output: %v", entry)
	}
	if entry["message"] != "emitted" {
		t.Errorf("message: got %v, want \"emitted\"", entry["message"])
	}
}

// TestChildIsIndependentOfItsParent is the reason returning by value is safe.
//
// .With() derives rather than mutates, so one component tagging itself
// cannot leak its tag onto every other component sharing the same logger —
// which is exactly what would happen if New returned a pointer and .With()
// modified it in place.
func TestChildIsIndependentOfItsParent(t *testing.T) {
	t.Parallel()

	out := emit(config.EnvProduction, func(parent zerolog.Logger) {
		_ = parent.With().Str("service", "wilayah").Logger()
		parent.Info().Msg("from the parent")
	})

	var entry map[string]any
	line := strings.TrimSpace(out)
	if err := json.Unmarshal([]byte(line), &entry); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}
	if _, ok := entry["service"]; ok {
		t.Errorf("the child's tag leaked onto the parent: %v", entry)
	}
}

// TestCallerReportsTheCallSiteNotAHelper pins the caveat in the doc comment.
//
// zerolog records the frame that called the event method, so a logError(log,
// err) wrapper makes every entry blame the wrapper. CallerSkipFrameCount
// exists for that and is deliberately not set.
//
// The assertion is on the LINE NUMBER, which is the only thing that can tell
// the two frames apart. Checking merely that the output contains
// "logger_test.go:" would not do it: the helper and its call site are both in
// this file, so such an assertion holds under either behaviour and would stay
// green with CallerSkipFrameCount set to 1.
//
// Both line numbers are computed by the code that owns them, on the line
// immediately above the call they describe, so editing this file cannot
// silently invalidate the expectation. callerLine is what keeps that offset
// honest: reading runtime.Caller inline would put the assignment itself
// between the reading and the call, making the true offset two rather than
// one.
func TestCallerReportsTheCallSiteNotAHelper(t *testing.T) {
	t.Parallel()

	var helperLine int
	logViaHelper := func(l zerolog.Logger, msg string) {
		helperLine = callerLine() + 1
		l.Debug().Msg(msg) // <- the frame zerolog must report
	}

	var callSiteLine int
	out := emit(config.EnvDevelopment, func(l zerolog.Logger) {
		callSiteLine = callerLine() + 1
		logViaHelper(l, "through a wrapper")
	})

	m := regexp.MustCompile(`logger_test\.go:(\d+)`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no caller in output:\n%s", out)
	}
	got, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("unparseable caller line %q: %v", m[1], err)
	}

	switch got {
	case helperLine:
		// Correct: no frames are skipped, so the wrapper takes the blame.
	case callSiteLine:
		t.Errorf("caller reported line %d, the CALL SITE; zerolog skipped "+
			"a frame, so CallerSkipFrameCount is set and the doc "+
			"comment's caveat does not hold", got)
	default:
		t.Errorf("caller reported line %d, want %d (the helper's own log "+
			"call):\n%s", got, helperLine, out)
	}
}

// TestNewIsSafeToCallConcurrently is a small guard on the constructor being
// a pure function of its argument. A process typically calls it once, but
// nothing in the signature says it must, and an edit reaching for a
// package-level variable would show up here under -race.
func TestNewIsSafeToCallConcurrently(t *testing.T) {
	t.Parallel()

	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			env := config.EnvProduction
			if i%2 == 0 {
				env = config.EnvDevelopment
			}
			_ = New(env)
		}(i)
	}
	wg.Wait()
}
