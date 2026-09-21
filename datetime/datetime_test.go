package datetime

import (
	"database/sql"
	"database/sql/driver"
	"encoding"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// testZone is the zone every test in this file runs in: a fixed +07:00,
// which is enough to make the naive-versus-offset distinction visible
// without depending on any particular region's rules.
//
// A FIXED zone rather than time.LoadLocation: LoadLocation reads the system
// tzdata, which is absent from the minimal images CI runners and container
// builds often use, and a test that skips itself on the build machine is a
// test that never runs. A zone with no DST and a stable offset is
// indistinguishable from a real one for everything asserted below.
//
// (If a zone with real transition rules is ever needed, import
// _ "time/tzdata" to embed the database in the binary rather than reaching
// for the system copy.)
var testZone = time.FixedZone("TEST", 7*60*60)

// Compile-time assertions for the receiver split described at the bottom of
// Value's doc comment.
//
// The pointer entries are the interesting half. Value and MarshalJSON take
// VALUE receivers, so they are in *Datetime's method set as well — which is
// why a model holding a *Datetime can still be written to a column and
// marshalled. Switching either to a pointer receiver would break the value
// entries below and go unnoticed at every call site, because a model would
// simply stop being a Valuer and database/sql would fall back to reflecting
// over the struct.
var (
	_ json.Marshaler   = Datetime{}
	_ json.Marshaler   = (*Datetime)(nil)
	_ driver.Valuer    = Datetime{}
	_ driver.Valuer    = (*Datetime)(nil)
	_ json.Unmarshaler = (*Datetime)(nil)
	_ sql.Scanner      = (*Datetime)(nil)

	// The entry that is easy to lose. Delete UnmarshalText and this line still
	// compiles, because *Datetime PROMOTES time.Time's RFC 3339-only version
	// and satisfies the interface with it. TestUnmarshalTextAcceptsEveryLayout
	// is what actually holds the behaviour; this only records the intent.
	_ encoding.TextUnmarshaler = (*Datetime)(nil)
)

// local builds an expected time in the test zone, keeping the tables below
// readable.
func local(year int, month time.Month, day, hour, min, sec int) time.Time {
	return time.Date(year, month, day, hour, min, sec, 0, testZone)
}

// TestMain pins time.Local before any test runs.
//
// This mirrors what a process is expected to do once at startup, and it is
// not optional here: every naive layout in parseLayouts resolves in
// time.Local, so without this the results would depend on the TZ
// environment variable of whichever machine ran the suite — passing on a
// developer's laptop and failing in a UTC container, which is the exact
// class of bug resolving naive layouts in time.Local rules out.
//
// Setting it once, before tests start, also keeps the package safe to run
// with t.Parallel(): nothing mutates time.Local afterwards, so there is no
// shared state to race on.
func TestMain(m *testing.M) {
	time.Local = testZone
	os.Exit(m.Run())
}

// TestUnmarshalJSONAcceptsEveryLayout walks parseLayouts entry by entry.
//
// Each case is a format some real producer emits — a browser input element,
// a JSON client, a driver that stringified a column — and the expected
// value is written as an explicit instant rather than a re-parse of the
// same string, so a broken layout cannot agree with a broken expectation.
func TestUnmarshalJSONAcceptsEveryLayout(t *testing.T) {
	t.Parallel()

	utc := func(h, m, s, ns int) time.Time {
		return time.Date(2024, 3, 15, h, m, s, ns, time.UTC)
	}

	tests := []struct {
		name  string
		input string
		want  time.Time
	}{
		{"RFC3339 UTC", `"2024-03-15T10:00:00Z"`, utc(10, 0, 0, 0)},
		{"RFC3339 with offset", `"2024-03-15T10:00:00+07:00"`,
			local(2024, 3, 15, 10, 0, 0)},
		{"RFC3339 nano", `"2024-03-15T10:00:00.123456789Z"`,
			utc(10, 0, 0, 123456789)},
		{"datetime-local with seconds", `"2024-03-15T10:30:45"`,
			local(2024, 3, 15, 10, 30, 45)},
		{"datetime-local without seconds", `"2024-03-15T10:30"`,
			local(2024, 3, 15, 10, 30, 0)},
		{"SQL text timestamp", `"2024-03-15 10:30:45"`,
			local(2024, 3, 15, 10, 30, 45)},
		{"date only", `"2024-03-15"`, local(2024, 3, 15, 0, 0, 0)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var d Datetime
			if err := d.UnmarshalJSON([]byte(tc.input)); err != nil {
				t.Fatalf("UnmarshalJSON(%s): %v", tc.input, err)
			}
			if !d.Time.Equal(tc.want) {
				t.Errorf(
					"instant: got %s, want %s",
					d.Time.Format(time.RFC3339Nano),
					tc.want.Format(time.RFC3339Nano))
			}
		})
	}
}

