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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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

// A body over BodyLimit, which is the framework failure whose path is
// least obvious and the reason doc.go describes the claim in terms of the
// ErrorHandler rather than in terms of what a client receives.
//
// The ErrorHandler DOES run, with fiber.ErrRequestEntityTooLarge, so the
// envelope is built. What is not guaranteed is that the client reads it:
// the request was refused partway through being sent, and an in-process
// transport can fail the write before the response comes back. A client
// that streams a large upload may therefore see a transport error where
// the server logged a perfectly well-formed 413.
//
// Both halves are asserted, because a caller planning an upload endpoint
// needs to know that "the envelope covers it" is a statement about the
// server and not a promise about the client.
func TestIntegrationBodyLimitReachesTheErrorHandler(t *testing.T) {
	var gotCode int
	var gotErr error

	app := fiber.New(fiber.Config{
		BodyLimit: 32,
		ErrorHandler: func(c fiber.Ctx, err error) error {
			gotErr = err
			gotCode = fiber.StatusInternalServerError
			var fe *fiber.Error
			if errors.As(err, &fe) {
				gotCode = fe.Code
			}
			return c.Status(gotCode).JSON(Fail("request body too large"))
		},
	})
	app.Post("/items", func(c fiber.Ctx) error {
		t.Error("the handler ran despite an over-limit body")
		return nil
	})

	req := httptest.NewRequest(http.MethodPost, "/items",
		strings.NewReader(strings.Repeat("x", 512)))
	req.Header.Set("Content-Type", "application/json")

	resp, testErr := app.Test(req, fiber.TestConfig{Timeout: 0})
	if resp != nil {
		_ = resp.Body.Close()
	}

	if gotErr == nil {
		t.Fatal("the ErrorHandler never ran for an over-limit body")
	}
	if gotCode != http.StatusRequestEntityTooLarge {
		t.Errorf("ErrorHandler saw %d, want %d",
			gotCode, http.StatusRequestEntityTooLarge)
	}
	// The other half: the response may not survive the transport. Not a
	// failure — just the fact a caller has to plan around.
	if testErr != nil {
		t.Logf("the envelope was built but did not reach the client: %v",
			testErr)
	}
}
