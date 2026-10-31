package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	fiberrecover "github.com/gofiber/fiber/v3/middleware/recover"
	"github.com/gofiber/fiber/v3/middleware/requestid"
	"github.com/mas-ony/go-toolkit/internal/testutil"
	"github.com/mas-ony/go-toolkit/internal/testutil/fibertest"
	"github.com/rs/zerolog"
)

// line is one decoded log line.
type line map[string]any

// newApp returns an app with ErrorHandler and, under recover,
// StackTraceHandler, logging into w, and with a body limit of 16 bytes.
func newApp(w io.Writer) *fiber.App {
	log := zerolog.New(w)
	app := fiber.New(fiber.Config{
		BodyLimit:    16,
		ErrorHandler: ErrorHandler(log, "request_id"),
	})
	app.Use(requestid.New())
	app.Use(fiberrecover.New(fiberrecover.Config{
		EnableStackTrace:  true,
		StackTraceHandler: StackTraceHandler(log, "request_id"),
	}))
	app.Get("/missing", func(fiber.Ctx) error {
		return fiber.NewError(fiber.StatusNotFound, "item not found")
	})
	app.Get("/bug", func(fiber.Ctx) error {
		return errors.New("dial tcp: password=secret")
	})
	app.Get("/panic", func(fiber.Ctx) error { panic("boom") })
	app.Post("/echo", func(c fiber.Ctx) error { return c.SendStatus(204) })
	return app
}

// lines decodes every line in buf.
func lines(t *testing.T, buf *bytes.Buffer) []line {
	t.Helper()
	var out []line
	for _, raw := range bytes.Split(bytes.TrimSpace(buf.Bytes()),
		[]byte("\n")) {
		if len(raw) == 0 {
			continue
		}
		var l line
		if err := json.Unmarshal(raw, &l); err != nil {
			t.Fatalf("log line is not JSON: %q", raw)
		}
		out = append(out, l)
	}
	return out
}

// send runs one request and returns its status and decoded envelope.
func send(t *testing.T, app *fiber.App, method, path, body string) (
	int, map[string]any) {
	t.Helper()
	resp, err := app.Test(httptest.NewRequest(method, path,
		strings.NewReader(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var env map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatalf("%s %s: body is not JSON: %q", method, path, raw)
		}
	}
	return resp.StatusCode, env
}

// A *fiber.Error keeps its status and message and logs nothing.
func TestErrorHandlerFiberError(t *testing.T) {
	var buf bytes.Buffer
	status, env := send(t, newApp(&buf), "GET", "/missing", "")
	if status != 404 || env["success"] != false ||
		env["message"] != "item not found" {
		t.Fatalf("got %d %v", status, env)
	}
	if got := lines(t, &buf); len(got) != 0 {
		t.Errorf("logged %v", got)
	}
}

// Any other error is a 500 whose text stays in the log.
func TestErrorHandlerOtherError(t *testing.T) {
	var buf bytes.Buffer
	status, env := send(t, newApp(&buf), "GET", "/bug", "")
	if status != 500 || env["message"] != "internal server error" {
		t.Fatalf("got %d %v", status, env)
	}
	got := lines(t, &buf)
	if len(got) != 1 || got[0]["level"] != "error" ||
		got[0]["error"] != "dial tcp: password=secret" ||
		got[0]["path"] != "/bug" || got[0]["request_id"] == "" {
		t.Errorf("log = %v", got)
	}
}

// A body over the limit is a 413 in the envelope, with a line of its own.
// fasthttp refuses the body before routing, which only a request read off
// the wire shows; app.Test reports the refusal as its own error instead.
func TestErrorHandlerBodyLimit(t *testing.T) {
	var buf testutil.SyncBuffer
	base := fibertest.Serve(t, newApp(&buf))
	resp, err := http.Post(base+"/echo", "text/plain",
		strings.NewReader(strings.Repeat("x", 64)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var env map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 413 || env["success"] != false {
		t.Fatalf("got %d %v", resp.StatusCode, env)
	}
	got := lines(t, bytes.NewBuffer(buf.Bytes()))
	if len(got) != 1 || got[0]["level"] != "warn" ||
		got[0]["status"] != float64(413) {
		t.Errorf("log = %v", got)
	}
}

// A panic is logged with its stack and answered 500.
func TestStackTraceHandler(t *testing.T) {
	var buf bytes.Buffer
	status, env := send(t, newApp(&buf), "GET", "/panic", "")
	if status != 500 || env["message"] != "internal server error" {
		t.Fatalf("got %d %v", status, env)
	}
	var panicked bool
	for _, l := range lines(t, &buf) {
		if l["message"] == "recovered panic" {
			panicked = l["panic"] == "boom" && l["stack"] != nil &&
				l["request_id"] != ""
		}
	}
	if !panicked {
		t.Errorf("no recovered panic line: %s", buf.String())
	}
}

// LimitReached hands the 429 to the error handler.
func TestLimitReached(t *testing.T) {
	if err := LimitReached(nil); !errors.Is(err,
		fiber.ErrTooManyRequests) {
		t.Errorf("LimitReached = %v", err)
	}
}