// TestNaiveInputResolvesInLocalNotUTC pins the skew described in parse's
// doc comment.
//
// Resolved in UTC, a form submitting "2024-03-15" would mean midnight UTC —
// 07:00 local — and any value near midnight would land on the wrong day
// once the driver wrote it back. The assertion is deliberately written as
// the UTC instant rather than as a wall clock, because a wall-clock
// comparison passes under either behaviour.
func TestNaiveInputResolvesInLocalNotUTC(t *testing.T) {
	t.Parallel()

	var d Datetime
	if err := d.UnmarshalJSON([]byte(`"2024-03-15"`)); err != nil {
		t.Fatalf("UnmarshalJSON: %v", err)
	}

	// Midnight local.
	want := time.Date(2024, 3, 14, 17, 0, 0, 0, time.UTC)
	if !d.Time.Equal(want) {
		t.Errorf(
			"instant: got %s, want %s — a naive date must mean midnight "+
				"in the local zone, not in UTC",
			d.Time.UTC().Format(time.RFC3339),
			want.Format(time.RFC3339))
	}
}

// TestOffsetBearingInputKeepsItsOffset is the other half of the same rule.
//
// ParseInLocation applies the location only when the input carries no zone,
// so a client that pins a specific instant keeps it. Reinterpreting "…Z" as
// a local wall clock would shift the value by the zone's whole offset in
// the opposite direction, and is what a careless "just use Parse with
// time.Local" would do.
func TestOffsetBearingInputKeepsItsOffset(t *testing.T) {
	t.Parallel()

	var d Datetime
	if err := d.UnmarshalJSON([]byte(`"2024-03-15T10:00:00Z"`)); err != nil {
		t.Fatalf("UnmarshalJSON: %v", err)
	}

	want := time.Date(2024, 3, 15, 10, 0, 0, 0, time.UTC)
	if !d.Time.Equal(want) {
		t.Errorf(
			"instant: got %s, want %s",
			d.Time.UTC().Format(time.RFC3339), want.Format(time.RFC3339))
	}
	// The wall clock in the local zone is the visible consequence: 10:00Z
	// is 17:00 at +07:00, and that is what a client will be shown.
	if h := d.Time.In(testZone).Hour(); h != 17 {
		t.Errorf("local hour: got %d, want 17", h)
	}
}

// TestShorterLayoutDoesNotSwallowLongerInput backs the ordering argument in
// parseLayouts' comment.
//
// time.Parse requires the layout to consume the whole input, so "2006-01-02"
// cannot match a full timestamp by prefix. That is the property that makes the
// list order safe to reason about; if it ever stopped holding, every timestamp
// would silently truncate to midnight.
func TestShorterLayoutDoesNotSwallowLongerInput(t *testing.T) {
	t.Parallel()

	_, err := time.ParseInLocation(
		"2006-01-02", "2024-03-15T10:30:45",
		time.Local)
	if err == nil {
		t.Fatal("the date-only layout matched a full timestamp — " +
			"parseLayouts' ordering rationale no longer holds")
	}

	var d Datetime
	if err := d.UnmarshalJSON([]byte(`"2024-03-15T10:30:45"`)); err != nil {
		t.Fatalf("UnmarshalJSON: %v", err)
	}
	if d.Time.Hour() != 10 || d.Time.Minute() != 30 || d.Time.Second() != 45 {
		t.Errorf(
			"got %s, want the full time preserved",
			d.Time.Format(time.RFC3339))
	}
}

// TestUnmarshalJSONEmptyAndNull covers the two inputs that mean "no value".
func TestUnmarshalJSONEmptyAndNull(t *testing.T) {
	t.Parallel()

	inputs := []string{`null`, `""`, `"null"`, `"   "`, `"  null  "`}
	for _, input := range inputs {
		t.Run(input, func(t *testing.T) {
			t.Parallel()

			// Start non-zero so a no-op would fail.
			d := Datetime{Time: time.Now()}
			if err := d.UnmarshalJSON([]byte(input)); err != nil {
				t.Fatalf("UnmarshalJSON(%s): %v", input, err)
			}
			if !d.Time.IsZero() {
				t.Errorf(
					"got %s, want the zero Datetime",
					d.Time.Format(time.RFC3339))
			}
		})
	}
}

