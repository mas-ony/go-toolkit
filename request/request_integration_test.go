//go:build integration

package request

// Integration tests for the cloning rule.
//
// request_test.go already drives every helper through a real router, so
// "integration" here is not about using the framework. It is about using
// it the one way that can expose the hazard: over a REAL LISTENER, with
// keep-alive, for enough sequential requests that the framework recycles
// the buffers it parsed them into.
//
// That distinction is the whole reason this file exists, and it was
// established by trying the easy way first. The framework's in-process
// test transport gives every call a fresh context, so a test written
// against it passes with the clones in request.go and passes just as
// happily with them deleted — which is worse than no test, because it
// looks like coverage. Against a real listener the same code corrupts 199
// of 200 captured values.
//
// So every test below holds what a handler produced, keeps issuing
// requests on the same connections, and then checks what it held. Remove
// a strings.Clone from request.go and the matching test fails by a wide
// margin rather than marginally.
//
//	go test -tags integration -run Integration ./request
//
// It binds a listener on 127.0.0.1 with a port the operating system
// chooses, and needs nothing else.

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
)

// requests is how many sequential calls each test makes. It has to be
// comfortably more than the connection pool holds, because corruption only
// appears once a buffer is handed to a SECOND request.
const requests = 200

// harness is one application on a real port, plus a client that reuses
// connections.
type harness struct {
	base   string
	client *http.Client
	app    *fiber.App
}

// capture collects one value per request under a lock, since the framework
// may serve them on more than one goroutine.
type capture struct {
	mu   sync.Mutex
	vals []string
}

// serve starts app on a loopback port and returns a harness for it.
//
// Keep-alive is left on, since it is what makes the framework reuse a
// context at all, and the idle pool is deliberately small so reuse starts
// early rather than after hundreds of requests.
func serve(t *testing.T, app *fiber.App) *harness {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = app.Listener(ln) }()

	h := &harness{
		base: "http://" + ln.Addr().String(),
		app:  app,
		client: &http.Client{
			Timeout:   5 * time.Second,
			Transport: &http.Transport{MaxIdleConnsPerHost: 4},
		},
	}
	t.Cleanup(func() {
		h.client.CloseIdleConnections()
		_ = app.Shutdown()
	})

	h.waitReady(t)
	return h
}

// waitReady covers the gap between Listen returning and the serving
// goroutine being scheduled.
func (h *harness) waitReady(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := h.client.Get(h.base + "/ready")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the application never became reachable")
}

// get issues one request and drains the body, which is what returns the
// connection to the pool so the next request can reuse the buffer behind
// it.
func (h *harness) get(t *testing.T, path string) {
	t.Helper()
	resp, err := h.client.Get(h.base + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

// ready registers the endpoint waitReady polls.
func ready(app *fiber.App) {
	app.Get("/ready", func(c fiber.Ctx) error { return c.SendString("ok") })
}

func (c *capture) add(s string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.vals = append(c.vals, s)
}

// check reports how many held values no longer match what their own
// request sent.
func (c *capture) check(t *testing.T, what string, want func(int) string) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.vals) != requests {
		t.Fatalf("%s: captured %d values, want %d", what, len(c.vals),
			requests)
	}
	bad := 0
	for i, got := range c.vals {
		if got != want(i) {
			if bad < 3 {
				t.Errorf("%s[%d] = %q, want %q", what, i, got, want(i))
			}
			bad++
		}
	}
	if bad > 0 {
		t.Errorf("%s: %d of %d held values were overwritten by later "+
			"requests — the value is not being cloned", what, bad,
			requests)
	}
}

// StringPtr is the most dangerous of the three, because a *string is
// exactly what a caller stores on a filter struct and carries past the
// handler that built it.
func TestIntegrationStringPtrSurvivesBufferReuse(t *testing.T) {
	var held capture

	app := fiber.New()
	ready(app)
	app.Get("/c", func(c fiber.Ctx) error {
		if p := StringPtr(c, "code"); p != nil {
			held.add(*p)
		} else {
			held.add("<nil>")
		}
		return c.SendString("ok")
	})

	h := serve(t, app)
	for i := 0; i < requests; i++ {
		h.get(t, fmt.Sprintf("/c?code=INV-%04d", i))
	}

	held.check(t, "StringPtr", func(i int) string {
		return fmt.Sprintf("INV-%04d", i)
	})
}

// Cols clones every token, so a handler storing the slice keeps its own
// copy of each name rather than a window onto the request buffer.
func TestIntegrationColsSurviveBufferReuse(t *testing.T) {
	var held capture

	app := fiber.New()
	ready(app)
	app.Get("/c", func(c fiber.Ctx) error {
		cols := Cols(c)
		if len(cols) == 0 {
			held.add("<none>")
			return c.SendString("ok")
		}
		// The first token only: enough to detect corruption, and it
		// keeps the failure message readable.
		held.add(cols[0])
		return c.SendString("ok")
	})

	h := serve(t, app)
	for i := 0; i < requests; i++ {
		h.get(t, fmt.Sprintf("/c?cols=col_%04d,name,id", i))
	}

	held.check(t, "Cols", func(i int) string {
		return fmt.Sprintf("col_%04d", i)
	})
}

