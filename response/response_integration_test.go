//go:build integration

package response

// Integration tests for the universality claim in doc.go.
//
// Everything else about this package is testable from the struct: which
// keys appear, which are omitted, what a typed nil does. What is NOT
// testable from the struct is the claim the package exists to support —
// that a client can rely on this shape for EVERY response, including the
// ones no handler produced.
//
// Those are the failures a framework generates on its own: a route that
// matches nothing, a method the route does not allow, a body over the
// configured limit, a panic. Each has a default rendering that is not this
// envelope, and each becomes this envelope only because an ErrorHandler at
// the top of the application built it. That wiring is the thing under
// test, so the test builds it.
//
// This is the only file in the package that imports a web framework, it is
// behind a build tag, and it is a test file — the package itself still
// depends on nothing outside the standard library, which is the property
// worth not losing to a convenience.
//
//	go test -tags integration -run Integration ./response
//
// It needs no server and no configuration; the framework's own in-process
// test transport carries the requests.

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/recover"
)

// decoded is the envelope as a client would parse it, plus whether each
// optional key was present at all — which is the distinction omitempty
// exists for and the one a plain struct decode would erase.
type decoded struct {
	Success    bool
	Message    string
	HasMessage bool
	HasData    bool
	DataIsNull bool
}

// envelopeApp is the wiring under test: one ErrorHandler that renders
// every escaped error through this package.
//
// The message rule is the one doc.go describes — a recognised framework
// error passes its own message through, and anything else collapses to a
// fixed string — so the test also shows that an unexpected error cannot
// leak its text to a client.
func envelopeApp(t *testing.T, cfg fiber.Config) *fiber.App {
	t.Helper()

	cfg.ErrorHandler = func(c fiber.Ctx, err error) error {
		code := fiber.StatusInternalServerError
		msg := "internal server error"

		var fe *fiber.Error
		if errors.As(err, &fe) {
			code, msg = fe.Code, fe.Message
		}
		return c.Status(code).JSON(Fail(msg))
	}

	app := fiber.New(cfg)

	// The recover middleware is REQUIRED for the panic case and is not on
	// by default. Without it a panic unwinds past the ErrorHandler and
	// kills the process, so "panics reach the envelope" is a claim about
	// the application's middleware stack rather than about this package.
	// Mounting it here is what makes the claim testable at all.
	app.Use(recover.New())

	app.Get("/items", func(c fiber.Ctx) error {
		return c.JSON(OKList("", []string{"a", "b"}))
	})
	app.Post("/items", func(c fiber.Ctx) error {
		return c.JSON(OK("created", nil))
	})
	app.Get("/boom", func(c fiber.Ctx) error {
		panic("handler exploded")
	})
	return app
}

// waitForListener covers the gap between Listen returning and the serving
// goroutine accepting connections.
func waitForListener(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.Dial("tcp", addr); err == nil {
			_ = c.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the listener never accepted a connection")
}

func do(t *testing.T, app *fiber.App, req *http.Request) (int, decoded) {
	t.Helper()
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 0})
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("status %d body is not the envelope: %v\n%s",
			resp.StatusCode, err, body)
	}

	var d decoded
	if v, ok := raw["success"]; ok {
		if err := json.Unmarshal(v, &d.Success); err != nil {
			t.Fatalf("success: %v", err)
		}
	} else {
		t.Errorf("no \"success\" key in %s", body)
	}
	if v, ok := raw["message"]; ok {
		d.HasMessage = true
		if err := json.Unmarshal(v, &d.Message); err != nil {
			t.Fatalf("message: %v", err)
		}
	}
	if v, ok := raw["data"]; ok {
		d.HasData = true
		d.DataIsNull = string(v) == "null"
	}
	return resp.StatusCode, d
}