// TestUnmarshalJSONRejectsGarbage checks the failure path returns an error
// rather than a zero value, which would look like a legitimately empty field.
func TestUnmarshalJSONRejectsGarbage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
	}{
		{"free text", `"not a date"`},
		{"US order", `"03/15/2024"`},
		{"impossible month", `"2024-13-01"`},
		{"impossible day", `"2024-02-30"`},
		{"trailing garbage", `"2024-03-15 extra"`},
		{"number", `1710500000`},

		// Documented limitation: parse strips quotes with a cutset rather than
		// decoding a JSON string, so an escape sequence arrives verbatim and
		// simply fails to match a layout. Pinned here so the behaviour is a
		// known trade-off rather than a surprise if this routine is ever
		// reused for free text.
		{"unprocessed escape sequence", `"2024-03-15T10:00:00\u005A"`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var d Datetime
			if err := d.UnmarshalJSON([]byte(tc.input)); err == nil {
				t.Errorf(
					"UnmarshalJSON(%s) succeeded, want an error "+
						"(got %s)",
					tc.input,
					d.Time.Format(time.RFC3339))
			}
		})
	}
}

// TestParseTolerantOfSurroundingNoise pins the two documented laxities in
// parse's trimming, both of which exist so Scan can reuse UnmarshalJSON by
// wrapping a bare driver value in quotes.
//
// Repeated quotes: strings.Trim removes every leading and trailing quote, not
// one pair, so genuinely malformed JSON is accepted. Recorded as a deliberate
// trade-off so a stricter rewrite is a visible decision.
//
// Whitespace outside the quotes: trimmed before the cutset runs, so the quotes
// are still reachable. encoding/json never produces that shape, but
// UnmarshalText serves query and form binding, where a stray space around the
// value is ordinary.
func TestParseTolerantOfSurroundingNoise(t *testing.T) {
	t.Parallel()

	inputs := []string{
		`""2024-03-15""`,
		`  "2024-03-15"  `,
		`"  2024-03-15  "`,
	}

	for _, input := range inputs {
		t.Run(input, func(t *testing.T) {
			t.Parallel()

			var d Datetime
			if err := d.UnmarshalJSON([]byte(input)); err != nil {
				t.Fatalf("UnmarshalJSON(%s): %v", input, err)
			}
			if !d.Time.Equal(local(2024, 3, 15, 0, 0, 0)) {
				t.Errorf(
					"got %s, want local midnight",
					d.Time.Format(time.RFC3339))
			}
		})
	}
}

// TestMarshalJSON covers the output contract: RFC 3339, always in the local
// zone, with a zero value becoming null.
func TestMarshalJSON(t *testing.T) {
	t.Parallel()

	jst := time.FixedZone("JST", 9*3600)

	tests := []struct {
		name string
		in   time.Time
		want string
	}{
		{"zero marshals as null", time.Time{}, `null`},
		{"local value keeps its wall clock", local(2026, 8, 4, 14, 30, 0),
			`"2026-08-04T14:30:00+07:00"`},

		// The two values below are the same instant as the one above, labelled
		// in another zone. They must come out identically: MarshalJSON
		// converts into time.Local rather than echoing whatever zone the value
		// happened to carry, which is what keeps every response's offset the
		// same.
		{"UTC value is converted, not echoed",
			time.Date(2026, 8, 4, 7, 30, 0, 0, time.UTC),
			`"2026-08-04T14:30:00+07:00"`},
		{"foreign offset is converted",
			time.Date(2026, 8, 4, 16, 30, 0, 0, jst),
			`"2026-08-04T14:30:00+07:00"`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			b, err := Datetime{Time: tc.in}.MarshalJSON()
			if err != nil {
				t.Fatalf("MarshalJSON: %v", err)
			}
			if string(b) != tc.want {
				t.Errorf("got %s, want %s", b, tc.want)
			}
		})
	}
}

