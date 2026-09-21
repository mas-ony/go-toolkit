package httpclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/mas-ony/go-toolkit/config"
	"github.com/mas-ony/go-toolkit/response"
)

// writeEnvelope answers in the shape this package decodes, so the tests
// run against a real server and a real client rather than a mock of
// either.
func writeEnvelope(
	w http.ResponseWriter, status int, msg string, data any,
) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"success": status < 300, "message": msg, "data": data,
	})
}

func testClient(
	t *testing.T, h http.Handler,
	cfg config.ClientConfig, opt Options,
) *Client {
	t.Helper()

	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	cfg.BaseURL = srv.URL
	opt.Log = zerolog.New(io.Discard)
	c, err := New(&cfg, opt)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// The base URL should tolerate the form somebody pastes from a browser.
func TestNewNormalisesBaseURL(t *testing.T) {
	for _, in := range []string{
		"https://host.example.go.id",
		"https://host.example.go.id/",
		"https://host.example.go.id/api/v1",
		"https://host.example.go.id/api/v1/",
	} {
		c, err := New(&config.ClientConfig{BaseURL: in},
			Options{TrimPathSuffix: "/api/v1"})
		if err != nil {
			t.Fatalf("New(%q): %v", in, err)
		}
		got := c.URL("/api/v1/thing", nil)
		want := "https://host.example.go.id/api/v1/thing"
		if got != want {
			t.Errorf("New(%q) builds %q, want %q", in, got, want)
		}
	}

	for _, bad := range []string{"", "  ", "host.example.go.id",
		"ftp://host/x"} {
		if _, err := New(&config.ClientConfig{BaseURL: bad},
			Options{}); err == nil {
			t.Errorf("New(%q) should have failed", bad)
		}
	}
}

// A nil section is a wiring mistake, and it reaches the first field read
// before anything else in New. It gets the same shape of error every
// section's own Validate returns for one.
func TestNewRejectsANilSection(t *testing.T) {
	if _, err := New(nil, Options{}); err == nil {
		t.Fatal("expected an error for a nil *config.ClientConfig")
	}
}

// The error names no flag, because the package does not know what
// supplied the URL. A caller with one to blame wraps this.
func TestNewErrorNamesNoSetting(t *testing.T) {
	_, err := New(&config.ClientConfig{BaseURL: ""}, Options{})
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "-") {
		t.Errorf("error mentions a flag: %q", err)
	}
}

// A transient status is worth another attempt; a deterministic one is
// not, and repeating it only delays the same answer.
func TestRetriesTransientStatusesOnly(t *testing.T) {
	var flaky, bad int
	mux := http.NewServeMux()
	mux.HandleFunc("/flaky", func(
		w http.ResponseWriter, r *http.Request,
	) {
		flaky++
		if flaky < 3 {
			writeEnvelope(w, http.StatusServiceUnavailable,
				"database unreachable", nil)
			return
		}
		writeEnvelope(w, http.StatusOK, "ready", nil)
	})
	mux.HandleFunc("/bad", func(
		w http.ResponseWriter, r *http.Request,
	) {
		bad++
		writeEnvelope(w, http.StatusBadRequest,
			"invalid request body", nil)
	})

	c := testClient(t, mux, config.ClientConfig{Retries: 2}, Options{})
	ctx := context.Background()

	if err := c.Do(ctx, http.MethodGet, c.URL("/flaky", nil),
		nil, "", nil); err != nil {
		t.Fatalf("should have succeeded on the third attempt: %v", err)
	}
	if flaky != 3 {
		t.Errorf("called %d times, want 3 (two 503s then success)",
			flaky)
	}

	err := c.Do(ctx, http.MethodGet, c.URL("/bad", nil), nil, "", nil)
	if err == nil {
		t.Fatal("expected the 400 to fail")
	}
	if bad != 1 {
		t.Errorf("attempted %d times, want 1 — a 4xx is deterministic",
			bad)
	}
	if !strings.Contains(err.Error(), "invalid request body") {
		t.Errorf("error should carry the server's message, got %q", err)
	}
}

