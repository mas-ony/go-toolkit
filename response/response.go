package response

// Response is the top-level JSON envelope for every API response.
//
// Conventions:
//   - A successful response always uses a 2xx HTTP status + Success: true.
//   - A failed response always uses a non-2xx HTTP status + Success: false.
//     Never return HTTP 200 with Success: false — HTTP clients detect
//     errors via the status code; parsing the body is optional.
//   - Message is optional on success (e.g. "deleted") and required on
//     failure. It must be a safe, user-facing string. Never include
//     internal error text, stack traces, or database error messages — log
//     those server-side instead.
//   - Data should be nil when the operation has no return value (delete,
//     logout). Use an empty slice, not nil, when the result is an empty
//     collection so the client receives "data":[] rather than "data":null
//     and can iterate safely.
//
// omitempty behaviour:
//   - Message: omitted from JSON when the string is empty (""). OK() called
//     with an empty msg string produces no "message" key at all — this is
//     intentional for responses where the HTTP status code is
//     self-explanatory, a 201 say. A 204 carries no body at all, so it
//     never carries this envelope either.
//   - Data: omitted from JSON only when nil, not when it is a zero-value
//     struct or an empty slice. Always set Data to nil explicitly (not an
//     empty struct) for no-data responses so the field is absent rather
//     than present as an empty object.
//
// "nil" above means a nil INTERFACE, which is stricter than it sounds: a
// typed nil is not one. See the interface-nil trap in the package
// documentation for the three cases and the two constructors that avoid
// it — NewPaginated for a page, OKList for a bare slice.
//
// The `example` struct tags follow swaggo (Swagger/OpenAPI) annotation
// conventions. They have no effect at runtime: without a swag
// code-generation step they are documentation only, and with one they
// become example values in the generated API spec.
type Response struct {
	Success bool   `json:"success"           example:"true"`
	Message string `json:"message,omitempty" example:"something went wrong"`
	Data    any    `json:"data,omitempty"    example:"[]"`
}

// Paginated holds a single page of results alongside the metadata a client
// needs to implement pagination controls (page number buttons, "X of Y"
// label).
//
// Total is the count of all matching rows across all pages — not just the
// current page — so the client can calculate the total number of pages:
//
//	totalPages = ceil(Total / Limit)  // JS: Math.ceil(total / limit)
//
// Page and Limit mirror the values from the request so the client does not
// need to track them separately (useful when several paginated lists are on
// screen). Page is 1-based — the first page is page 1, not page 0.
//
// Generic over T so each endpoint's items keep their concrete type through
// the envelope — no []any, no per-endpoint copy of this struct. The cost is
// worth naming: swaggo's parser handles generic types poorly, so if the
// `example` tags below ever drive live spec generation, this is the type
// most likely to need a hand-written definition.
type Paginated[T any] struct {
	Items []T `json:"items" example:"[]"`
	Total int `json:"total" example:"1"` // matching rows, all pages
	Page  int `json:"page"  example:"1"` // current page (1-based)
	Limit int `json:"limit" example:"1"` // max items per page
}

// OK constructs a successful Response (Success: true).
// Pass nil for data when the operation produces no return value (e.g.
// logout, delete). Pass an empty slice, not nil, when returning an empty
// collection.
//
// data is typed as any, so callers may pass a plain struct, a slice, a
// Paginated[T] value, or nil — the JSON encoder handles all cases. When
// passing a Paginated[T], wrap it as
//
//	response.OK("ok", response.NewPaginated(...))
//
// so the Data field in the JSON output is the paginated object, not a bare
// slice.
//
// A note on what OK does NOT do: it performs no nil normalisation on data.
// Passing a nil slice here yields "data":null, not "data":[].
//
// That is not an oversight waiting on a reflection-based fix. data is any
// because this is where a caller passes a struct, a pointer, a Paginated
// or nothing at all, and a constructor that inspected the argument's kind
// would have to decide on the caller's behalf whether a nil pointer means
// "empty" or "absent" — a distinction only the caller holds. Reflecting on
// the slice kind alone would close one arm of the trap and leave the
// pointer arm open, which is worse than leaving both visible.
//
// Use OKList for a bare slice. It cannot be got wrong, because its
// parameter is typed as a slice rather than as any.
func OK(msg string, data any) Response {
	return Response{
		Success: true,
		Message: msg,
		Data:    data,
	}
}

// OKList constructs a successful Response whose Data is a slice, with a
// nil slice normalised to an empty one so the client receives "data":[]
// rather than "data":null.
//
// It exists because OK cannot do this safely and a caller should not have
// to remember to. The whole difference is in the signature: items is typed
// []T rather than any, so the compiler has already established that the
// argument is a slice, and normalising it involves no guess about what the
// caller meant.
//
// Use it for a list endpoint that does not paginate. One that does should
// pass NewPaginated to OK instead, which normalises Items for the same
// reason.
//
//	return c.JSON(response.OKList("", users))   // "data":[] when empty
func OKList[T any](msg string, items []T) Response {
	if items == nil {
		items = []T{}
	}
	return Response{
		Success: true,
		Message: msg,
		Data:    items,
	}
}

// Fail constructs a failure Response (Success: false).
// msg must be a safe, user-facing message. Internal error details must be
// logged server-side and must not be returned to the client.
//
// Data is always nil on a failure response — the field is omitted from the
// JSON output entirely (due to omitempty). Callers must not set Data on an
// error response; if additional context is needed, encode it in msg or add
// a new envelope field.
//
// Success is left as its zero value rather than written as
// `Success: false`. That is correct but invisible, and it is the one field
// a reader might expect to see set explicitly in a constructor named Fail —
// worth knowing before someone "fixes" the omission and assumes it changed
// behaviour.
//
// A framework-level error handler is typically the largest caller. The
// pattern that keeps this path safe is to pass the framework error's own
// message through for recognised failures and a fixed string such as
// "internal server error" for anything else, so no internal error text
// reaches a client.
func Fail(msg string) Response {
	return Response{
		Success: false,
		Message: msg,
	}
}

// NewPaginated constructs a Paginated value.
// A nil items slice is normalised to an empty slice so the client always
// receives "items":[] rather than "items":null when there are no results,
// which allows safe iteration without a null check.
//
// Total, Page, and Limit are taken on trust — nothing here validates that
// Page is at least 1, that Limit is non-zero, or that Total is consistent
// with len(items). Callers parse and clamp those values before querying (a
// zero Limit would make the client's ceil(Total/Limit) divide by zero), so
// this constructor stays a plain value assembler.
func NewPaginated[T any](items []T, total, page, limit int) Paginated[T] {
	// The one normalisation in this package, and the reason list endpoints
	// do not hit the interface-nil trap documented on Response: a
	// repository returning a nil slice for an empty page becomes "items":[]
	// here rather than "items":null, so the client can iterate without a
	// guard.
	if items == nil {
		items = []T{}
	}
	return Paginated[T]{Items: items, Total: total, Page: page, Limit: limit}
}