// TestMarshalJSONDropsFractionalSeconds pins the documented asymmetry between
// the parse and format sides.
//
// RFC3339Nano is accepted on the way in; time.RFC3339 is emitted on the way
// out. Harmless against DATETIME2(0), but it means marshal(unmarshal(x)) is
// not always x — which is worth a failing test if anyone ever relies on it.
func TestMarshalJSONDropsFractionalSeconds(t *testing.T) {
	t.Parallel()

	var d Datetime
	err := d.UnmarshalJSON([]byte(`"2024-03-15T10:00:00.123456789Z"`))
	if err != nil {
		t.Fatalf("UnmarshalJSON: %v", err)
	}
	if d.Time.Nanosecond() != 123456789 {
		t.Errorf(
			"nanoseconds: got %d, want them preserved in memory",
			d.Time.Nanosecond())
	}

	b, err := d.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	if got, want := string(b), `"2024-03-15T17:00:00+07:00"`; got != want {
		t.Errorf(
			"got %s, want %s — fractional seconds are dropped on output",
			got,
			want)
	}
}

// TestRoundTripIsStableAfterOnePass is the useful form of the round-trip
// property: the first marshal normalises (zone pinned, fractions dropped),
// and every pass after that is a fixed point.
//
// Clients re-submitting a value they were served must not see it drift, which
// is the guarantee that actually matters at the API boundary.
func TestRoundTripIsStableAfterOnePass(t *testing.T) {
	t.Parallel()

	inputs := []string{
		`"2024-03-15T10:00:00Z"`,
		`"2024-03-15T10:30:45"`,
		`"2024-03-15"`,
		`"2024-03-15 10:30:45"`,
	}

	for _, in := range inputs {
		t.Run(in, func(t *testing.T) {
			t.Parallel()

			var first Datetime
			if err := first.UnmarshalJSON([]byte(in)); err != nil {
				t.Fatalf("UnmarshalJSON(%s): %v", in, err)
			}
			once, err := first.MarshalJSON()
			if err != nil {
				t.Fatalf("MarshalJSON: %v", err)
			}

			var second Datetime
			if err := second.UnmarshalJSON(once); err != nil {
				t.Fatalf("re-unmarshalling %s: %v", once, err)
			}
			twice, err := second.MarshalJSON()
			if err != nil {
				t.Fatalf("MarshalJSON: %v", err)
			}

			if string(once) != string(twice) {
				t.Errorf("not stable: %s then %s", once, twice)
			}
			if !first.Time.Equal(second.Time) {
				t.Errorf(
					"instant drifted: %s then %s",
					first.Time,
					second.Time)
			}
		})
	}
}

// TestShadowingBeatsTimeTimeStrictness is the whole reason this type exists.
//
// The same JSON that a time.Time field rejects must be accepted by a Datetime
// field. Renaming the embedded field — the hazard called out in the type's
// doc comment — un-shadows time.Time's RFC 3339-only UnmarshalJSON and this
// test is where that shows up, rather than in a browser form three
// environments later.
func TestShadowingBeatsTimeTimeStrictness(t *testing.T) {
	t.Parallel()

	const body = `{"issued_at":"2024-03-15"}`

	var strict struct {
		IssuedAt time.Time `json:"issued_at"`
	}
	if err := json.Unmarshal([]byte(body), &strict); err == nil {
		t.Fatal("time.Time accepted a date-only string; the premise of " +
			"this package no longer holds")
	}

	var lax struct {
		IssuedAt *Datetime `json:"issued_at"`
	}
	if err := json.Unmarshal([]byte(body), &lax); err != nil {
		t.Fatalf("Datetime rejected a date-only string: %v", err)
	}
	if lax.IssuedAt == nil {
		t.Fatal("IssuedAt: got nil, want a parsed value")
	}
	if !lax.IssuedAt.Time.Equal(local(2024, 3, 15, 0, 0, 0)) {
		t.Errorf(
			"got %s, want local midnight",
			lax.IssuedAt.Time.Format(time.RFC3339))
	}
}

// TestBothNullRoutesSerialiseIdentically checks the claim that a nil pointer
// and a non-nil pointer holding a zero Datetime are indistinguishable on the
// wire, so nothing downstream has to tell them apart.
func TestBothNullRoutesSerialiseIdentically(t *testing.T) {
	t.Parallel()

	type model struct {
		IssuedAt *Datetime `json:"issued_at"`
	}

	nilPointer, err := json.Marshal(model{IssuedAt: nil})
	if err != nil {
		t.Fatalf("marshal nil pointer: %v", err)
	}
	zeroValue, err := json.Marshal(model{IssuedAt: &Datetime{}})
	if err != nil {
		t.Fatalf("marshal zero value: %v", err)
	}

	const want = `{"issued_at":null}`
	if string(nilPointer) != want {
		t.Errorf("nil pointer: got %s, want %s", nilPointer, want)
	}
	if string(zeroValue) != want {
		t.Errorf("zero value: got %s, want %s", zeroValue, want)
	}
}

