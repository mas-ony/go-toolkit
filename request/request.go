package request

import (
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/mas-ony/go-toolkit/database"
	"github.com/mas-ony/go-toolkit/internal/strlist"
)

// Paging defaults, and the ceiling on how many rows one page may hold.
//
// Variables rather than constants, because a page ceiling is a property of
// the service and not of this module. A service that serves wider pages
// assigns these at startup, before the first request:
//
//	request.MaxLimit = 500
//
// Assigning them while requests are in flight is a data race, and assigning
// MaxLimit below DefaultLimit makes every out-of-range limit fall back to a
// value the ceiling forbids. Page reads all three on every call, so neither
// is checked here.
var (
	DefaultPage  = 1
	DefaultLimit = 10
	MaxLimit     = 100
)

// positiveID returns id, or the 400 ID and QueryID answer for a value that
// is missing, non-numeric or not positive, all of which Fiber has already
// read as 0 or less. param names the parameter in the error.
func positiveID(id int, param string) (int, error) {
	if id < 1 {
		return 0, fiber.NewError(fiber.StatusBadRequest, "invalid "+param)
	}
	return id, nil
}

// isDate reports whether s is a calendar date spelled exactly as
// time.DateOnly spells one: four digits of year, two of month and two of
// day, joined by hyphens, naming a day its month has. Date and DatePtr
// share it, so the two cannot drift apart on what a date is.
func isDate(s string) bool {
	_, err := time.Parse(time.DateOnly, s)
	return err == nil
}

// ID reads a route parameter, as in /items/:id, as a positive integer:
//
//	id, err := ID(c, "id")
//	if err != nil {
//		return err
//	}
//
// A missing, non-numeric or non-positive value yields 0 and a *fiber.Error
// carrying 400 and the parameter's name, which the caller returns unchanged.
//
// fiber.Params[int] yields 0 for a missing or non-numeric value, so /0, /abc
// and a missing segment are indistinguishable here. All three deserve the
// same 400, so that conflation costs nothing.
func ID(c fiber.Ctx, param string) (int, error) {
	return positiveID(fiber.Params[int](c, param), param)
}

// IDs reads a comma-separated list of positive integers from a query
// parameter: ?ids=1,2,3 gives []int{1, 2, 3}.
//
// The name says ids, but nothing here is key-specific: one handler may call
// it for several parameters that filter different columns. A parameter that
// holds a single value to match exactly belongs in IntPtr instead.
//
// Junk is skipped rather than rejected — a non-numeric entry, a zero, a
// negative — so a partly valid list still answers, and a repeat is dropped
// because it would only add a bound parameter. The result is nil rather than
// an empty slice when the parameter is absent or holds nothing usable, so a
// caller can test len(ids) == 0 and leave its IN clause out.
//
// The list parameters, this one, Cols and Sort, share one definition of
// what a list is, strlist.Split, which is also how config reads a list
// key. Its pieces still point into the request buffer, so a caller that
// keeps one as a string clones it.
//
// Each value becomes one bound parameter downstream, and SQL Server refuses
// a statement carrying more than 2100 of them. Fiber's default 4 KiB read
// buffer turns a query string that long away before it gets here, so the
// ceiling matters only to a deployment that raises fiber.read_buffer_size,
// and that deployment needs a limit of its own.
func IDs(c fiber.Ctx, param string) []int {
	var ids []int
	for _, token := range strlist.Split(c.Query(param)) {
		n, err := strconv.Atoi(token)
		if err != nil || n < 1 || slices.Contains(ids, n) {
			continue
		}
		ids = append(ids, n)
	}
	return ids
}

// QueryID reads a query parameter, as in ?unit_id=1, as a positive
// integer, and reports the same 400 as ID for a missing, non-numeric or
// non-positive value. Use ID for a route parameter.
//
// It is for a parameter the handler cannot work without. One that merely
// narrows a search belongs in IntPtr, which treats a missing value as
// "do not filter" rather than as a mistake.
func QueryID(c fiber.Ctx, param string) (int, error) {
	return positiveID(fiber.Query[int](c, param), param)
}

// Date reads a route parameter, as in /holidays/:tgl, as a plain calendar
// date, and returns it as text in time.DateOnly's layout:
//
//	tgl, err := Date(c, "tgl")
//	if err != nil {
//		return err
//	}
//
// A missing value, or one in any other shape — "2026-1-2", a time of day,
// the thirtieth of February — yields "" and a *fiber.Error carrying 400
// and the parameter's name, the answer ID gives. A route parameter names
// the row to act on, so a date that does not parse has no substitute.
//
// The text comes back rather than a time.Time because the text is what a
// repository binds; see the package documentation on dates. A query
// parameter that bounds a list belongs in DatePtr instead.
func Date(c fiber.Ctx, param string) (string, error) {
	raw := c.Params(param)
	if !isDate(raw) {
		return "", fiber.NewError(fiber.StatusBadRequest, "invalid "+param)
	}
	return strings.Clone(raw), nil
}

