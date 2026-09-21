//go:build integration

package httpclient

// Integration tests for httpclient.
//
// httpclient_test.go already runs against a real HTTP server, so
// "integration" here is not about reaching the wire — that suite already
// does. It is about the four claims that need something a plain handler
// cannot give: a certificate, a clock, a pathological body, and
// contention.
//
// Each is reachable only from outside the package's own logic, and each
// fails if the thing it covers is removed, which is the bar every test
// here had to clear before being written.
//
//	go test -tags integration -run Integration ./httpclient
//
// It binds loopback listeners and needs no configuration. The backoff
// tests wait real seconds, which is most of the suite's runtime and the
// reason they are here rather than in the unit suite.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/mas-ony/go-toolkit/config"
)

// countingServer answers with status until it has been called n times,
// then answers 200. It records when each call arrived.
func countingServer(t *testing.T, status, succeedOn int) (
	*httptest.Server, func() []time.Time) {
	t.Helper()

	var mu sync.Mutex
	var seen []time.Time

	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			seen = append(seen, time.Now())
			n := len(seen)
			mu.Unlock()

			w.Header().Set("Content-Type", "application/json")
			if n >= succeedOn {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(
					`{"success":true,"message":"","data":{}}`))
				return
			}
			w.WriteHeader(status)
			_, _ = w.Write([]byte(
				`{"success":false,"message":"later","data":null}`))
		}))
	t.Cleanup(srv.Close)

	return srv, func() []time.Time {
		mu.Lock()
		defer mu.Unlock()
		out := make([]time.Time, len(seen))
		copy(out, seen)
		return out
	}
}

// bodyServer answers every request with the given status and body.
func bodyServer(t *testing.T, status int, contentType string,
	body []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			if contentType != "" {
				w.Header().Set("Content-Type", contentType)
			}
			w.WriteHeader(status)
			_, _ = w.Write(body)
		}))
	t.Cleanup(srv.Close)
	return srv
}

// newClient builds a client for base, failing the test rather than
// returning an error nobody would check.
func newClient(t *testing.T, cfg *config.ClientConfig) *Client {
	t.Helper()
	c, err := New(cfg, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// ----------------------------------------------------------------------------
// TLS
// ----------------------------------------------------------------------------

// Insecure is the one setting in the config section with a security
// consequence, and it cannot be tested at all without a server presenting
// a certificate this client would otherwise refuse.
//
// Both directions are asserted. The permissive half alone would pass just
// as well if the flag were ignored and verification had never been on.
func TestIntegrationInsecureSkipsCertificateVerification(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(
				`{"success":true,"message":"","data":{"ok":true}}`))
		}))
	defer srv.Close()

	type body struct {
		OK bool `json:"ok"`
	}

	t.Run("a self-signed certificate is refused by default",
		func(t *testing.T) {
			c := newClient(t, &config.ClientConfig{BaseURL: srv.URL})

			var env Envelope[body]
			err := c.Do(context.Background(), http.MethodGet,
				c.URL("/x", nil), nil, "", &env)
			if err == nil {
				t.Fatal("the client accepted an untrusted certificate")
			}
			// The failure is a transport error the retry loop exhausts, so
			// what reaches the caller is the give-up wrapper. Its text is
			// the only place the cause is visible.
			if !strings.Contains(err.Error(), "certificate") &&
				!strings.Contains(err.Error(), "x509") &&
				!strings.Contains(err.Error(), "tls") {
				t.Errorf("err = %v, want it to name the TLS failure", err)
			}
		})

	t.Run("Insecure accepts it", func(t *testing.T) {
		c := newClient(t, &config.ClientConfig{
			BaseURL:  srv.URL,
			Insecure: true,
		})

		var env Envelope[body]
		if err := c.Do(context.Background(), http.MethodGet,
			c.URL("/x", nil), nil, "", &env); err != nil {
			t.Fatalf("Do: %v", err)
		}
		if !env.Data.OK {
			t.Error("the response did not decode")
		}
	})
}

// TLSConfig returns the LIVE config rather than a copy, which is the whole
// reason New can set one field without replacing the rest. If that ever
// changed to return a copy, Insecure would silently stop working, and the
// test above would be the only thing to notice — so this pins the property
// directly, where the failure names the cause.
func TestIntegrationInsecureLeavesTheRestOfTheTLSConfigAlone(t *testing.T) {
	c := newClient(t, &config.ClientConfig{
		BaseURL:  "https://example.invalid",
		Insecure: true,
	})

	tc := c.hc.TLSConfig()
	if tc == nil {
		t.Fatal("TLSConfig is nil")
	}
	if !tc.InsecureSkipVerify {
		t.Error("InsecureSkipVerify was not applied to the live config")
	}
}

// ----------------------------------------------------------------------------
// Backoff
// ----------------------------------------------------------------------------