// The failures a handler never sees that DO route through the
// ErrorHandler. Each one's default rendering is not this envelope — an
// unmatched route is plain text, a recovered panic is a stack trace — so
// each is evidence that one ErrorHandler is what makes the shape uniform.
//
// Not every framework failure reaches here; see the body-limit test below
// for the one that does not.
func TestIntegrationFrameworkErrorsUseTheEnvelope(t *testing.T) {
	app := envelopeApp(t, fiber.Config{})

	tests := []struct {
		name     string
		req      *http.Request
		wantCode int
		// wantMessage is empty when the framework's own wording is not
		// worth pinning; only its PRESENCE is required on a failure.
		wantMessage string
	}{
		{
			name:     "unmatched route",
			req:      httptest.NewRequest(http.MethodGet, "/nope", nil),
			wantCode: http.StatusNotFound,
		},
		{
			name: "method the route does not allow",
			req: httptest.NewRequest(http.MethodDelete, "/items",
				nil),
			wantCode: http.StatusMethodNotAllowed,
		},
		{
			name:        "panic in a handler",
			req:         httptest.NewRequest(http.MethodGet, "/boom", nil),
			wantCode:    http.StatusInternalServerError,
			wantMessage: "internal server error",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, env := do(t, app, tc.req)

			if code != tc.wantCode {
				t.Errorf("status %d, want %d", code, tc.wantCode)
			}
			// The rule the whole envelope rests on: a non-2xx never
			// carries Success true.
			if env.Success {
				t.Error("success is true on a failure response")
			}
			if !env.HasMessage || env.Message == "" {
				t.Error("no message on a failure response")
			}
			if tc.wantMessage != "" && env.Message != tc.wantMessage {
				t.Errorf("message %q, want %q", env.Message,
					tc.wantMessage)
			}
			// Fail never sets Data, so the key must be absent rather
			// than present as null.
			if env.HasData {
				t.Error("a failure response carries a data key")
			}
		})
	}
}

// A panic must not put the panic value in front of a client. This is the
// half of the message rule that matters, and it is only reachable through
// a framework that recovers.
func TestIntegrationPanicTextDoesNotReachTheClient(t *testing.T) {
	app := envelopeApp(t, fiber.Config{})

	_, env := do(t, app,
		httptest.NewRequest(http.MethodGet, "/boom", nil))

	if strings.Contains(env.Message, "exploded") {
		t.Errorf("the panic value reached the client: %q", env.Message)
	}
}

// The success side, through the same transport: OKList normalises an empty
// slice, and OK with an untyped nil omits the key entirely. Both are
// asserted on the WIRE rather than on the struct, because omitempty is a
// property of the encoding and this is where it actually runs.
func TestIntegrationSuccessShapesOnTheWire(t *testing.T) {
	app := fiber.New()
	app.Get("/empty", func(c fiber.Ctx) error {
		var none []string // nil
		return c.JSON(OKList("", none))
	})
	app.Get("/full", func(c fiber.Ctx) error {
		return c.JSON(OKList("", []string{"a"}))
	})
	app.Delete("/thing", func(c fiber.Ctx) error {
		return c.JSON(OK("deleted", nil))
	})
	app.Get("/trap", func(c fiber.Ctx) error {
		var none []string // nil, straight into OK
		return c.JSON(OK("", none))
	})

	t.Run("OKList turns a nil slice into an empty array", func(t *testing.T) {
		code, env := do(t, app,
			httptest.NewRequest(http.MethodGet, "/empty", nil))
		if code != http.StatusOK || !env.Success {
			t.Fatalf("status %d success %v", code, env.Success)
		}
		if !env.HasData {
			t.Fatal("no data key, want []")
		}
		if env.DataIsNull {
			t.Error("data is null; OKList did not normalise")
		}
		if env.HasMessage {
			t.Error("an empty message produced a message key")
		}
	})

	t.Run("a populated slice is unaffected", func(t *testing.T) {
		_, env := do(t, app,
			httptest.NewRequest(http.MethodGet, "/full", nil))
		if !env.HasData || env.DataIsNull {
			t.Error("data missing or null for a populated slice")
		}
	})

	t.Run("an untyped nil omits data", func(t *testing.T) {
		_, env := do(t, app,
			httptest.NewRequest(http.MethodDelete, "/thing", nil))
		if env.HasData {
			t.Error("data key present, want it omitted")
		}
		if env.Message != "deleted" {
			t.Errorf("message %q, want deleted", env.Message)
		}
	})

	// The trap itself, pinned rather than fixed: OK with a nil slice
	// really does put "data":null on the wire. If this ever starts
	// failing, OK acquired normalisation and doc.go is now wrong.
	t.Run("OK with a nil slice still sends null", func(t *testing.T) {
		_, env := do(t, app,
			httptest.NewRequest(http.MethodGet, "/trap", nil))
		if !env.HasData || !env.DataIsNull {
			t.Errorf("data present=%v null=%v — OK now normalises, "+
				"which doc.go says it does not",
				env.HasData, env.DataIsNull)
		}
	})
}