// Cols reads ?cols= into the column names a repository restricts its
// SELECT list to, and returns nil for "every column".
//
// Nothing is validated here: the repository keeps the names its allowlist
// knows and drops the rest. The silence is total — ?cols=nonsense drops every
// name, the repository falls back to its default list, and the client
// receives more than it asked for rather than an error. A partly wrong list
// does not fail a read, at the price of a typo that never shows.
func Cols(c fiber.Ctx) []string {
	tokens := strlist.Split(c.Query("cols"))
	cols := make([]string, 0, len(tokens))
	for _, token := range tokens {
		cols = append(cols, strings.Clone(token))
	}
	if len(cols) == 0 {
		return nil
	}
	return cols
}

// Page reads ?page= and ?limit= with defaults and a ceiling on page
// size, so a client cannot ask for a result set that has no bound.
//
// Out-of-range values are corrected rather than rejected, and the two are
// corrected differently on purpose:
//
//	page < 1                      page = 1   the nearest valid value
//	limit < 1 or above the max    limit = 10 the default, not the ceiling
//
// So ?limit=500 returns 10 rows, not 100. A client asking for 500 has misread
// the contract, and quietly serving 100 would look like it worked.
func Page(c fiber.Ctx) (page, limit int) {
	page = fiber.Query(c, "page", DefaultPage)
	limit = fiber.Query(c, "limit", DefaultLimit)
	if page < 1 {
		page = DefaultPage
	}
	if limit < 1 || limit > MaxLimit {
		limit = DefaultLimit
	}
	return page, limit
}

// Sort reads ?sort= into the fields a repository sorts by:
//
//	?sort=t.created_at:desc      one column, descending
//	?sort=t.name:asc,t.id:desc   two, in the order given
//	?sort=                       nil, leaving the repository's default
//
// The direction is optional, and anything other than "desc", in any letter
// case, sorts ascending: each field's Dir is what
// database.SortField.Direction reads it as, "ASC" or "DESC", the same rule
// database.SortClause applies to whatever it is handed. A column keeps the
// spelling the client sent, qualified with whatever table alias the
// caller's queries use, because database.SortClause matches it against
// the repository's ORDER BY allowlist and drops whatever is missing from
// it; no unvalidated name reaches the SQL text.
//
// A column is cut at its first colon, since that is what separates the
// direction. One that contains a colon of its own therefore arrives
// truncated, and the allowlist drops it like any other unknown name.
func Sort(c fiber.Ctx) []database.SortField {
	tokens := strlist.Split(c.Query("sort"))
	fields := make([]database.SortField, 0, len(tokens))
	for _, token := range tokens {
		col, dir, _ := strings.Cut(token, ":")
		col = strings.TrimSpace(col)
		if col == "" {
			continue
		}
		// Direction reads dir as SortClause will and returns a constant,
		// so the direction keeps nothing of the request buffer.
		field := database.SortField{Col: strings.Clone(col), Dir: dir}
		field.Dir = field.Direction()
		fields = append(fields, field)
	}
	if len(fields) == 0 {
		return nil
	}
	return fields
}

// StringPtr returns a query value, or nil when the parameter is absent
// or empty.
//
// The difference carries meaning downstream: a nil filter matches every row,
// while a pointer to "" filters for the empty string and matches none.
// Returning nil makes ?code= behave as though it had not been sent, which is
// what a cleared search box means.
func StringPtr(c fiber.Ctx, key string) *string {
	v := strings.TrimSpace(c.Query(key))
	if v == "" {
		return nil
	}
	v = strings.Clone(v)
	return &v
}

// IntPtr returns a positive integer query value, or nil when the
// parameter is absent, not a number, or below 1. fiber.Query yields the
// default for all three, so one lookup answers them together.
func IntPtr(c fiber.Ctx, key string) *int {
	v := fiber.Query(c, key, 0)
	if v < 1 {
		return nil
	}
	return &v
}

// BoolPtr returns a boolean query value, or nil when the parameter is
// absent or spelled in a way this does not recognise.
//
// An unrecognised spelling filters on nothing rather than defaulting to
// false, which would answer a different question than the one asked:
// ?has_file=yeah would return every row WITHOUT a file, the opposite of what
// was meant.
func BoolPtr(c fiber.Ctx, key string) *bool {
	switch strlist.Normalise(c.Query(key)) {
	case "true", "1", "yes":
		t := true
		return &t
	case "false", "0", "no":
		f := false
		return &f
	}
	return nil
}

// DatePtr returns a query value that is a plain calendar date, as Date
// reads one, or nil when the parameter is absent, empty or spelled any
// other way. It reads the value through StringPtr and keeps it only when
// it is a date, so the white space around it is trimmed and the date is
// copied out of the request exactly as StringPtr does both.
//
// It is for the bounds of a date range a list is filtered by, where nil
// means what it means for every pointer here: do not filter. A malformed
// bound therefore widens the result rather than failing the request, the
// trade IntPtr makes for a malformed number.
func DatePtr(c fiber.Ctx, key string) *string {
	if v := StringPtr(c, key); v != nil && isDate(*v) {
		return v
	}
	return nil
}