// TestEmbeddedTimeMethodsArePromoted spot-checks the free method set that
// embedding buys, since the doc comment sells it as a reason for the design.
func TestEmbeddedTimeMethodsArePromoted(t *testing.T) {
	t.Parallel()

	early := Datetime{Time: local(2024, 1, 1, 0, 0, 0)}
	late := Datetime{Time: local(2024, 12, 31, 0, 0, 0)}

	if !early.Before(late.Time) {
		t.Error("Before: got false, want true")
	}
	if got := early.Year(); got != 2024 {
		t.Errorf("Year: got %d, want 2024", got)
	}
	if got := late.Sub(early.Time); got <= 0 {
		t.Errorf("Sub: got %v, want a positive duration", got)
	}
}

// TestScan covers every source type the switch handles.
func TestScan(t *testing.T) {
	t.Parallel()

	want := local(2024, 3, 15, 10, 30, 45)

	tests := []struct {
		name string
		src  any
		want time.Time
		zero bool
	}{
		{name: "nil is SQL NULL", src: nil, zero: true},
		{name: "time.Time passes through", src: want, want: want},
		{name: "byte slice", src: []byte("2024-03-15 10:30:45"), want: want},
		{name: "string", src: "2024-03-15 10:30:45", want: want},
		{name: "RFC3339 string", src: "2024-03-15T10:30:45+07:00",
			want: want},
		{name: "date-only byte slice", src: []byte("2024-03-15"),
			want: local(2024, 3, 15, 0, 0, 0)},
		{name: "empty byte slice is NULL-ish", src: []byte(""), zero: true},
		{name: "empty string is NULL-ish", src: "", zero: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Non-zero so a no-op Scan would fail.
			d := Datetime{Time: time.Now()}
			if err := d.Scan(tc.src); err != nil {
				t.Fatalf("Scan(%#v): %v", tc.src, err)
			}
			if tc.zero {
				if !d.Time.IsZero() {
					t.Errorf(
						"got %s, want the zero Datetime",
						d.Time.Format(time.RFC3339))
				}
				return
			}
			if !d.Time.Equal(tc.want) {
				t.Errorf(
					"got %s, want %s",
					d.Time.Format(time.RFC3339),
					tc.want.Format(time.RFC3339))
			}
		})
	}
}

// TestScanTimeTimeIsStoredVerbatim guards the primary path for both
// configured drivers.
//
// A time.Time from the driver is already an instant in a known zone;
// re-parsing or converting it here would be a second chance to get the zone
// wrong for no benefit. The location assertion is what distinguishes "stored
// as-is" from a silent .UTC() creeping back in.
func TestScanTimeTimeIsStoredVerbatim(t *testing.T) {
	t.Parallel()

	fromDriver := time.Date(2024, 3, 15, 10, 0, 0, 0, testZone)

	var d Datetime
	if err := d.Scan(fromDriver); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if !d.Time.Equal(fromDriver) {
		t.Errorf("instant: got %s, want %s", d.Time, fromDriver)
	}
	if d.Time.Location() != fromDriver.Location() {
		t.Errorf(
			"location: got %v, want %v — the driver's value must not be "+
				"relabelled",
			d.Time.Location(),
			fromDriver.Location())
	}
}

// TestScanDoesNotWriteIntoTheDriversBuffer is the test for the allocation
// decision inside the []byte case.
//
// Drivers are entitled to hand back a scan buffer they will refill for the
// next row. Appending a quote into that slice writes past len into shared
// backing memory whenever cap exceeds len — corrupting the NEXT row's bytes,
// which surfaces as an unparseable date on an unrelated record and is close
// to impossible to trace back here.
//
// The setup reproduces exactly that shape: a slice with spare capacity,
// filled with a sentinel so any write is visible.
func TestScanDoesNotWriteIntoTheDriversBuffer(t *testing.T) {
	t.Parallel()

	const sentinel = 0xAA

	backing := make([]byte, 0, 64)
	backing = append(backing, "2024-03-15 10:30:45"...)

	spare := backing[len(backing):cap(backing)]
	for i := range spare {
		spare[i] = sentinel
	}

	src := backing
	before := string(src)

	var d Datetime
	if err := d.Scan(src); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	for i, b := range spare {
		if b != sentinel {
			t.Fatalf(
				"Scan wrote 0x%02X into the driver's spare capacity at "+
					"offset %d — the quoted slice must be freshly allocated, "+
					"not appended into the source", b, i)
		}
	}
	if string(src) != before {
		t.Errorf(
			"Scan modified the source slice: got %q, want %q",
			src,
			before)
	}
}

