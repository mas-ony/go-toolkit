package request

// Tests for request.go, each driven through a real Fiber router so the
// values under test are the ones a handler receives: ID, QueryID and Date
// and the one 400 they return, the junk-skipping in IDs, the paging
// defaults and ceiling, Sort and Cols, the answers of each pointer helper,
// and the cloning of every string that leaves the package. Buffer reuse
// across requests needs a real listener and is in
// request_integration_test.go.

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"unsafe"

	"github.com/gofiber/fiber/v3"
	"github.com/mas-ony/go-toolkit/database"
)

// probe sends one GET through an app whose three routes, /items,
// /items/:id and /days/:day, all call fn with the request context, and
// returns the response.
func probe(t *testing.T, target string, fn func(c fiber.Ctx)) *http.Response {
	t.Helper()
	app := fiber.New()
	route := func(c fiber.Ctx) error {
		fn(c)
		return c.SendString("ok")
	}
	app.Get("/items", route)
	app.Get("/items/:id", route)
	app.Get("/days/:day", route)

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, target, nil))
	if err != nil {
		t.Fatalf("%s: %v", target, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// badRequest reports the code and message of a *fiber.Error, or (0, "") for
// anything else.
func badRequest(err error) (int, string) {
	if fe, ok := errors.AsType[*fiber.Error](err); ok {
		return fe.Code, fe.Message
	}
	return 0, ""
}

// ID reads a positive route parameter, and answers a missing, non-numeric
// or non-positive one with the same 400 naming the parameter.
func TestID(t *testing.T) {
	for _, target := range []string{"/items/7"} {
		probe(t, target, func(c fiber.Ctx) {
			id, err := ID(c, "id")
			if id != 7 || err != nil {
				t.Errorf("%s: id, err = %d, %v", target, id, err)
			}
		})
	}

	// A missing segment, a non-numeric one and a non-positive one are the
	// same mistake and get the same answer.
	for _, target := range []string{"/items", "/items/abc", "/items/0",
		"/items/-3"} {
		probe(t, target, func(c fiber.Ctx) {
			id, err := ID(c, "id")
			code, msg := badRequest(err)
			if id != 0 || code != fiber.StatusBadRequest ||
				msg != "invalid id" {
				t.Errorf("%s: id, err = %d, %v", target, id, err)
			}
		})
	}
}

// QueryID holds a query parameter to the same rule and the same 400 as ID.
func TestQueryID(t *testing.T) {
	probe(t, "/items?unit_id=4", func(c fiber.Ctx) {
		id, err := QueryID(c, "unit_id")
		if id != 4 || err != nil {
			t.Errorf("id, err = %d, %v", id, err)
		}
	})
	for _, target := range []string{"/items", "/items?unit_id=",
		"/items?unit_id=abc", "/items?unit_id=0"} {
		probe(t, target, func(c fiber.Ctx) {
			id, err := QueryID(c, "unit_id")
			code, msg := badRequest(err)
			if id != 0 || code != fiber.StatusBadRequest ||
				msg != "invalid unit_id" {
				t.Errorf("%s: id, err = %d, %v", target, id, err)
			}
		})
	}
}

// Date reads a route parameter in time.DateOnly's layout, and answers a
// missing one, or one in any other shape, with the same 400 naming the
// parameter.
func TestDate(t *testing.T) {
	for _, day := range []string{"2026-01-02", "2024-02-29"} {
		probe(t, "/days/"+day, func(c fiber.Ctx) {
			got, err := Date(c, "day")
			if got != day || err != nil {
				t.Errorf("%s: day, err = %q, %v", day, got, err)
			}
		})
	}

	// A missing segment, an unpadded date, a day the month lacks, a time
	// of day, a signed year and another order are one mistake each, and
	// all get the same answer.
	for _, target := range []string{"/items", "/days/2026-1-2",
		"/days/2026-13-01", "/days/2026-02-30", "/days/2025-02-29",
		"/days/2026-01-02T00:00:00", "/days/-026-01-02",
		"/days/02-01-2026", "/days/garbage"} {
		probe(t, target, func(c fiber.Ctx) {
			got, err := Date(c, "day")
			code, msg := badRequest(err)
			if got != "" || code != fiber.StatusBadRequest ||
				msg != "invalid day" {
				t.Errorf("%s: day, err = %q, %v", target, got, err)
			}
		})
	}
}

// The whole point of returning the error: the caller's guard fires, the
// handler stops, and the error handler renders one 400.
func TestIDRendersOneBadRequest(t *testing.T) {
	reached := false
	app := fiber.New(fiber.Config{
		ErrorHandler: func(c fiber.Ctx, err error) error {
			if code, msg := badRequest(err); code != 0 {
				return c.Status(code).JSON(map[string]any{"message": msg})
			}
			return c.Status(http.StatusInternalServerError).
				SendString(err.Error())
		},
	})
	route := func(c fiber.Ctx) error {
		id, err := ID(c, "id")
		if err != nil {
			return err
		}
		reached = true
		return c.JSON(map[string]any{"id": id})
	}
	app.Get("/items", route)
	app.Get("/items/:id", route)

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/items/abc",
		nil))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	if !strings.Contains(string(body), "invalid id") {
		t.Errorf("body = %s, want it to name the parameter", body)
	}
	if reached {
		t.Error("the handler ran on after the guard")
	}
}

