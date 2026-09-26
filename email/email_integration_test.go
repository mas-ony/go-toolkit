//go:build integration

package email

// Integration tests for email.
//
// email_test.go already runs every send against a real SMTP server on
// loopback, so "integration" here is not about reaching the wire. It is
// about the claims that need what a plain relay cannot give: a
// certificate, a clock, and contention.
//
//	go test -tags integration -run Integration ./email
//
// It binds loopback listeners, generates its certificate in memory, and
// needs no configuration. The timeout tests wait a second each.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mas-ony/go-toolkit/config"
)

// testCert is a self-signed certificate for 127.0.0.1 and no host name,
// made once per run. Naming only the IP is deliberate: a client that
// dials "localhost" has to be refused, which is what proves the host is
// the name the certificate is checked against.
var testCert = sync.OnceValues(func() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "relay.test"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(time.Hour),
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},

		KeyUsage: x509.KeyUsageDigitalSignature |
			x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},

		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl,
		&key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  key,
		Leaf:        leaf,
	}, nil
})

// serverTLS is the relay's half of the certificate.
func serverTLS(t *testing.T) *tls.Config {
	t.Helper()
	cert, err := testCert()
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}}
}

// trust makes s accept the test certificate, as a relay whose certificate
// comes from a real authority is accepted.
func trust(t *testing.T, s *Service) {
	t.Helper()
	cert, err := testCert()
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert.Leaf)
	s.tlsConf.RootCAs = pool
}