// TestScanRejectsUnknownTypes checks the default branch, including that the
// concrete type reaches the message. The %T is the only thing that makes a
// driver change diagnosable from a log line.
func TestScanRejectsUnknownTypes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		src  any
		want string
	}{
		{"int64", int64(1710500000), "int64"},
		{"float64", 1.5, "float64"},
		{"bool", true, "bool"},

		// Explicitly listed in the doc comment as deliberately unhandled: a
		// driver that starts returning these should be noticed, not absorbed.
		{"pointer to time", &time.Time{}, "*time.Time"},
		{"sql.RawBytes", sql.RawBytes("2024-03-15"), "sql.RawBytes"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var d Datetime
			err := d.Scan(tc.src)
			if err == nil {
				t.Fatalf("Scan(%T) succeeded, want an error", tc.src)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf(
					"error %q does not name the concrete type %q",
					err,
					tc.want)
			}
		})
	}
}

// TestValue covers the write side: a zero value becomes SQL NULL, and every
// other value is converted into time.Local before it reaches the driver.
//
// The conversion assertion is the one with teeth, and the tempting wrong
// version of it is to require Value NOT to convert, on the belief that
// go-mssqldb applies the DSN timezone parameter on the encode path. It does
// not — that parameter is read when DECODING, and a plain time.Time is sent
// as DATETIMEOFFSET carrying its own offset, which SQL Server then truncates
// into DATETIME2/DATE. See Value's doc comment.
func TestValue(t *testing.T) {
	t.Parallel()

	t.Run("zero becomes SQL NULL", func(t *testing.T) {
		t.Parallel()

		v, err := Datetime{}.Value()
		if err != nil {
			t.Fatalf("Value: %v", err)
		}
		if v != nil {
			t.Errorf("got %#v, want nil", v)
		}
	})

	t.Run("instant survives, zone is pinned to local", func(t *testing.T) {
		t.Parallel()

		in := time.Date(2024, 3, 15, 10, 0, 0, 0, time.UTC)
		v, err := Datetime{Time: in}.Value()
		if err != nil {
			t.Fatalf("Value: %v", err)
		}
		got, ok := v.(time.Time)
		if !ok {
			t.Fatalf("got %T, want time.Time", v)
		}
		if !got.Equal(in) {
			t.Errorf("instant: got %s, want %s — .In() must convert, not "+
				"relabel",
				got,
				in)
		}
		if got.Location() != time.Local {
			t.Errorf("location: got %v, want %v — Value pins the wall clock "+
				"the driver sees",
				got.Location(),
				time.Local)
		}
		if h := got.Hour(); h != 17 {
			t.Errorf("local hour: got %d, want 17 (10:00Z is 17:00 at +07)", h)
		}
	})

	t.Run("a local value is left where it is", func(t *testing.T) {
		t.Parallel()

		in := local(2024, 3, 15, 10, 0, 0)
		v, err := Datetime{Time: in}.Value()
		if err != nil {
			t.Fatalf("Value: %v", err)
		}
		got := v.(time.Time)
		if !got.Equal(in) || got.Hour() != 10 {
			t.Errorf("got %s, want %s unchanged", got, in)
		}
	})
}

