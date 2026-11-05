package fibertest

import (
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
)

// Send runs req through app in process, as app.Test does, and returns
// the status app answered with and the whole body, read and closed. It
// fails the test when app cannot serve req or the body cannot be read,
// naming the request, so a suite is handed the reply and nothing left
// to clean up.
func Send(t *testing.T, app *fiber.App, req *http.Request) (int, []byte) {
	t.Helper()
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.URL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s: reading the body: %v", req.Method, req.URL, err)
	}
	return resp.StatusCode, body
}

// Serve starts app on a loopback port, shuts it down when the test ends,
// and returns its base URL, "http://127.0.0.1:<port>", once the
// application has reached the point where it serves.
//
// The wait is on Fiber's OnListen hook rather than on a request, because a
// request would reach the application's own handlers, error handler
// included, which is often exactly what a test is counting. The listener
// is already open when the hook runs, so a request sent the moment Serve
// returns is queued by the kernel rather than refused.
func Serve(t *testing.T, app *fiber.App) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	listening := make(chan struct{})
	app.Hooks().OnListen(func(fiber.ListenData) error {
		close(listening)
		return nil
	})
	go func() { _ = app.Listener(ln) }()
	t.Cleanup(func() { _ = app.Shutdown() })

	select {
	case <-listening:
	case <-time.After(3 * time.Second):
		t.Fatal("the application never started listening")
	}
	return "http://" + ln.Addr().String()
}
