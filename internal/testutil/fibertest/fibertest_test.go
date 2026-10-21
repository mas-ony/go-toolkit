package fibertest

// Tests for fibertest.go: a served application answers on the URL Serve
// returns, and nothing reached its handlers before the test's own request.

import (
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gofiber/fiber/v3"
)

// The first request the application sees is the test's, and it is answered
// on the URL Serve handed back.
func TestServe(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		hits.Add(1)
		return c.Next()
	})
	app.Get("/ping", func(c fiber.Ctx) error { return c.SendString("pong") })

	base := Serve(t, app)
	if !strings.HasPrefix(base, "http://127.0.0.1:") {
		t.Fatalf("Serve returned %q", base)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("the application saw %d requests before the test's", n)
	}

	resp, err := http.Get(base + "/ping")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "pong" {
		t.Errorf("GET /ping = %d %q", resp.StatusCode, body)
	}
}