// TestValueDoesNotShiftTheStoredDate is the regression test for the
// off-by-one day, and it asserts against the value the DRIVER will act on
// rather than against Value's return type.
//
// go-mssqldb sends a time.Time as DATETIMEOFFSET(7) built from the value's
// own zone offset, and SQL Server's implicit conversion into DATETIME2 or
// DATE copies the local date and time and truncates the offset. The wall
// clock in the returned value's own location is therefore exactly what lands
// in the column, which is what storedWallClock below stands in for.
//
// 20:00Z on the 15th is 03:00 on the 16th at +07:00. Unconverted, a DATE
// column would record the 15th while MarshalJSON served the 16th for the
// same row — two different days for one instant, with nothing in either
// layer disagreeing out loud.
func TestValueDoesNotShiftTheStoredDate(t *testing.T) {
	t.Parallel()

	storedWallClock := func(d Datetime) string {
		v, err := d.Value()
		if err != nil {
			t.Fatalf("Value: %v", err)
		}
		return v.(time.Time).Format("2006-01-02 15:04:05")
	}

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"naive date", `"2024-03-15"`, "2024-03-15 00:00:00"},
		{"naive datetime", `"2024-03-15T10:30:45"`, "2024-03-15 10:30:45"},
		{"UTC crossing midnight", `"2024-03-15T20:00:00Z"`,
			"2024-03-16 03:00:00"},
		{"UTC not crossing", `"2024-03-15T02:00:00Z"`,
			"2024-03-15 09:00:00"},
		{"foreign offset", `"2024-03-15T12:00:00+09:00"`,
			"2024-03-15 10:00:00"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var d Datetime
			if err := d.UnmarshalJSON([]byte(tc.input)); err != nil {
				t.Fatalf("UnmarshalJSON(%s): %v", tc.input, err)
			}
			if got := storedWallClock(d); got != tc.want {
				t.Errorf(
					"%s would be stored as %q, want %q",
					tc.input,
					got,
					tc.want)
			}
		})
	}
}

// TestValueAgreesWithMarshalJSON states the property the two methods exist to
// share, rather than restating either one's expected output.
//
// What a client is shown and what the database is told must be the same wall
// clock. Written as an agreement so it keeps holding if the output format
// changes, and so a future edit to one method that forgets the other fails
// here instead of in a report someone runs at the end of the month.
func TestValueAgreesWithMarshalJSON(t *testing.T) {
	t.Parallel()

	inputs := []string{
		`"2024-03-15T20:00:00Z"`,
		`"2024-03-15T12:00:00+09:00"`,
		`"2024-03-15T10:30:45"`,
		`"2024-03-15"`,
	}

	for _, in := range inputs {
		t.Run(in, func(t *testing.T) {
			t.Parallel()

			var d Datetime
			if err := d.UnmarshalJSON([]byte(in)); err != nil {
				t.Fatalf("UnmarshalJSON(%s): %v", in, err)
			}

			v, err := d.Value()
			if err != nil {
				t.Fatalf("Value: %v", err)
			}
			b, err := d.MarshalJSON()
			if err != nil {
				t.Fatalf("MarshalJSON: %v", err)
			}

			// RFC 3339 up to the offset, which MarshalJSON always renders as
			// +07:00 and Value carries in the location instead.
			toDriver := v.(time.Time).Format("2006-01-02T15:04:05")
			toClient := string(b)
			if want := `"` + toDriver + `+07:00"`; toClient != want {
				t.Errorf(
					"driver would be told %s, client is told %s",
					toDriver,
					toClient)
			}
		})
	}
}

// TestValueIsDriverCompatible pushes both null routes through database/sql's
// own parameter converter rather than calling Value directly.
//
// The nil-pointer case is the one worth the extra layer: calling Value on a
// nil *Datetime would dereference nil, and it only works because database/sql
// detects a nil pointer whose Value has a value receiver and substitutes NULL
// without calling it. That is a property of the RECEIVER CHOICE, not of any
// code in this file, and it is invisible until a model with an unset optional
// date is inserted.
func TestValueIsDriverCompatible(t *testing.T) {
	t.Parallel()

	at10 := local(2024, 3, 15, 10, 0, 0)

	tests := []struct {
		name string
		in   any
		want any
	}{
		{"nil pointer", (*Datetime)(nil), nil},
		{"pointer to zero", &Datetime{}, nil},
		{"pointer to value", &Datetime{Time: at10}, at10},
		{"value", Datetime{Time: at10}, at10},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := driver.DefaultParameterConverter.ConvertValue(tc.in)
			if err != nil {
				t.Fatalf("ConvertValue: %v", err)
			}
			if tc.want == nil {
				if got != nil {
					t.Errorf("got %#v, want nil (SQL NULL)", got)
				}
				return
			}
			gotTime, ok := got.(time.Time)
			if !ok {
				t.Fatalf("got %T, want time.Time", got)
			}
			if !gotTime.Equal(tc.want.(time.Time)) {
				t.Errorf("got %s, want %s", gotTime, tc.want)
			}
		})
	}
}

// TestScanValueRoundTrip closes the loop the way a real row does: write
// through Value, read the same value back through Scan, and confirm the
// instant survives.
func TestScanValueRoundTrip(t *testing.T) {
	t.Parallel()

	original := Datetime{Time: local(2024, 3, 15, 10, 30, 45)}

	v, err := original.Value()
	if err != nil {
		t.Fatalf("Value: %v", err)
	}

	var back Datetime
	if err := back.Scan(v); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if !back.Time.Equal(original.Time) {
		t.Errorf("got %s, want %s", back.Time, original.Time)
	}
}