// Sort clones the column name. The direction is one of two constants this
// package writes itself, so Col is the only field that can be corrupted.
func TestIntegrationSortColumnsSurviveBufferReuse(t *testing.T) {
	var held capture

	app := fiber.New()
	ready(app)
	app.Get("/c", func(c fiber.Ctx) error {
		fields := Sort(c)
		if len(fields) == 0 {
			held.add("<none>")
			return c.SendString("ok")
		}
		held.add(fields[0].Col + ":" + fields[0].Dir)
		return c.SendString("ok")
	})

	h := serve(t, app)
	for i := 0; i < requests; i++ {
		h.get(t, fmt.Sprintf("/c?sort=t.col_%04d:desc,t.id:asc", i))
	}

	held.check(t, "Sort", func(i int) string {
		return fmt.Sprintf("t.col_%04d:DESC", i)
	})
}

// The helpers that return integers, or pointers to fresh locals, cannot be
// corrupted. Pinning that is worth as much as the tests above: it is why
// they carry no strings.Clone, and why adding one would be noise rather
// than safety.
func TestIntegrationValueHelpersNeedNoClone(t *testing.T) {
	type snap struct {
		ids   []int
		unit  *int
		flag  *bool
		limit int
	}
	var mu sync.Mutex
	var held []snap

	app := fiber.New()
	ready(app)
	app.Get("/c", func(c fiber.Ctx) error {
		s := snap{
			ids:  IDs(c, "ids"),
			unit: IntPtr(c, "unit_id"),
			flag: BoolPtr(c, "has_file"),
		}
		_, s.limit = Page(c)
		mu.Lock()
		held = append(held, s)
		mu.Unlock()
		return c.SendString("ok")
	})

	h := serve(t, app)
	for i := 0; i < requests; i++ {
		h.get(t, fmt.Sprintf(
			"/c?ids=%d,%d&unit_id=%d&has_file=%v&limit=%d",
			i+1, i+2, i+1, i%2 == 0, (i%50)+1))
	}

	mu.Lock()
	defer mu.Unlock()
	if len(held) != requests {
		t.Fatalf("captured %d snapshots, want %d", len(held), requests)
	}
	for i, s := range held {
		if len(s.ids) != 2 || s.ids[0] != i+1 || s.ids[1] != i+2 {
			t.Fatalf("held[%d].ids = %v, want [%d %d]", i, s.ids,
				i+1, i+2)
		}
		if s.unit == nil || *s.unit != i+1 {
			t.Fatalf("held[%d].unit = %v, want %d", i, s.unit, i+1)
		}
		if s.flag == nil || *s.flag != (i%2 == 0) {
			t.Fatalf("held[%d].flag = %v, want %v", i, s.flag,
				i%2 == 0)
		}
		if want := (i % 50) + 1; s.limit != want {
			t.Fatalf("held[%d].limit = %d, want %d", i, s.limit, want)
		}
	}
}

// The paging ceiling is package state read on every call, so raising it
// takes effect for requests already arriving at a live application. This
// is the one test here about the variables rather than the buffer.
func TestIntegrationMaxLimitAppliesAcrossRequests(t *testing.T) {
	// Not parallel, and restored on the way out: these are the
	// process-wide variables doc.go warns about, and leaving them changed
	// would quietly rewrite what every later test expects.
	oldMax, oldDefault := MaxLimit, DefaultLimit
	t.Cleanup(func() { MaxLimit, DefaultLimit = oldMax, oldDefault })

	var mu sync.Mutex
	var limit int

	app := fiber.New()
	ready(app)
	app.Get("/p", func(c fiber.Ctx) error {
		mu.Lock()
		_, limit = Page(c)
		mu.Unlock()
		return c.SendString("ok")
	})

	h := serve(t, app)
	read := func(path string) int {
		h.get(t, path)
		mu.Lock()
		defer mu.Unlock()
		return limit
	}

	if got := read("/p?limit=250"); got != DefaultLimit {
		t.Errorf("limit = %d over the ceiling, want the default %d",
			got, DefaultLimit)
	}

	MaxLimit = 500
	if got := read("/p?limit=250"); got != 250 {
		t.Errorf("limit = %d after raising MaxLimit, want 250", got)
	}
	// Still corrected above the NEW ceiling, which is what shows the
	// variable is read per call rather than captured at startup.
	if got := read("/p?limit=600"); got != DefaultLimit {
		t.Errorf("limit = %d over the new ceiling, want the default %d",
			got, DefaultLimit)
	}
}
