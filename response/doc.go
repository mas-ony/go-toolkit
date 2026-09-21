// Package response defines the standard JSON envelope for API endpoints.
//
// A handler returns either a Response for a single result, or a Response
// whose Data field holds a Paginated value for a list. Nothing here writes
// to a connection, imports a web framework, or knows what an HTTP status
// code is: the envelope is a pair of structs and three constructors, and
// the caller decides how it reaches the wire.
//
//	return c.Status(200).JSON(response.OK("", user))
//	return c.Status(404).JSON(response.Fail("user not found"))
//
// # The envelope is meant to be universal
//
// Handlers build it explicitly, and a framework-level error handler is
// expected to build it for everything that escapes a handler — unmatched
// routes, wrong methods, oversized bodies and panics alike.
//
// That last part is what makes the shape worth having. A client that can
// rely on {success, message, data} for EVERY response never has to
// special-case the failures its own code did not produce, and those are
// exactly the ones it is least equipped to parse. A 404 from the router
// and a 404 from a handler should be the same object.
//
// Nothing in this package enforces that, because nothing here sees the
// framework. What holds it up is one ErrorHandler at the top of the
// application — and two qualifications that are easy to assume away, both
// established against a running framework in the integration suite rather
// than argued here:
//
//   - A PANIC only reaches the ErrorHandler if the application mounts
//     recovery middleware. It is not on by default. Without it a panic
//     unwinds past the handler and takes the process with it, so "panics
//     use the envelope" is a claim about the middleware stack, not about
//     this package.
//   - An OVER-LIMIT BODY does reach the ErrorHandler, so the envelope is
//     built — but the request was refused partway through being sent, and
//     the response may not survive back to the client. A caller planning
//     an upload endpoint should read "the envelope covers it" as a
//     statement about what the server logs, not as a promise about what
//     the client parses.
//
// # Status and Success say the same thing twice, deliberately
//
// A successful response is a 2xx with Success true; a failure is a non-2xx
// with Success false. Never a 200 with Success false: HTTP clients detect
// failure from the status code and treat the body as optional, so an error
// reported only in the body is an error most clients will miss.
//
// The duplication earns its place in a client that has already parsed the
// body — a single boolean is easier to branch on than a status range — and
// costs nothing as long as the two never disagree.
//
// # Message is a user-facing string
//
// Optional on success, required on failure, and never internal error text,
// a stack trace, or a driver message. Those go to the log.
//
// The pattern that keeps a framework ErrorHandler safe is to pass the
// framework error's own message through for recognised failures and a fixed
// string such as "internal server error" for everything else, so nothing a
// handler leaked reaches a client.
//
// # The interface-nil trap
//
// Data is typed any, and that makes omitempty weaker than it looks. A
// TYPED nil — a nil slice, a nil pointer — produces a non-nil interface
// carrying a type, so the field is present as null rather than omitted:
//
//	var items []Item
//	response.OK("ok", items)     // -> "data":null      NOT omitted
//	var m *Item
//	response.OK("ok", m)         // -> "data":null      NOT omitted
//	response.OK("deleted", nil)  // -> field absent     omitted
//
// Only an untyped nil literal reaches the omitempty path. This is the
// classic Go interface-nil trap, and it lands exactly where this package's
// own advice says it should not: a repository returning a nil slice for an
// empty result sends "data":null to a client that was promised "data":[].
//
// There are two ways not to be caught by it. NewPaginated normalises Items,
// so every list endpoint that paginates is already safe. OKList does the
// same for a bare slice, and is the one to reach for when a list endpoint
// does not paginate.
//
// OK itself performs no normalisation and is not going to start: it is
// where a caller passes a struct, a pointer, a Paginated or nothing, and a
// constructor that reflected on its argument to decide whether null means
// "empty" or "absent" would be guessing at the one thing the caller knows.
//
// # What the tests hold in place
//
// The unit suite covers the envelope shape as JSON — which keys appear,
// which are omitted, and every arm of the trap above — plus NewPaginated's
// normalisation and its generic instantiation.
//
// response_integration_test.go covers the universality claim, which no
// amount of struct testing can reach: it mounts these constructors as a
// real framework's ErrorHandler and fires the failures a handler never
// sees — an unmatched route, a method the route does not allow, a body
// over the limit, and a panic — asserting that each one comes back as this
// envelope rather than as the framework's own error page.
//
// That is the only place this module's web framework appears in this
// package, and it appears in a test file behind a build tag, so the
// package itself still imports nothing outside the standard library.
//
//	go test -tags integration -run Integration ./response
package response