// TestUnmarshalTextAcceptsEveryLayout is the reason UnmarshalText exists.
//
// Without it, *Datetime still satisfies encoding.TextUnmarshaler — it
// promotes time.Time's, which takes RFC 3339 and nothing else. So the failure
// this guards against is not a missing interface but a SILENTLY STRICTER one,
// and the only way to see it is to push a non-RFC-3339 layout through the
// text path and check it lands.
//
// The layouts are the same list UnmarshalJSON accepts, deliberately: a date
// that works in a request body and fails in a query string is the bug, and
// one shared table is what keeps the two from drifting apart.
func TestUnmarshalTextAcceptsEveryLayout(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  time.Time
	}{
		{"RFC3339 UTC", "2024-03-15T10:00:00Z",
			time.Date(2024, 3, 15, 10, 0, 0, 0, time.UTC)},
		{"datetime-local with seconds", "2024-03-15T10:30:45",
			local(2024, 3, 15, 10, 30, 45)},
		{"datetime-local without seconds", "2024-03-15T10:30",
			local(2024, 3, 15, 10, 30, 0)},
		{"SQL text timestamp", "2024-03-15 10:30:45",
			local(2024, 3, 15, 10, 30, 45)},
		{"date only", "2024-03-15", local(2024, 3, 15, 0, 0, 0)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var d Datetime
			if err := d.UnmarshalText([]byte(tc.input)); err != nil {
				t.Fatalf("UnmarshalText(%q): %v", tc.input, err)
			}
			if !d.Time.Equal(tc.want) {
				t.Errorf(
					"instant: got %s, want %s",
					d.Time.Format(time.RFC3339Nano),
					tc.want.Format(time.RFC3339Nano))
			}
		})
	}
}

// TestTextIsStricterOnABareTimeTime states the premise the test above depends
// on, the same way TestShadowingBeatsTimeTimeStrictness does for JSON.
//
// If time.Time ever relaxed its TextUnmarshaler, UnmarshalText would stop
// being load-bearing and this fails — which is the signal to re-read whether
// the method is still worth keeping, not merely a broken assertion.
func TestTextIsStricterOnABareTimeTime(t *testing.T) {
	t.Parallel()

	var strict time.Time
	if err := strict.UnmarshalText([]byte("2024-03-15")); err == nil {
		t.Fatal("time.Time accepted a date-only string; UnmarshalText no " +
			"longer adds anything")
	}

	var lax Datetime
	if err := lax.UnmarshalText([]byte("2024-03-15")); err != nil {
		t.Fatalf("Datetime rejected a date-only string: %v", err)
	}
	if !lax.Time.Equal(local(2024, 3, 15, 0, 0, 0)) {
		t.Errorf(
			"got %s, want local midnight",
			lax.Time.Format(time.RFC3339))
	}
}

// TestUnmarshalTextAndJSONAgree is the property that matters at the API
// boundary: the same string means the same instant whether it arrives in a
// body or a query parameter.
//
// Asserting agreement rather than two separate expected values is what makes
// this survive a change to the layout list — add a layout to parseLayouts and
// this keeps holding without being edited, which is the point.
func TestUnmarshalTextAndJSONAgree(t *testing.T) {
	t.Parallel()

	inputs := []string{
		"2024-03-15T10:00:00Z",
		"2024-03-15T10:30:45",
		"2024-03-15T10:30",
		"2024-03-15 10:30:45",
		"2024-03-15",
		"",
	}

	for _, in := range inputs {
		t.Run(in, func(t *testing.T) {
			t.Parallel()

			var viaText Datetime
			textErr := viaText.UnmarshalText([]byte(in))

			var viaJSON Datetime
			jsonErr := viaJSON.UnmarshalJSON([]byte(`"` + in + `"`))

			if (textErr == nil) != (jsonErr == nil) {
				t.Fatalf(
					"disagreed on validity: text=%v json=%v",
					textErr,
					jsonErr)
			}
			if textErr != nil {
				return
			}
			if !viaText.Time.Equal(viaJSON.Time) {
				t.Errorf(
					"text gave %s, json gave %s",
					viaText.Time,
					viaJSON.Time)
			}
		})
	}
}