// The relay offers AUTH only once the connection is encrypted, as a
// submission server does, and records whether each step arrived over
// TLS. A client that logged in before upgrading would find nothing to log
// in with; one that upgraded late would be recorded doing it.
func TestIntegrationStartTLSComesBeforeTheLogin(t *testing.T) {
	t.Parallel()
	r := (&relay{
		tlsConf:      serverTLS(t),
		mechs:        []string{"PLAIN"},
		users:        map[string]string{"notif": "rahasia"},
		authAfterTLS: true,
	}).start(t)
	s := service(t, r, func(c *config.EmailConfig) {
		c.TLS = config.EmailTLSStartTLS
		login("notif", "rahasia")(c)
	})
	trust(t, s)

	if err := s.SendText(t.Context(), "budi@example.go.id", "s",
		"b"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	got := r.only(t)
	if got.user != "notif" || !got.authTLS || !got.mailTLS {
		t.Errorf("user %q, login over TLS %t, MAIL FROM over TLS %t; "+
			"want notif and both encrypted", got.user, got.authTLS,
			got.mailTLS)
	}
}

func TestIntegrationImplicitTLS(t *testing.T) {
	t.Parallel()
	r := (&relay{
		tlsConf:  serverTLS(t),
		implicit: true,
		mechs:    []string{"PLAIN"},
		users:    map[string]string{"notif": "rahasia"},
	}).start(t)
	s := service(t, r, func(c *config.EmailConfig) {
		c.TLS = config.EmailTLSImplicit
		login("notif", "rahasia")(c)
	})
	trust(t, s)

	if err := s.SendText(t.Context(), "budi@example.go.id", "s",
		"b"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := r.only(t); !got.tls || !got.authTLS || got.data == nil {
		t.Errorf("encrypted %t, login over TLS %t, delivered %t",
			got.tls, got.authTLS, got.data != nil)
	}
}

// A certificate nobody vouches for fails the send before anything is
// said over the connection, in both TLS modes, and insecure is what lets
// it through. A certificate for another name fails the same way even
// from a trusted authority.
func TestIntegrationTheCertificateIsVerified(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{
		config.EmailTLSStartTLS, config.EmailTLSImplicit,
	} {
		relayFor := func(t *testing.T) *relay {
			return (&relay{
				tlsConf:  serverTLS(t),
				implicit: mode == config.EmailTLSImplicit,
				mechs:    []string{"PLAIN"},
				users:    map[string]string{"notif": "rahasia"},
			}).start(t)
		}
		configure := func(change func(*config.EmailConfig)) func(
			*config.EmailConfig) {
			return func(c *config.EmailConfig) {
				c.TLS = mode
				login("notif", "rahasia")(c)
				if change != nil {
					change(c)
				}
			}
		}

		t.Run(mode+"/untrusted", func(t *testing.T) {
			t.Parallel()
			r := relayFor(t)
			s := service(t, r, configure(nil))

			err := s.SendText(t.Context(), "budi@example.go.id", "s", "b")
			var verr *tls.CertificateVerificationError
			if !errors.As(err, &verr) {
				t.Fatalf("err = %v, want a certificate verification "+
					"error", err)
			}
			for _, got := range r.seen() {
				if got.user != "" || got.from != "" {
					t.Errorf("sent over an unverified connection: "+
						"login %q, MAIL FROM %q", got.user, got.from)
				}
			}
		})

		t.Run(mode+"/insecure", func(t *testing.T) {
			t.Parallel()
			r := relayFor(t)
			s := service(t, r, configure(func(c *config.EmailConfig) {
				c.Insecure = true
			}))

			if err := s.SendText(t.Context(), "budi@example.go.id", "s",
				"b"); err != nil {
				t.Fatalf("Send with insecure: %v", err)
			}
			if r.only(t).data == nil {
				t.Error("the relay has no message")
			}
		})

		t.Run(mode+"/another name", func(t *testing.T) {
			t.Parallel()
			r := relayFor(t)
			s := service(t, r, configure(func(c *config.EmailConfig) {
				c.Host = "localhost"
			}))
			trust(t, s)

			err := s.SendText(t.Context(), "budi@example.go.id", "s", "b")
			var herr x509.HostnameError
			if !errors.As(err, &herr) {
				t.Fatalf("err = %v, want the name mismatch reported", err)
			}
		})
	}
}

// The configured timeout ends a send whichever step the relay stops
// answering at: before its greeting, and after the whole message, on a
// connection that has already been upgraded to TLS. The second is the
// case the connection-deadline design has to survive, since the deadline
// is set on the connection underneath the TLS one.
func TestIntegrationTheTimeoutEndsAStalledSend(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		relay *relay
		mode  string
		step  string
	}{
		{"no greeting", &relay{silent: true},
			config.EmailTLSNone, "greeting"},
		{"no verdict", &relay{tlsConf: serverTLS(t), stallAfterData: true},
			config.EmailTLSStartTLS, "end of DATA"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := c.relay.start(t)
			s := service(t, r, func(cfg *config.EmailConfig) {
				cfg.TLS = c.mode
				cfg.Timeout = time.Second
			})
			if s.tlsConf != nil {
				trust(t, s)
			}

			start := time.Now()
			err := s.SendText(t.Context(), "budi@example.go.id", "s", "b")
			elapsed := time.Since(start)

			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("err = %v, want context.DeadlineExceeded", err)
			}
			if !strings.Contains(err.Error(), c.step) {
				t.Errorf("err = %v, want it to name %s", err, c.step)
			}
			if elapsed < 900*time.Millisecond || elapsed > 5*time.Second {
				t.Errorf("the send took %s against a 1s timeout", elapsed)
			}
		})
	}
}

// Cancellation ends a send long before a generous timeout would, which is
// what a caller shutting down relies on.
func TestIntegrationCancellationEndsASend(t *testing.T) {
	t.Parallel()
	r := (&relay{silent: true}).start(t)
	s := service(t, r, func(c *config.EmailConfig) {
		c.Timeout = time.Minute
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	time.AfterFunc(100*time.Millisecond, cancel)

	start := time.Now()
	err := s.SendText(ctx, "budi@example.go.id", "s", "b")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("the send took %s to notice the cancellation", elapsed)
	}
}

// One Service, many goroutines, each message arriving whole and on its
// own connection. Run under -race, this is what the claim that a Service
// is read-only after New rests on.
func TestIntegrationConcurrentSends(t *testing.T) {
	t.Parallel()
	r := (&relay{}).start(t)
	s := service(t, r, nil)

	const senders = 25
	errs := make(chan error, senders)
	var wg sync.WaitGroup
	for i := range senders {
		wg.Go(func() {
			errs <- s.SendText(t.Context(), "budi@example.go.id",
				fmt.Sprintf("pesan %02d", i), "isi")
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("Send: %v", err)
		}
	}

	subjects := make(map[string]bool)
	for _, got := range r.seen() {
		subjects[read(t, got.data).Header.Get("Subject")] = true
	}
	if len(subjects) != senders {
		t.Errorf("the relay holds %d distinct messages, want %d",
			len(subjects), senders)
	}
}
