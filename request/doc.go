// Package request parses route parameters and query strings into the
// shapes a service layer takes: ids, paging, sorting, column selection and
// optional filters.
//
// Every helper takes a fiber.Ctx and returns a plain Go value. Nothing is
// cached, nothing is stored, and no helper depends on another, so a handler
// calls exactly the ones it needs:
//
//	id, err := request.ID(c, "id")
//	if err != nil {
//		return err
//	}
//	page, limit := request.Page(c)
//	sorts := request.Sort(c)
//
// Three rules hold across the package, and each is a decision rather than
// an implementation detail.
//
// # Bad input is not an error
//
// Every helper but ID and QueryID corrects or ignores what it cannot use —
// an unknown sort column, a non-numeric entry in an id list, a page size
// above the ceiling — and the request goes on.
//
// Only a parameter that names the row to act on is worth a 400, because
// nothing can stand in for it. A filter that cannot be parsed has an
// obvious substitute, which is not to filter; a page size that cannot be
// parsed has a default. An id does not: guessing one would act on the
// wrong row.
//
// The cost is that a typo never shows. ?cols=nonsense drops every name and
// the client receives more columns than it asked for rather than an error.
// That is the trade the rule makes, in exchange for a partly-wrong request
// still answering.
//
// # Nothing here writes a response
//
// ID and QueryID return a *fiber.Error, the caller returns it unchanged,
// and the application's error handler renders one body from it.
//
// Writing the response here instead does not work, and the way it fails is
// quiet. Fiber's JSON returns nil whenever encoding succeeds, so
// "return c.Status(400).JSON(...)" hands the caller a nil error, its
// "if err != nil" guard is skipped, and the handler runs on with id 0 —
// which a service resolves to "not found", overwriting the 400 with a 404.
// The client gets a plausible wrong answer rather than the right one.
//
// # What leaves this package is cloned
//
// A query value points into the request buffer, which Fiber reuses once
// the handler returns unless the application is configured as immutable.
// Anything that outlives the request — a value captured by a goroutine,
// cached, or queued for a retry — would otherwise read whatever the next
// request put there.
//
// So Cols, Sort and StringPtr clone. IDs and IntPtr do not need to: they
// return integers, which are copies already. BoolPtr returns a pointer to
// a fresh local.
//
// This is the rule most likely to be broken by a helper added later, and
// the one whose breakage is least visible in a test: a single-request test
// never reuses the buffer, so a missing clone passes everything until two
// requests arrive in sequence under load. request_integration_test.go
// exists for exactly that case.
//
// # Paging defaults are package state
//
// DefaultPage, DefaultLimit and MaxLimit are variables because a page
// ceiling is a property of the service, not of this module. Assign them at
// startup, before the first request; assigning them while requests are in
// flight is a data race.
//
// MaxLimit below DefaultLimit makes every out-of-range limit fall back to
// a value the ceiling forbids. Nothing checks for that, because Page reads
// all three on every call and there is no moment this package could
// usefully validate them in.
//
// # The one dependency worth noticing
//
// Sort returns []database.SortField, so this package imports the SQL half
// of this module. That is the only coupling between the two, and it is
// here rather than in database because a sort order arrives as text in a
// query string and has to become something a repository can hold.
//
// A service that parses sorts for something other than SQL should call the
// rest of this package and write its own Sort. Nothing else here knows
// what a database is.
//
// # What the tests hold in place
//
// The unit suite drives every helper through a real framework context
// built by a real router, rather than a hand-made one, so the values under
// test are the values a handler would actually receive: the defaults and
// the ceiling, the junk-skipping in IDs, the three-way distinction in
// BoolPtr, and the 400 shape from ID and QueryID.
//
// request_integration_test.go covers the buffer-reuse rule, which a
// single-request test cannot reach by construction. It fires requests in
// sequence through one application, keeps the values the first one
// produced, and checks them after later requests have overwritten the
// buffer underneath — which is the only way a missing clone shows up as
// anything other than a passing test.
//
//	go test -tags integration -run Integration ./request
//
// It needs no server and no configuration.
package request
