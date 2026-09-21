package response

import (
	"encoding/json"
	"reflect"
	"testing"
)

// The tests in this file assert on MARSHALLED JSON rather than on struct
// fields, because the struct is not the contract — the wire format is. A
// browser client sees only the bytes, and every claim the package doc makes
// (omitempty behaviour, "data":[] versus "data":null, key names) is a claim
// about those bytes. Asserting `r.Message == "ok"` would pass just as
// happily with a mistyped json tag.

// marshal is the shared assertion helper: it encodes v and returns the
// exact JSON string, so a test can compare against a literal.
//
// encoding/json emits struct fields in declaration order, which makes exact
// string comparison stable and far more readable than unmarshalling into a
// map and probing it key by key. The cost is that reordering fields in the
// struct breaks these tests — which is correct, since reordering changes
// the bytes a client parses even though it changes nothing about the Go
// type.
func marshal(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return string(b)
}

// TestOK covers the success constructor's three fields in one shot.
func TestOK(t *testing.T) {
	t.Parallel()

	r := OK("created", map[string]int{"id": 7})
	if !r.Success {
		t.Error("Success: got false, want true")
	}
	if r.Message != "created" {
		t.Errorf("Message: got %q, want %q", r.Message, "created")
	}
	if r.Data == nil {
		t.Error("Data: got nil, want the value passed in")
	}

	const want = `{"success":true,"message":"created","data":{"id":7}}`
	if got := marshal(t, r); got != want {
		t.Errorf("JSON:\n got %s\nwant %s", got, want)
	}
}

// TestFailNeverCarriesData pins two things the doc comment states
// separately: Success is false (left as the zero value in the constructor,
// which is easy to "fix" into something else by accident), and Data is
// absent from the output.
//
// The Data assertion is the one that matters operationally. Callers are
// told not to set Data on an error response; this proves the constructor
// does not set it for them either, so an error envelope can never leak a
// payload.
func TestFailNeverCarriesData(t *testing.T) {
	t.Parallel()

	r := Fail("invalid request")
	if r.Success {
		t.Error("Success: got true, want false")
	}
	if r.Data != nil {
		t.Errorf(
			"Data: got %#v, want nil — failures must not carry a payload",
			r.Data)
	}

	const want = `{"success":false,"message":"invalid request"}`
	if got := marshal(t, r); got != want {
		t.Errorf("JSON:\n got %s\nwant %s", got, want)
	}
}

// TestSuccessKeyIsAlwaysPresent guards the one field with no omitempty.
//
// Adding omitempty to Success would delete the key from every failure
// response, since false is the zero value — turning the documented "non-2xx
// + success: false" contract into "non-2xx and a missing field" for exactly
// the responses a client most needs to branch on.
func TestSuccessKeyIsAlwaysPresent(t *testing.T) {
	t.Parallel()

	var m map[string]any
	err := json.Unmarshal([]byte(marshal(t, Fail("boom"))), &m)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := m["success"]; !ok {
		t.Error(`"success" key missing from a failure response — ` +
			`omitempty must never be added to that field`)
	}
}

