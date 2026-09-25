// Package httpclient talks JSON to an HTTP service that answers in a
// success/message/data envelope.
//
// It holds the parts of a client that have nothing to do with any
// particular endpoint: building a URL against a base, retrying what is
// worth retrying, decoding the envelope, and turning a non-2xx answer into
// one error carrying the status, the method, the URL, and whatever the
// server had to say about it. Which paths exist, what they return, and
// which deadline each one deserves belong to the caller.
//
//	c, err := httpclient.New(config.NewClientConfig(v), httpclient.Options{
//		TrimPathSuffix: "/api/v1",
//		Log:            log,
//	})
//	...
//	var env httpclient.Envelope[Item]
//	err = c.Do(ctx, http.MethodGet, c.URL("/items/7", nil), nil, "", &env)
//
// The transport is Fiber's client, fasthttp underneath. It buffers a whole
// response in memory rather than streaming it, so an endpoint that answers
// with something too large to hold wants its own client and not this one.
//
// # Deadlines
//
// This client sets none, and the transport it builds has no timeout of its
// own. A metadata call and a multi-megabyte upload want different limits,
// and the only place that knows which is which is the call site, so every
// request takes its deadline from the context it is given.
//
// A Do with a context that carries no deadline can therefore wait
// indefinitely, and with retries configured it can do so several times
// over. That is the caller's decision to make, not this package's to
// override, but it is worth making deliberately.
//
// # Where a client's settings come from
//
// New takes a config.ClientConfig, so the deployment's settings reach the
// client without an intermediate struct to copy them onto and keep in step.
//
// Three settings a client needs cannot be written in YAML, and those come
// separately, in Options: a route prefix to trim, a header map, and a
// logger. NewClientConfig documents why each is absent from the section.
//
// # What is retried
//
// Transport failures and the five statuses that mean "try again later":
// 429, 500, 502, 503 and 504. Everything else is a deterministic answer
// about this request, and repeating it only delays the same result.
//
// The backoff doubles from 500ms and stops at 8s, with no jitter, so
// callers that fail together retry together. No Retry-After header is
// read. A method listed in the config's NoRetryMethods gets exactly one
// attempt whatever the failure, and POST is that list's default, because a
// POST that timed out may have been applied.
//
// Do's contract on the way out is that a status this client will not retry
// gives an *Error directly, and an exhausted budget gives an error
// wrapping the last failure — an *Error if the last attempt reached the
// server, a transport error if it did not. So errors.As reaches the status
// either way, and errors.Is reaches the three sentinels.
//
// # One client, shared
//
// A Client is safe for concurrent use and is meant to be built once and
// shared. Nothing on it is written after New returns, and each request is
// taken fresh off the transport. Building one per call would also throw
// away the connection pool behind it, which is most of what the transport
// is for.
//
// # What the tests hold in place
//
// The unit suite already runs against a real HTTP server rather than a
// stub transport, so it reaches the wire: base-URL normalisation, which
// statuses retry and which do not, the no-retry method list, the
// sentinels, headers and token, decoding into the caller's type, query
// merging, and a context deadline.
//
// httpclient_integration_test.go covers four things that suite does not,
// each needing something a plain HTTP handler cannot provide:
//
//   - TLS. Insecure is untestable without a server presenting a
//     certificate this client would otherwise refuse, so the test starts
//     one and asserts both sides — that the default rejects it and that
//     the opt-in accepts it.
//   - Backoff TIMING rather than attempt counts. That the first wait is
//     roughly firstBackoff, that it doubles, and that it stops at
//     maxBackoff, which is the difference between a retry policy and a
//     hot loop.
//   - The caps on an error message, against bodies built to defeat them:
//     one far larger than maxErrorBody, one that is multi-byte UTF-8
//     across the rune boundary the cut lands on, and one that is not text
//     at all.
//   - Concurrency, under the race detector, against the claim above that
//     one client is meant to be shared.
//
// One thing deliberately NOT claimed by a test: the bytes.Clone in send.
// Its comment explains that the transport pools response buffers and that
// anything still pointing at one after Close reads whatever lands there
// next. That reasoning is sound and the clone should stay. But with the
// clone removed, 8,000 requests across 16 concurrent workers produced no
// corruption — first on fiber v3.0.0-beta.4, and again on v3.5.0 — so a
// test asserting otherwise would be asserting something that does not
// happen on either. The clone is a defence against a documented transport
// behaviour, not against an observed failure. Re-run that probe when the
// transport changes: a version that does corrupt makes the test possible,
// the way it was for the request package.
//
//	go test -tags integration -run Integration ./httpclient
//
// It binds loopback listeners and needs no configuration.
package httpclient