// Why nothing here writes the response itself: Fiber's JSON returns nil
// whenever encoding succeeds, so a handler that answers that way hands its
// caller no error and the caller's guard never fires.
func TestWritingTheResponseYieldsNoError(t *testing.T) {
	var written error
	probe(t, "/items/abc", func(c fiber.Ctx) {
		written = c.Status(fiber.StatusBadRequest).
			JSON(map[string]any{"message": "invalid id"})
	})
	if written != nil {
		t.Errorf("JSON returned %v, so the reason in the doc has changed",
			written)
	}
}

// IDs keeps each positive integer once, in order, skips everything else,
// and returns nil when nothing usable is left.
func TestIDs(t *testing.T) {
	cases := map[string][]int{
		"/items":                         nil,
		"/items?ids=":                    nil,
		"/items?ids=abc":                 nil,
		"/items?ids=0,-2":                nil,
		"/items?ids=1,2,3":               {1, 2, 3},
		"/items?ids=%204%20,x,0,%204%20": {4},
		"/items?ids=3,3,5,3":             {3, 5},
		"/items?ids=9,,8":                {9, 8},
	}
	for target, want := range cases {
		probe(t, target, func(c fiber.Ctx) {
			if got := IDs(c, "ids"); !reflect.DeepEqual(got, want) {
				t.Errorf("%s: IDs = %v, want %v", target, got, want)
			}
		})
	}
}

// Page corrects a page below 1 to the first, and a limit outside 1 to
// MaxLimit to the default rather than to the ceiling.
func TestPage(t *testing.T) {
	cases := map[string][2]int{
		"/items":                  {1, 10},
		"/items?page=3&limit=25":  {3, 25},
		"/items?page=0":           {1, 10},
		"/items?page=-5":          {1, 10},
		"/items?page=abc":         {1, 10},
		"/items?limit=0":          {1, 10},
		"/items?limit=101":        {1, 10},
		"/items?limit=500":        {1, 10},
		"/items?limit=abc":        {1, 10},
		"/items?page=2&limit=100": {2, 100},
	}
	for target, want := range cases {
		probe(t, target, func(c fiber.Ctx) {
			page, limit := Page(c)
			if page != want[0] || limit != want[1] {
				t.Errorf("%s: page, limit = %d, %d; want %d, %d",
					target, page, limit, want[0], want[1])
			}
		})
	}
}

// Sort keeps each column as sent, reads desc in any case as descending and
// anything else as ascending, and returns nil when nothing is left.
func TestSort(t *testing.T) {
	field := func(col, dir string) database.SortField {
		return database.SortField{Col: col, Dir: dir}
	}
	cases := map[string][]database.SortField{
		"/items":                      nil,
		"/items?sort=":                nil,
		"/items?sort=,":               nil,
		"/items?sort=:desc":           nil,
		"/items?sort=t.name":          {field("t.name", "ASC")},
		"/items?sort=t.name:":         {field("t.name", "ASC")},
		"/items?sort=t.name:sideways": {field("t.name", "ASC")},
		"/items?sort=t.name:DeSc":     {field("t.name", "DESC")},
		"/items?sort=t.name:desc,t.id:asc": {
			field("t.name", "DESC"), field("t.id", "ASC"),
		},
		"/items?sort=%20t.name%20:%20desc%20": {field("t.name", "DESC")},
	}
	for target, want := range cases {
		probe(t, target, func(c fiber.Ctx) {
			if got := Sort(c); !reflect.DeepEqual(got, want) {
				t.Errorf("%s: Sort = %v, want %v", target, got, want)
			}
		})
	}
}

// Cols trims and keeps each non-empty name, and returns nil for every
// column.
func TestCols(t *testing.T) {
	cases := map[string][]string{
		"/items":                  nil,
		"/items?cols=":            nil,
		"/items?cols=,,":          nil,
		"/items?cols=code":        {"code"},
		"/items?cols=code,title":  {"code", "title"},
		"/items?cols=%20code%20,": {"code"},
	}
	for target, want := range cases {
		probe(t, target, func(c fiber.Ctx) {
			if got := Cols(c); !reflect.DeepEqual(got, want) {
				t.Errorf("%s: Cols = %v, want %v", target, got, want)
			}
		})
	}
}

