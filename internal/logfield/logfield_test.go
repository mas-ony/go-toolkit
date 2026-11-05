package logfield

// Tests for logfield.go: each helper writes its fields under the keys the
// lines of rest, server, notify and crud are read by, and leaves the rest
// of the line to its caller.

import (
	"bytes"
	"testing"

	"github.com/mas-ony/go-toolkit/internal/testutil"
	"github.com/rs/zerolog"
)

// written logs one line, its fields added by add, and returns it decoded.
func written(
	t *testing.T,
	add func(e *zerolog.Event) *zerolog.Event,
) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	log := zerolog.New(&buf)
	add(log.Error().Str("handler", "row")).Msg("logged")
	return testutil.Only(t, "log lines", testutil.LogLines(t, buf.Bytes()))
}

// The name is written under handler on every line the returned logger
// writes, beside what each line adds of its own.
func TestHandler(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	log := Handler(zerolog.New(&buf), "error")
	log.Warn().Int("status", 413).Msg("first")
	log.Error().Msg("second")
	lines := testutil.LogLines(t, buf.Bytes())
	if len(lines) != 2 {
		t.Fatalf("saw %d log lines, want 2", len(lines))
	}
	for i, got := range lines {
		if got["handler"] != "error" {
			t.Errorf("line %d = %v", i, got)
		}
	}
	if lines[0]["status"] != float64(413) || lines[1]["message"] != "second" {
		t.Errorf("lines = %v", lines)
	}
}

// The method and the path are written under method and path, beside what
// the caller put on the line before and after them.
func TestRequest(t *testing.T) {
	t.Parallel()
	got := written(t, func(e *zerolog.Event) *zerolog.Event {
		return Request(e, "DELETE", "/r/7").Int("caller", 4)
	})
	if got["method"] != "DELETE" || got["path"] != "/r/7" ||
		got["handler"] != "row" || got["caller"] != float64(4) ||
		got["message"] != "logged" {
		t.Errorf("line = %v", got)
	}
}

// The value is written under panic and the trace under stack, exactly as
// the caller took it.
func TestPanic(t *testing.T) {
	t.Parallel()
	const trace = "goroutine 7 [running]:\nmain.recovering()"
	got := written(t, func(e *zerolog.Event) *zerolog.Event {
		return Panic(e, "boom", []byte(trace))
	})
	if got["panic"] != "boom" || got["stack"] != trace ||
		got["handler"] != "row" || got["level"] != "error" {
		t.Errorf("line = %v", got)
	}
}

// The name is written under entity, beside what the caller put on the
// line before and after it.
func TestEntity(t *testing.T) {
	t.Parallel()
	got := written(t, func(e *zerolog.Event) *zerolog.Event {
		return Entity(e, "surat_masuk").Int("id", 7)
	})
	if got["entity"] != "surat_masuk" || got["id"] != float64(7) ||
		got["handler"] != "row" || got["message"] != "logged" {
		t.Errorf("line = %v", got)
	}
}