// TestOmitemptyBehaviour walks the exact table in the type's doc comment.
//
// Each case is a documented promise about which keys appear, so a change to
// any json tag surfaces here as a diff between two readable JSON strings
// rather than as a client bug three layers away.
func TestOmitemptyBehaviour(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		resp Response
		want string
	}{
		{
			name: "message and data both present",
			resp: OK("fetched", []int{1, 2}),
			want: `{"success":true,"message":"fetched","data":[1,2]}`,
		},
		{
			name: "empty message is omitted",
			resp: OK("", []int{1}),
			want: `{"success":true,"data":[1]}`,
		},
		{
			name: "untyped nil data is omitted",
			resp: OK("deleted", nil),
			want: `{"success":true,"message":"deleted"}`,
		},
		{
			name: "both empty leaves only success",
			resp: OK("", nil),
			want: `{"success":true}`,
		},
		{
			name: "empty slice is present, not omitted",
			resp: OK("ok", []int{}),
			want: `{"success":true,"message":"ok","data":[]}`,
		},
		{
			name: "zero-value struct is present, not omitted",
			resp: OK("ok", struct {
				N int `json:"n"`
			}{}),
			want: `{"success":true,"message":"ok","data":{"n":0}}`,
		},
		{
			name: "false is not omitted from success",
			resp: Fail("nope"),
			want: `{"success":false,"message":"nope"}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := marshal(t, tc.resp); got != tc.want {
				t.Errorf("JSON:\n got %s\nwant %s", got, tc.want)
			}
		})
	}
}

// TestInterfaceNilTrap pins a SHARP EDGE rather than a desirable behaviour.
//
// The Response doc describes it at length: Data is typed any, so a typed nil
// (a nil slice, a nil pointer) produces a non-nil interface and omitempty does
// not fire. The result is "data":null reaching a client that the same doc
// promises will get "data":[].
//
// This test exists so that edge is a decision instead of folklore. If someone
// later adds the reflection-based normalisation the doc suggests belongs in
// OK, these three cases fail — which is exactly right: that would be a
// deliberate change to the wire format for every list endpoint that passes a
// bare slice, and it should not slip in unnoticed.
func TestInterfaceNilTrap(t *testing.T) {
	t.Parallel()

	type model struct {
		ID int `json:"id"`
	}

	var nilSlice []model
	var nilPointer *model
	var nilMap map[string]int

	const null = `{"success":true,"message":"ok","data":null}`

	tests := []struct {
		name string
		data any
		want string
	}{
		{"nil slice yields null", nilSlice, null},
		{"nil pointer yields null", nilPointer, null},
		{"nil map yields null", nilMap, null},
		{"untyped nil is omitted", nil, `{"success":true,"message":"ok"}`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := marshal(t, OK("ok", tc.data)); got != tc.want {
				t.Errorf(
					"JSON:\n got %s\nwant %s\n"+
						"(see the interface-nil warning on the Response type)",
					got,
					tc.want)
			}
		})
	}
}

// TestNewPaginatedNormalisesNilItems is the counterpart to the test above: the
// one place in the package where the trap IS closed.
//
// A repository that returns a nil slice for an empty page is the normal case,
// not an edge case — every list endpoint hits it the moment a filter matches
// nothing. This normalisation is why paginated responses can promise a client
// it may iterate items without a null check.
func TestNewPaginatedNormalisesNilItems(t *testing.T) {
	t.Parallel()

	var fromEmptyQuery []string

	p := NewPaginated(fromEmptyQuery, 0, 1, 25)
	if p.Items == nil {
		t.Fatal("Items: got nil, want an empty slice")
	}
	if len(p.Items) != 0 {
		t.Errorf("Items: got %d entries, want 0", len(p.Items))
	}

	const want = `{"items":[],"total":0,"page":1,"limit":25}`
	if got := marshal(t, p); got != want {
		t.Errorf("JSON:\n got %s\nwant %s", got, want)
	}
}

// TestNewPaginatedPreservesMetadata checks the plain assembly path, including
// that the constructor takes Total, Page and Limit on trust.
//
// The nonsense values here are intentional: the doc says nothing validates
// them, and callers clamp before querying. Pinning that keeps validation from
// being added silently in a place where the caller has no way to see it happen
// and no error to react to.
func TestNewPaginatedPreservesMetadata(t *testing.T) {
	t.Parallel()

	items := []int{5, 6, 7}
	p := NewPaginated(items, 99, 0, 0)

	if !reflect.DeepEqual(p.Items, items) {
		t.Errorf("Items: got %v, want %v", p.Items, items)
	}
	if p.Total != 99 {
		t.Errorf("Total: got %d, want 99", p.Total)
	}
	if p.Page != 0 {
		t.Errorf("Page: got %d, want 0 — must not clamp", p.Page)
	}
	if p.Limit != 0 {
		t.Errorf("Limit: got %d, want 0 — must not clamp", p.Limit)
	}
}

// TestNewPaginatedDoesNotCopyItems documents that the caller's slice is stored
// by reference, not cloned. Nothing depends on a copy today, and adding one
// would silently double the allocation of every list response.
func TestNewPaginatedDoesNotCopyItems(t *testing.T) {
	t.Parallel()

	items := []int{1, 2, 3}
	p := NewPaginated(items, 3, 1, 10)
	items[0] = 42

	if p.Items[0] != 42 {
		t.Errorf(
			"Items[0]: got %d, want 42 — the constructor is expected to "+
				"alias the caller's slice", p.Items[0])
	}
}

// TestPaginatedInsideResponse is the end-to-end shape a list endpoint
// produces, asserted as one string.
//
// This is the assembly the OK doc comment tells callers to use, and it is
// worth one exact-match test: it catches a Paginated accidentally passed as a
// bare slice, a missing envelope, and any key rename in either struct at once.
func TestPaginatedInsideResponse(t *testing.T) {
	t.Parallel()

	type sertifikat struct {
		ID   int    `json:"id"`
		Nama string `json:"nama"`
	}

	r := OK("ok", NewPaginated([]sertifikat{{ID: 1, Nama: "A"}}, 42, 3, 10))

	const want = `{"success":true,"message":"ok","data":` +
		`{"items":[{"id":1,"nama":"A"}],"total":42,"page":3,"limit":10}}`
	if got := marshal(t, r); got != want {
		t.Errorf("JSON:\n got %s\nwant %s", got, want)
	}
}

// TestPaginatedKeepsConcreteItemType is the reason the type is generic.
//
// The assertion is a type switch rather than a JSON comparison because the
// point is what survives on the GO side: Items must come back as []sertifikat,
// not []any. A non-generic version storing []any would marshal identically and
// pass every other test in this file.
func TestPaginatedKeepsConcreteItemType(t *testing.T) {
	t.Parallel()

	type sertifikat struct {
		ID int `json:"id"`
	}

	p := NewPaginated([]sertifikat{{ID: 1}}, 1, 1, 10)

	var items any = p.Items
	if _, ok := items.([]sertifikat); !ok {
		t.Errorf("Items: got %T, want []sertifikat", p.Items)
	}
}

// TestJSONTagsAreTheWireContract reads the struct tags directly.
//
// The tests above prove the CURRENT tags produce the documented bytes. This
// one states the tags themselves, so a change is legible as a change to the
// API contract — including the omitempty flags, which are the difference
// between an absent key and a null and which no amount of squinting at a
// marshalled sample makes obvious.
func TestJSONTagsAreTheWireContract(t *testing.T) {
	t.Parallel()

	response := reflect.TypeOf(Response{})
	paginated := reflect.TypeOf(Paginated[int]{})

	tests := []struct {
		typ    reflect.Type
		field  string
		want   string
		reason string
	}{
		{response, "Success", "success",
			"always present; omitempty would delete it from a failure"},
		{response, "Message", "message,omitempty",
			"optional on success"},
		{response, "Data", "data,omitempty",
			"absent, not null, when there is nothing to return"},
		{paginated, "Items", "items",
			"never omitted; NewPaginated guarantees a non-nil slice"},
		{paginated, "Total", "total",
			"client computes ceil(total/limit) from it"},
		{paginated, "Page", "page",
			"1-based"},
		{paginated, "Limit", "limit",
			"client mirrors it back on the next request"},
	}

	for _, tc := range tests {
		t.Run(tc.typ.Name()+"."+tc.field, func(t *testing.T) {
			t.Parallel()
			f, ok := tc.typ.FieldByName(tc.field)
			if !ok {
				t.Fatalf("field %s not found on %s", tc.field, tc.typ)
			}
			if got := f.Tag.Get("json"); got != tc.want {
				t.Errorf(
					"json tag: got %q, want %q (%s)",
					got,
					tc.want,
					tc.reason)
			}
		})
	}
}

// TestSwaggoTagsAreInertDocumentation records the doc comment's claim that the
// `example` tags have no runtime effect.
//
// They sit next to the json tags and look like they might do something. This
// asserts they do not: the marshalled output of a zero-value Response contains
// none of the example values, so nobody has to wonder whether "[]" from the
// Data tag can leak into a real response.
func TestSwaggoTagsAreInertDocumentation(t *testing.T) {
	t.Parallel()

	f, ok := reflect.TypeOf(Response{}).FieldByName("Data")
	if !ok {
		t.Fatal("Data field not found")
	}
	if f.Tag.Get("example") == "" {
		t.Skip("example tags removed; nothing left to assert")
	}

	const want = `{"success":false}`
	if got := marshal(t, Response{}); got != want {
		t.Errorf(
			"a zero Response marshalled to %s, want %s — example tags must "+
				"not affect output", got, want)
	}
}

// OKList is the constructor that cannot be caught by the interface-nil
// trap, and both halves of that are worth pinning: the nil case becomes an
// empty slice, and the populated case is passed through untouched.
func TestOKListNormalisesANilSlice(t *testing.T) {
	var none []string

	got := OKList("", none)
	if !got.Success {
		t.Error("Success = false, want true")
	}
	if got.Message != "" {
		t.Errorf("Message = %q, want empty", got.Message)
	}

	items, ok := got.Data.([]string)
	if !ok {
		t.Fatalf("Data is %T, want []string", got.Data)
	}
	if items == nil {
		t.Error("Data is a nil slice; OKList did not normalise")
	}
	if len(items) != 0 {
		t.Errorf("Data has %d items, want 0", len(items))
	}
}

func TestOKListPassesAPopulatedSliceThrough(t *testing.T) {
	got := OKList("ok", []int{1, 2, 3})

	items, ok := got.Data.([]int)
	if !ok {
		t.Fatalf("Data is %T, want []int", got.Data)
	}
	if len(items) != 3 || items[0] != 1 || items[2] != 3 {
		t.Errorf("Data = %v, want [1 2 3]", items)
	}
	if got.Message != "ok" {
		t.Errorf("Message = %q, want ok", got.Message)
	}
}

// The distinction OKList exists for, stated as a comparison so a reader
// sees both answers at once. If OK ever starts normalising, this fails and
// doc.go needs correcting along with it.
func TestOKAndOKListDisagreeOnANilSlice(t *testing.T) {
	var none []string

	if OK("", none).Data == nil {
		t.Error("OK normalised a nil slice to an untyped nil; doc.go " +
			"says it performs no normalisation")
	}
	if OKList("", none).Data == nil {
		t.Error("OKList left Data as an untyped nil")
	}
}