// A method the caller has excluded gets one attempt however retryable
// the failure looks. POST is excluded by default.
func TestNoRetryMethodsGetOneAttempt(t *testing.T) {
	var posts int
	mux := http.NewServeMux()
	mux.HandleFunc("/thing", func(
		w http.ResponseWriter, r *http.Request,
	) {
		posts++
		writeEnvelope(w, http.StatusServiceUnavailable,
			"database unreachable", nil)
	})

	c := testClient(t, mux, config.ClientConfig{Retries: 2}, Options{})
	err := c.Do(context.Background(), http.MethodPost,
		c.URL("/thing", nil), []byte("{}"), "application/json", nil)
	if err == nil {
		t.Fatal("expected the 503 to fail")
	}
	if posts != 1 {
		t.Errorf("POST attempted %d times, want 1 by default", posts)
	}
}

// An explicitly empty list replays everything, which is the opposite of
// the nil default and has to be asked for.
func TestEmptyNoRetryMethodsReplaysEverything(t *testing.T) {
	var posts int
	var bodies []string
	mux := http.NewServeMux()
	mux.HandleFunc("/thing", func(
		w http.ResponseWriter, r *http.Request,
	) {
		posts++
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		writeEnvelope(w, http.StatusServiceUnavailable, "later", nil)
	})

	c := testClient(t, mux, config.ClientConfig{
		Retries:        1,
		NoRetryMethods: []string{},
	}, Options{})
	err := c.Do(context.Background(), http.MethodPost,
		c.URL("/thing", nil), []byte(`{"id":1}`),
		"application/json", nil)
	if err == nil {
		t.Fatal("expected the 503 to fail")
	}
	if posts != 2 {
		t.Errorf("POST attempted %d times, want 2", posts)
	}
	// The replay carries the body again, which is the whole reason Do
	// takes bytes rather than a reader.
	for i, b := range bodies {
		if b != `{"id":1}` {
			t.Errorf("attempt %d sent %q", i+1, b)
		}
	}
}

// The three statuses a caller branches on reach errors.Is; everything
// else stays a plain *Error.
func TestErrorUnwrapsToTheSentinels(t *testing.T) {
	mux := http.NewServeMux()
	for path, status := range map[string]int{
		"/missing":     http.StatusNotFound,
		"/conflict":    http.StatusConflict,
		"/unprocessed": http.StatusUnprocessableEntity,
		"/bad":         http.StatusBadRequest,
	} {
		mux.HandleFunc(path, func(
			w http.ResponseWriter, r *http.Request,
		) {
			writeEnvelope(w, status, "no", nil)
		})
	}

	c := testClient(t, mux, config.ClientConfig{}, Options{})
	ctx := context.Background()

	cases := []struct {
		path string
		want error
	}{
		{"/missing", ErrNotFound},
		{"/conflict", ErrConflict},
		{"/unprocessed", ErrUnprocessable},
	}
	for _, tc := range cases {
		err := c.Do(ctx, http.MethodGet, c.URL(tc.path, nil),
			nil, "", nil)
		if !errors.Is(err, tc.want) {
			t.Errorf("%s = %v, want %v", tc.path, err, tc.want)
		}
	}

	err := c.Do(ctx, http.MethodGet, c.URL("/bad", nil), nil, "", nil)
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("400 gave %T, want an *Error", err)
	}
	if apiErr.Status != http.StatusBadRequest {
		t.Errorf("Status = %d, want 400", apiErr.Status)
	}
	if errors.Is(err, ErrNotFound) {
		t.Error("a 400 must not unwrap to a sentinel")
	}
}

func TestHeadersAndTokenAreSent(t *testing.T) {
	var auth, extra, accept, agent, ctype string
	mux := http.NewServeMux()
	mux.HandleFunc("/thing", func(
		w http.ResponseWriter, r *http.Request,
	) {
		auth = r.Header.Get("Authorization")
		extra = r.Header.Get("X-Api-Key")
		accept = r.Header.Get("Accept")
		agent = r.Header.Get("User-Agent")
		ctype = r.Header.Get("Content-Type")
		writeEnvelope(w, http.StatusOK, "ok", nil)
	})

	c := testClient(t, mux,
		config.ClientConfig{Token: "test-token"},
		Options{Headers: map[string]string{
			"X-Api-Key":  "abc123",
			"User-Agent": "importer/1.0",
		}})
	err := c.Do(context.Background(), http.MethodPost,
		c.URL("/thing", nil), []byte("{}"), "application/json", nil)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}

	if auth != "Bearer test-token" {
		t.Errorf("Authorization = %q", auth)
	}
	if extra != "abc123" {
		t.Errorf("X-Api-Key = %q", extra)
	}
	if accept != "application/json" {
		t.Errorf("Accept = %q", accept)
	}
	// The transport writes its own User-Agent unless the caller's is
	// routed past the header map.
	if agent != "importer/1.0" {
		t.Errorf("User-Agent = %q", agent)
	}
	if ctype != "application/json" {
		t.Errorf("Content-Type = %q", ctype)
	}
}