// StringPtr trims the value, and treats an empty or absent one as nil.
func TestStringPtr(t *testing.T) {
	probe(t, "/items?code=%20alpha%20&empty=%20", func(c fiber.Ctx) {
		got := StringPtr(c, "code")
		if got == nil || *got != "alpha" {
			t.Errorf("code = %v, want alpha", got)
		}
		if got := StringPtr(c, "empty"); got != nil {
			t.Errorf("empty = %q, want nil", *got)
		}
		if got := StringPtr(c, "absent"); got != nil {
			t.Errorf("absent = %q, want nil", *got)
		}
	})
}

// IntPtr returns a positive integer, and nil for zero, a negative, text, a
// blank or an absent parameter.
func TestIntPtr(t *testing.T) {
	probe(t, "/items?n=5&zero=0&neg=-1&text=abc&blank=", func(c fiber.Ctx) {
		if got := IntPtr(c, "n"); got == nil || *got != 5 {
			t.Errorf("n = %v, want 5", got)
		}
		for _, key := range []string{"zero", "neg", "text", "blank",
			"absent"} {
			if got := IntPtr(c, key); got != nil {
				t.Errorf("%s = %d, want nil", key, *got)
			}
		}
	})
}

// DatePtr trims the value and keeps it when it is in time.DateOnly's
// layout; every other spelling, a blank and an absent parameter are nil.
func TestDatePtr(t *testing.T) {
	probe(t, "/items?a=2026-01-02&b=%202024-02-29%20&c=2026-1-2"+
		"&d=2026-02-30&e=2026-01-02T00:00:00&f=garbage&g=",
		func(c fiber.Ctx) {
			want := map[string]string{"a": "2026-01-02", "b": "2024-02-29"}
			for key, day := range want {
				if got := DatePtr(c, key); got == nil || *got != day {
					t.Errorf("%s = %v, want %s", key, got, day)
				}
			}
			for _, key := range []string{"c", "d", "e", "f", "g",
				"absent"} {
				if got := DatePtr(c, key); got != nil {
					t.Errorf("%s = %q, want nil", key, *got)
				}
			}
		})
}

// BoolPtr reads true, 1 and yes, and false, 0 and no, in any case; every
// other spelling, and an absent parameter, is nil.
func TestBoolPtr(t *testing.T) {
	probe(t, "/items?a=true&b=1&c=YES&d=false&e=0&f=No&g=yeah&h=",
		func(c fiber.Ctx) {
			for _, key := range []string{"a", "b", "c"} {
				if got := BoolPtr(c, key); got == nil || !*got {
					t.Errorf("%s = %v, want true", key, got)
				}
			}
			for _, key := range []string{"d", "e", "f"} {
				if got := BoolPtr(c, key); got == nil || *got {
					t.Errorf("%s = %v, want false", key, got)
				}
			}
			for _, key := range []string{"g", "h", "absent"} {
				if got := BoolPtr(c, key); got != nil {
					t.Errorf("%s = %v, want nil", key, *got)
				}
			}
		})
}

// A query value points into the request buffer, which Fiber reuses after the
// handler returns. Whatever the helpers keep has to be a copy, which is
// checked here by comparing the string data pointers.
func TestQueryValuesAreCloned(t *testing.T) {
	target := "/items?code=alpha&cols=alpha&sort=alpha&from=2026-01-02"
	probe(t, target, func(c fiber.Ctx) {
		raw := c.Query("code")
		if unsafe.StringData(raw) != unsafe.StringData(c.Query("code")) {
			t.Skip("this app copies query values already")
		}

		if got := StringPtr(c, "code"); got == nil ||
			unsafe.StringData(*got) == unsafe.StringData(raw) {
			t.Error("StringPtr kept the request buffer")
		}
		if got := Cols(c); len(got) != 1 ||
			unsafe.StringData(got[0]) ==
				unsafe.StringData(c.Query("cols")) {
			t.Error("Cols kept the request buffer")
		}
		if got := Sort(c); len(got) != 1 ||
			unsafe.StringData(got[0].Col) ==
				unsafe.StringData(c.Query("sort")) {
			t.Error("Sort kept the request buffer")
		}
		if got := DatePtr(c, "from"); got == nil ||
			unsafe.StringData(*got) ==
				unsafe.StringData(c.Query("from")) {
			t.Error("DatePtr kept the request buffer")
		}
	})
}

// A route parameter points into the same buffer as a query value, so Date
// copies what it returns as well.
func TestRouteValuesAreCloned(t *testing.T) {
	probe(t, "/days/2026-01-02", func(c fiber.Ctx) {
		got, err := Date(c, "day")
		if err != nil {
			t.Errorf("Date: %v", err)
			return
		}
		if unsafe.StringData(got) == unsafe.StringData(c.Params("day")) {
			t.Error("Date kept the request buffer")
		}
	})
}