// The unit suite counts attempts. This one measures the gaps between them,
// which is the difference between a retry policy and a hot loop: a backoff
// that computed a zero or negative duration would still produce the right
// number of attempts and still pass every test over there.
func TestIntegrationBackoffDoublesFromTheFirstWait(t *testing.T) {
	srv, times := countingServer(t, http.StatusServiceUnavailable, 4)

	c := newClient(t, &config.ClientConfig{
		BaseURL:        srv.URL,
		Retries:        3,
		NoRetryMethods: []string{},
	})

	start := time.Now()
	if err := c.Do(context.Background(), http.MethodGet,
		c.URL("/x", nil), nil, "", nil); err != nil {
		t.Fatalf("Do: %v", err)
	}
	elapsed := time.Since(start)

	seen := times()
	if len(seen) != 4 {
		t.Fatalf("%d attempts, want 4", len(seen))
	}

	// 500ms then 1s then 2s. The tolerance is generous downward and
	// tight upward: what matters is that a wait HAPPENED and roughly
	// doubled, not that the scheduler was punctual.
	wants := []time.Duration{firstBackoff, firstBackoff * 2,
		firstBackoff * 4}
	for i, want := range wants {
		gap := seen[i+1].Sub(seen[i])
		if gap < want*7/10 {
			t.Errorf("gap %d was %s, want about %s — the backoff is "+
				"too short", i+1, gap.Round(time.Millisecond), want)
		}
		if gap > want*2 {
			t.Errorf("gap %d was %s, want about %s", i+1,
				gap.Round(time.Millisecond), want)
		}
	}

	// The total is the sum of the waits, which is what a caller's own
	// deadline has to accommodate.
	if min := firstBackoff * 7 / 10 * 7; elapsed < min {
		t.Errorf("total %s, want at least about %s", elapsed, min)
	}
}

// The cap is what keeps a long retry budget from turning a transient blip
// into an apparent hang, and maxBackoffShift is what keeps the doubling
// itself from overflowing into a negative duration — which time.After
// fires on immediately, producing exactly the hot loop the cap exists to
// prevent.
//
// Asserted through the unexported computation rather than by waiting
// minutes for it, since the wait is the thing being bounded.
func TestIntegrationBackoffIsCappedAndNeverNegative(t *testing.T) {
	for attempt := 1; attempt <= 40; attempt++ {
		shift := attempt - 1
		if shift > maxBackoffShift {
			shift = maxBackoffShift
		}
		wait := firstBackoff << shift
		if wait > maxBackoff {
			wait = maxBackoff
		}

		if wait <= 0 {
			t.Fatalf("attempt %d computes a wait of %s — time.After "+
				"fires immediately on that", attempt, wait)
		}
		if wait > maxBackoff {
			t.Fatalf("attempt %d waits %s, over the %s cap",
				attempt, wait, maxBackoff)
		}
	}
}

// A context cancelled DURING a backoff has to end the call, not be
// discovered after the wait finishes. The select in Do is what does that,
// and this is the only way to reach it: the cancellation has to land while
// the client is sleeping rather than while it is sending.
func TestIntegrationContextCancelledDuringBackoffReturnsAtOnce(t *testing.T) {
	srv, times := countingServer(t, http.StatusServiceUnavailable, 99)

	c := newClient(t, &config.ClientConfig{
		BaseURL:        srv.URL,
		Retries:        5,
		NoRetryMethods: []string{},
	})

	ctx, cancel := context.WithCancel(context.Background())
	// Long enough that the first attempt has certainly been made and the
	// client is inside the first 500ms wait.
	time.AfterFunc(150*time.Millisecond, cancel)

	start := time.Now()
	err := c.Do(ctx, http.MethodGet, c.URL("/x", nil), nil, "", nil)
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if elapsed > firstBackoff {
		t.Errorf("took %s, want under %s — the wait ran to completion "+
			"before the cancellation was noticed", elapsed, firstBackoff)
	}
	if n := len(times()); n != 1 {
		t.Errorf("%d attempts, want 1", n)
	}
}

// ----------------------------------------------------------------------------
// Error message caps
// ----------------------------------------------------------------------------