// A typed response and a page both decode straight into the caller's
// type, which is the reason Envelope carries a parameter rather than an
// any.
func TestDecodesIntoTheCallersType(t *testing.T) {
	type thing struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/one", func(
		w http.ResponseWriter, r *http.Request,
	) {
		writeEnvelope(w, http.StatusOK, "ok", thing{ID: 42, Name: "x"})
	})
	mux.HandleFunc("/many", func(
		w http.ResponseWriter, r *http.Request,
	) {
		writeEnvelope(w, http.StatusOK, "ok", map[string]any{
			"items": []thing{{ID: 7}, {ID: 9}},
			"total": 12, "page": 1, "limit": 2,
		})
	})

	c := testClient(t, mux, config.ClientConfig{}, Options{})
	ctx := context.Background()

	var one Envelope[thing]
	if err := c.Do(ctx, http.MethodGet, c.URL("/one", nil),
		nil, "", &one); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if one.Data.ID != 42 || one.Data.Name != "x" {
		t.Errorf("decoded %+v", one.Data)
	}

	var many Envelope[response.Paginated[thing]]
	if err := c.Do(ctx, http.MethodGet, c.URL("/many", nil),
		nil, "", &many); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if len(many.Data.Items) != 2 || many.Data.Total != 12 {
		t.Errorf("decoded %+v", many.Data)
	}
}

// The query a caller builds survives the trip to the server.
func TestQueryReachesTheServer(t *testing.T) {
	var got string
	mux := http.NewServeMux()
	mux.HandleFunc("/list", func(
		w http.ResponseWriter, r *http.Request,
	) {
		got = r.URL.Query().Get("name")
		writeEnvelope(w, http.StatusOK, "ok", nil)
	})

	c := testClient(t, mux, config.ClientConfig{}, Options{})
	u := c.URL("/list", url.Values{"name": {"village hall"}})
	if err := c.Do(context.Background(), http.MethodGet, u,
		nil, "", nil); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if got != "village hall" {
		t.Errorf("name = %q, want village hall", got)
	}
}

// A deadline is the caller's, and the error it produces has to be the
// one a caller can branch on.
func TestContextDeadlineIsReported(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/slow", func(
		w http.ResponseWriter, r *http.Request,
	) {
		time.Sleep(300 * time.Millisecond)
		writeEnvelope(w, http.StatusOK, "late", nil)
	})

	c := testClient(t, mux, config.ClientConfig{Retries: 2}, Options{})
	ctx, cancel := context.WithTimeout(
		context.Background(), 50*time.Millisecond)
	defer cancel()

	err := c.Do(ctx, http.MethodGet, c.URL("/slow", nil),
		nil, "", nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Do = %v, want a deadline error", err)
	}
	if !strings.Contains(err.Error(), "/slow") {
		t.Errorf("error should name the call, got %q", err)
	}
}

// A gateway in front of the service answers with HTML rather than the
// envelope, and an unparsed error page is still the most informative
// thing available — collapsed and truncated, because a whole page of it
// in a per-row report is not.
//
// The truncation is asserted on the rune count, matching messageFrom: a
// byte cut would split a multi-byte character in a non-ASCII page.
func TestMessageFromFallsBackToTheBody(t *testing.T) {
	if got := messageFrom([]byte(
		`{"success":false,"message":"nope","data":null}`,
	)); got != "nope" {
		t.Errorf("envelope message = %q, want nope", got)
	}

	html := "<html>\n  <body>  502 Bad Gateway  </body>\n</html>"
	if got := messageFrom([]byte(html)); got !=
		"<html> <body> 502 Bad Gateway </body> </html>" {
		t.Errorf("collapsed to %q", got)
	}

	long := messageFrom([]byte(strings.Repeat("x", 500)))
	if len(long) != 203 || !strings.HasSuffix(long, "...") {
		t.Errorf("truncated to %d chars: %q", len(long), long)
	}

	if got := messageFrom(nil); got != "" {
		t.Errorf("empty body = %q, want empty", got)
	}
}
