//go:build integration

package telegram

// Integration tests for telegram.
//
// telegram_test.go already runs every send against a real HTTP server on
// loopback, so "integration" here is not about reaching the wire. It is
// about the claims that need what a plain server cannot give: a
// certificate, a clock, and contention.
//
//	go test -tags integration -run Integration ./telegram
//
// It binds loopback listeners, uses the certificate httptest makes in
// memory, and needs no configuration. The timeout tests wait a second
// each.

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mas-ony/go-toolkit/config"
)

// hang is a reply that never answers: it holds the request until the
// client gives up, and lets go after ten seconds if it never does, so a
// failing test cannot hold the server open.
func hang(_ http.ResponseWriter, r *http.Request) {
	select {
	case <-r.Context().Done():
	case <-time.After(10 * time.Second):
	}
}

// Over https the request arrives encrypted, to a server whose certificate
// the client has been told to trust.
func TestIntegrationSendOverTLS(t *testing.T) {
	b := (&bot{tls: true}).start(t)
	s := service(t, b, nil)
	s.client.Transport = b.srv.Client().Transport

	if err := s.SendText(t.Context(), "123456789", "x"); err != nil {
		t.Fatalf("SendText: %v", err)
	}
	if !b.only(t).secure {
		t.Error("the request arrived in clear text")
	}
}

// A server whose certificate nothing vouches for is refused before a byte
// of the request is sent, which is what keeps the token from it. The
// refusal does not quote the URL.
func TestIntegrationAnUntrustedCertificateIsRefused(t *testing.T) {
	b := (&bot{tls: true}).start(t)
	s := service(t, b, nil)

	err := s.SendText(t.Context(), "123456789", "x")
	var bad *tls.CertificateVerificationError
	if !errors.As(err, &bad) {
		t.Fatalf("err = %v, want a certificate verification error", err)
	}
	if n := len(b.seen()); n != 0 {
		t.Errorf("the API saw %d requests, want none", n)
	}
	if strings.Contains(err.Error(), testToken) {
		t.Errorf("err = %v, want it without the token", err)
	}
}

// The configured timeout ends a send that the API never answers, and the
// error says so in the terms a caller tests for.
func TestIntegrationTheTimeoutEndsASend(t *testing.T) {
	b := (&bot{reply: hang}).start(t)
	s := service(t, b, func(c *config.TelegramConfig) {
		c.Timeout = time.Second
	})

	start := time.Now()
	err := s.SendText(t.Context(), "123456789", "x")
	took := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if took < 900*time.Millisecond || took > 5*time.Second {
		t.Errorf("the send took %s, want about the 1s timeout", took)
	}
	if strings.Contains(err.Error(), testToken) {
		t.Errorf("err = %v, want it without the token", err)
	}
}

// A deadline on the context ends a send sooner than a timeout that is
// longer, which is the other half of "whichever ends first".
func TestIntegrationTheContextEndsASendBeforeTheTimeout(t *testing.T) {
	b := (&bot{reply: hang}).start(t)
	s := service(t, b, func(c *config.TelegramConfig) {
		c.Timeout = time.Minute
	})
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := s.SendText(ctx, "123456789", "x")
	took := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if took > 5*time.Second {
		t.Errorf("the send took %s, want it ended by the 100ms deadline",
			took)
	}
}

// Many goroutines sending at once through one Service each reach the API
// once, to their own chat. Run with -race, this is also the claim that a
// Service is safe for concurrent use.
func TestIntegrationConcurrentSendsAllArrive(t *testing.T) {
	const n = 20
	b := (&bot{}).start(t)
	s := service(t, b, nil)

	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Go(func() {
			chat := strconv.Itoa(1000 + i)
			errs <- s.SendText(t.Context(), chat, "hello "+chat)
		})
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Errorf("a concurrent send failed: %v", err)
		}
	}
	arrived := map[string]int{}
	for _, h := range b.seen() {
		arrived[h.form.Get("chat_id")]++
	}
	if len(arrived) != n {
		t.Errorf("%d chats were reached, want %d", len(arrived), n)
	}
	for chat, count := range arrived {
		if count != 1 {
			t.Errorf("chat %s was reached %d times, want once", chat, count)
		}
	}
}