// A body over BodyLimit always reaches the ErrorHandler; whether the client
// reads the envelope that builds depends on how much it was still sending.
//
// This runs on a REAL listener because the in-process test transport gives
// the wrong answer here: it fails every oversized request with a transport
// error, whatever its size, while a real client that finished sending reads
// the 413 perfectly well. A test written against the in-process transport
// would therefore claim clients never see the envelope, which is false.
//
// Only the deterministic half is asserted: a body a little over the limit
// is sent in full before the server answers, so its client must read the
// envelope. A body far over the limit usually gets a connection reset
// instead, because the server closes while the client is still writing —
// but "usually" depends on socket buffer sizes, so that half is logged
// rather than asserted. Asserting it would make a flaky test.
func TestIntegrationBodyLimitReachesTheErrorHandler(t *testing.T) {
	var mu sync.Mutex
	handled := 0

	app := fiber.New(fiber.Config{
		BodyLimit: 32,
		ErrorHandler: func(c fiber.Ctx, err error) error {
			mu.Lock()
			handled++
			mu.Unlock()
			code := fiber.StatusInternalServerError
			var fe *fiber.Error
			if errors.As(err, &fe) {
				code = fe.Code
			}
			return c.Status(code).JSON(Fail("request body too large"))
		},
	})
	app.Post("/items", func(c fiber.Ctx) error {
		t.Error("the handler ran despite an over-limit body")
		return nil
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = app.Listener(ln) }()
	t.Cleanup(func() { _ = app.Shutdown() })
	url := "http://" + ln.Addr().String() + "/items"
	waitForListener(t, ln.Addr().String())

	post := func(size int) (*http.Response, error) {
		return http.Post(url, "application/json",
			strings.NewReader(strings.Repeat("x", size)))
	}

	// A little over the limit: sent in full, so the envelope arrives.
	resp, err := post(512)
	if err != nil {
		t.Fatalf("a 512-byte body got a transport error: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status %d, want %d", resp.StatusCode,
			http.StatusRequestEntityTooLarge)
	}
	var env struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("the client did not receive the envelope: %v\n%s",
			err, body)
	}
	if env.Success || env.Message != "request body too large" {
		t.Errorf("envelope = %+v, want the ErrorHandler's failure", env)
	}

	// Far over the limit: usually a reset, depending on socket buffers.
	if resp, err := post(8 << 20); err != nil {
		t.Logf("an 8 MiB body got a transport error, as uploads "+
			"usually will: %v", err)
	} else {
		_ = resp.Body.Close()
		t.Logf("an 8 MiB body got status %d — this system's socket "+
			"buffers absorbed it", resp.StatusCode)
	}

	mu.Lock()
	defer mu.Unlock()
	if handled < 2 {
		t.Errorf("the ErrorHandler ran %d time(s), want once per "+
			"request", handled)
	}
}