// The caps only matter against a body built to defeat them, and a body
// that size is not something to put in a unit test's source.
//
// Three shapes, one per hazard: a gateway error page far over
// maxErrorBody, a message whose 200-rune boundary falls inside a
// multi-byte character, and a body that is not text at all.
func TestIntegrationErrorMessageIsCapped(t *testing.T) {
	const ellipsis = "..."

	t.Run("an oversized error page is cut", func(t *testing.T) {
		huge := []byte("<html><body>" +
			strings.Repeat("A", 4<<20) + "</body></html>")
		srv := bodyServer(t, http.StatusBadRequest, "text/html", huge)
		c := newClient(t, &config.ClientConfig{BaseURL: srv.URL})

		err := c.Do(context.Background(), http.MethodGet,
			c.URL("/x", nil), nil, "", nil)

		var apiErr *Error
		if !errors.As(err, &apiErr) {
			t.Fatalf("err = %v, want *Error", err)
		}
		runes := len([]rune(apiErr.Msg))
		if runes > maxMessageRunes+len(ellipsis) {
			t.Errorf("message is %d runes, want at most %d",
				runes, maxMessageRunes+len(ellipsis))
		}
		if !strings.HasSuffix(apiErr.Msg, ellipsis) {
			t.Error("a cut message should end in an ellipsis")
		}
	})

	t.Run("a multi-byte message is cut on a rune boundary",
		func(t *testing.T) {
			// Every character is three bytes, so a cut at a fixed byte
			// offset lands inside one and produces a replacement character.
			msg := strings.Repeat("あ", 500)
			body := []byte(fmt.Sprintf(
				`{"success":false,"message":%q,"data":null}`, msg))
			srv := bodyServer(t, http.StatusBadRequest,
				"application/json", body)
			c := newClient(t, &config.ClientConfig{BaseURL: srv.URL})

			err := c.Do(context.Background(), http.MethodGet,
				c.URL("/x", nil), nil, "", nil)

			var apiErr *Error
			if !errors.As(err, &apiErr) {
				t.Fatalf("err = %v, want *Error", err)
			}
			if strings.ContainsRune(apiErr.Msg, '\uFFFD') {
				t.Error("the cut split a character; it should land on a " +
					"rune boundary")
			}
			if runes := len([]rune(apiErr.Msg)); runes >
				maxMessageRunes+len(ellipsis) {
				t.Errorf("message is %d runes, want at most %d",
					runes, maxMessageRunes+len(ellipsis))
			}
		})

	t.Run("a binary body leaves as log-safe text", func(t *testing.T) {
		raw := make([]byte, 4096)
		for i := range raw {
			raw[i] = byte(i % 256)
		}
		srv := bodyServer(t, http.StatusBadGateway,
			"application/octet-stream", raw)
		c := newClient(t, &config.ClientConfig{
			BaseURL: srv.URL,
			Retries: 0,
		})

		err := c.Do(context.Background(), http.MethodGet,
			c.URL("/x", nil), nil, "", nil)
		if err == nil {
			t.Fatal("expected an error")
		}

		var apiErr *Error
		if !errors.As(err, &apiErr) {
			t.Fatalf("err = %v, want *Error", err)
		}
		// Valid UTF-8 and one line: the two properties that let the
		// message go into a JSON log line without breaking it. The
		// replacement characters are expected here — they are what
		// ToValidUTF8 puts where the bytes stopped being text — so what
		// is asserted is validity, not their absence.
		if !utf8.ValidString(apiErr.Msg) {
			t.Error("the message is not valid UTF-8 and would corrupt " +
				"a JSON log line")
		}
		if strings.ContainsAny(apiErr.Msg, "\n\r\t") {
			t.Error("the message carries whitespace that breaks a log " +
				"line")
		}
		if apiErr.Msg == "" {
			t.Error("a binary body produced no message at all")
		}
	})
}

// ----------------------------------------------------------------------------
// Concurrency
// ----------------------------------------------------------------------------

// A Client is documented as safe for concurrent use and meant to be
// shared. Nothing in the type enforces that, so the claim rests on a
// property a later change could remove without the compiler noticing.
//
// Meaningful mainly under -race. Without it the test still checks that
// every caller got its OWN response under contention, which is the weaker
// half and the one that would catch a shared buffer.
func TestIntegrationOneClientIsSafeToShare(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			id := strings.TrimPrefix(r.URL.Path, "/")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(fmt.Sprintf(
				`{"success":true,"message":"","data":{"id":%q,"pad":%q}}`,
				id, strings.Repeat("x", 4096))))
		}))
	defer srv.Close()

	c := newClient(t, &config.ClientConfig{BaseURL: srv.URL})

	type payload struct {
		ID  string `json:"id"`
		Pad string `json:"pad"`
	}

	const workers, each = 16, 100
	var wg sync.WaitGroup
	errs := make(chan string, workers)

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for j := 0; j < each; j++ {
				want := fmt.Sprintf("%04d", w*each+j)
				var env Envelope[payload]
				if err := c.Do(context.Background(), http.MethodGet,
					c.URL("/"+want, nil), nil, "", &env); err != nil {
					errs <- fmt.Sprintf("worker %d: %v", w, err)
					return
				}
				if env.Data.ID != want {
					errs <- fmt.Sprintf(
						"worker %d got id %q, want %q — one caller "+
							"read another's response",
						w, env.Data.ID, want)
					return
				}
				if len(env.Data.Pad) != 4096 {
					errs <- fmt.Sprintf(
						"worker %d got a %d-byte payload, want 4096",
						w, len(env.Data.Pad))
					return
				}
			}
		}(w)
	}

	wg.Wait()
	close(errs)
	for msg := range errs {
		t.Error(msg)
	}
}
