package datetime

import (
	"database/sql/driver"
	"fmt"
	"strings"
	"time"
)

// Datetime is a time.Time that accepts multiple date/time string formats when
// unmarshalling JSON. This is necessary because different HTML input types
// produce different string formats, and Go's encoding/json only accepts
// RFC 3339 for time.Time.
//
// It also implements sql.Scanner and driver.Valuer so sqlx can read and write
// time columns transparently — SQL drivers typically return time columns as
// time.Time, which Scan accepts directly without re-parsing.
//
// On marshal, Datetime always emits a full RFC 3339 string so API responses
// are consistent regardless of which input format was originally parsed.
//
// Usage in a model struct:
//
//	IssuedAt *datetime.Datetime `db:"issued_at" json:"issued_at"`
//
// Models use POINTERS to this type, which gives two distinct routes to a JSON
// null: a nil pointer, and a non-nil pointer holding a zero Datetime. Both
// serialise identically and both write SQL NULL through Value, so nothing
// downstream has to tell them apart.
//
// Embedding time.Time rather than wrapping it in a named field promotes every
// time.Time method — Before, Sub, Year, Format — onto Datetime for free.
//
// Three of the five methods below SHADOW a time.Time method: MarshalJSON,
// UnmarshalJSON, and UnmarshalText. That shadowing is the entire point of the
// type, and it rests on the three names alone. Misspell or rename one and the
// promoted time.Time method takes its place without a word — it satisfies the
// same interface, so nothing fails to compile — bringing back RFC 3339-only
// parsing on the way in, or, on the way out, a value in its own zone and a
// zero instant where null belongs.
//
// The other two ADD behaviour rather than replacing it: time.Time
// implements neither sql.Scanner nor driver.Valuer, so Scan and Value have
// nothing to shadow. A bare time.Time field cannot be scanned from or
// written to a column through this package's rules at all — that capability
// arrives only with this wrapper.
type Datetime struct {
	time.Time
}

// parseLayouts lists every format accepted by UnmarshalJSON and the string
// path of Scan, tried in order from most to least specific. The first
// successful parse wins.
//
// The two RFC 3339 entries are equivalent in practice: when parsing (only),
// Go's time.Parse accepts an optional fractional-second component immediately
// after the seconds field even if the layout does not contain one, so the
// plain RFC3339 layout also parses "...T10:00:00.123Z". RFC3339Nano is kept
// and listed first purely to make that intent explicit rather than relying on
// the parsing subtlety. Note the asymmetry with output: MarshalJSON formats
// with time.RFC3339, so fractional seconds survive in memory after parsing but
// are dropped when the value is re-serialised.
//
// The remaining entries cover the formats produced by HTML date/time input
// elements, which do not include a timezone offset:
//
//   - "2006-01-02T15:04:05" — <input type="datetime-local"> with seconds
//   - "2006-01-02T15:04"    — <input type="datetime-local"> no seconds
//   - "2006-01-02"          — <input type="date">
//
// One further entry, "2006-01-02 15:04:05", is the space-separated SQL
// text timestamp. It comes from the Scan side rather than from a browser —
// a driver or a CAST that hands back a stringified datetime — which is why
// it sits with the HTML formats despite not being one.
//
// Order matters, and the ordering here is correct for a reason worth keeping:
// time.Parse requires the layout to consume the ENTIRE input, so a shorter
// layout cannot match a longer string by prefix. "2006-01-02" does not swallow
// "2006-01-02T15:04:05". The real ordering constraint is that offset-bearing
// layouts precede naive ones, so an RFC 3339 string is never reinterpreted as
// a naive local time with trailing garbage.
var parseLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04:05", // datetime-local with seconds, no timezone
	"2006-01-02T15:04",    // datetime-local without seconds, no timezone
	"2006-01-02 15:04:05", // SQL text timestamp
	"2006-01-02",          // <input type="date">, and SQL DATE as text
}

// parse is the shared string→time routine behind UnmarshalJSON and Scan.
//
// Layouts without a timezone offset resolve in time.Local, not in UTC.
// ParseInLocation applies that location only when the input carries no zone
// indicator, so the two RFC 3339 layouts keep whatever offset they were
// sent with and are unaffected.
//
// Local rather than UTC because the whole pipeline is meant to agree on one
// zone: a driver configured with the same location LABELS the offsetless
// columns it decodes in it; Value converts into it on the way back out; and
// MarshalJSON emits in it. A naive "2024-03-15" therefore means midnight in
// the configured zone at every layer, which is what the person filling in
// the form meant.
//
// Parsing it as UTC instead would put a skew of the zone's whole offset
// between what the form says and what the driver writes back, showing up as
// a date landing on the wrong day whenever a value crosses midnight.
func parse(s string) (time.Time, error) {
	// Whitespace is trimmed on both sides of the quote-stripping, and the
	// quotes are removed with a cutset rather than a JSON unquote:
	// strings.Trim removes EVERY leading and trailing " character, not just
	// one pair. That is deliberate laxity — it lets the same routine serve
	// UnmarshalJSON, which receives a quoted JSON token, and Scan's string
	// path, which receives a bare value that the caller has wrapped in quotes
	// purely to reuse this code.
	//
	// The outer trim runs first so that whitespace OUTSIDE the quotes
	// does not stop the cutset from reaching them. encoding/json never
	// produces that shape, but UnmarshalText serves query and form
	// binding, where a stray space around the value is ordinary.
	//
	// The cost is that malformed input like `""2024-01-02""` is accepted
	// rather than rejected, and that a JSON string containing an escape
	// sequence (\u0032) is passed through unprocessed and simply fails to
	// parse. Neither matters for date fields; both would matter if this
	// routine were ever reused for free text.
	s = strings.TrimSpace(strings.Trim(strings.TrimSpace(s), `"`))

	// The "null" sentinel is a JSON literal, but this routine serves three
	// callers and the check cannot tell them apart. A column holding the
	// TEXT "null" therefore scans to the zero Datetime rather than
	// erroring, as does the bare word null arriving through UnmarshalText.
	// Neither is reachable while the columns are date or datetime types, a
	// driver having nothing to hand back that string from, and the
	// alternative is a second parse routine differing in one line.
	if s == "" || s == "null" {
		return time.Time{}, nil
	}
	for _, layout := range parseLayouts {
		// ParseInLocation, not Parse: identical for the offset-bearing
		// layouts, and the difference that matters for the naive ones.
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf(
		"datetime: cannot parse %q as a date/time", s)
}

// MarshalJSON implements json.Marshaler. It emits an RFC 3339 string carrying
// an explicit offset — "2026-08-04T14:30:00+07:00" — so API responses are
// consistent and timezone-unambiguous. A zero Datetime marshals as JSON null.
//
// Fractional seconds are dropped: time.RFC3339 has no fractional
// component, so a value parsed via RFC3339Nano keeps its nanoseconds in
// memory and loses them here. Harmless against a column with no sub-second
// precision, but it does mean marshal(unmarshal(x)) is not always x.
func (d Datetime) MarshalJSON() ([]byte, error) {
	if d.Time.IsZero() {
		return []byte("null"), nil
	}
	// .In(time.Local) rather than either .UTC() or a bare Format. It
	// converts — it does not relabel — so the instant is preserved, and it
	// pins the OUTPUT zone so every response carries the same offset
	// regardless of how the value arrived. A bare Format would echo whatever
	// zone the value happened to hold: "+07:00" for anything read from the
	// database, "Z" for a value still carrying an offset a client sent — one
	// response shape per input shape, which is the inconsistency this pins
	// down.
	//
	// With time.Local and the driver's own timezone setting resolved from
	// the same configured location, the round trip is closed: what Value
	// sends, what the engine stores, what the driver labels on the way
	// back, and what this emits are all the same zone. Value supplies the
	// write half — see its comment for why a driver may not supply it.
	return []byte(`"` + d.Time.In(time.Local).Format(time.RFC3339) + `"`), nil
}

// UnmarshalJSON implements json.Unmarshaler. It accepts all formats listed in
// parseLayouts as well as the JSON literal "null" and an empty string, both of
// which produce a zero Datetime (equivalent to time.Time{}).
//
// Timezone: layouts without an offset ("2006-01-02T15:04:05",
// "2006-01-02T15:04", "2006-01-02") resolve in time.Local, which the
// process is expected to set once at startup. An HTML date or
// datetime-local input submits exactly that shape, so a form filled in at
// 14:30 local time is stored as 14:30 local time — no offset needs
// appending on the client, and appending one is still honoured if a caller
// wants to pin a specific instant.
func (d *Datetime) UnmarshalJSON(b []byte) error {
	t, err := parse(string(b))
	if err != nil {
		return err
	}
	d.Time = t
	return nil
}

// UnmarshalText implements encoding.TextUnmarshaler, accepting exactly the
// layouts UnmarshalJSON accepts.
//
// This exists because of what Datetime would otherwise INHERIT. The type
// has no TextUnmarshaler of its own without this method, so *Datetime
// promotes time.Time's — which is RFC 3339 only, the strictness this whole
// package exists to avoid. encoding/json prefers json.Unmarshaler, so a
// JSON body is never affected; every other decoder is. Query, form and path
// binding in a typical web framework goes through a schema decoder that
// looks for encoding.TextUnmarshaler, so without this method a "2024-03-15"
// that parses in a request body would fail as ?date=2024-03-15 — the same
// string, accepted in one place and rejected in another, with nothing in
// either error saying why.
//
// Delegating to UnmarshalJSON rather than calling parse directly is safe
// because parse strips quotes with a cutset: unquoted text has none to
// strip and passes through unchanged. The one behavioural consequence is
// that the bare word null is treated as absent here too, matching the JSON
// side.
//
// One JSON case does come through here: encoding/json decodes MAP KEYS
// with TextUnmarshaler rather than json.Unmarshaler, so a map[Datetime]T
// decodes its keys through this method instead of time.Time's.
//
// There is deliberately no matching MarshalText. Adding one would shadow
// time.Time's, which nothing here wants shadowed, and the only place the
// absence shows is that same map-key path: a map[Datetime]T ENCODES through
// time.Time's RFC3339Nano text form, so a zero key marshals as
// "0001-01-01T00:00:00Z" rather than the null MarshalJSON would give it,
// and fractional seconds survive where MarshalJSON drops them. Asymmetric,
// rarely reached, and cheaper to write down than to fix.
func (d *Datetime) UnmarshalText(b []byte) error {
	return d.UnmarshalJSON(b)
}

// Scan implements sql.Scanner so sqlx can populate a *Datetime directly from a
// database column value. Four source values are handled:
//
//   - nil       → zero Datetime (SQL NULL).
//   - time.Time → stored as-is. This is the primary path for a driver
//     configured to decode time columns natively: MySQL with
//     parseTime=true, and go-mssqldb, which returns time.Time for its date
//     and time column types.
//   - []byte    → wrapped in double-quote bytes and delegated to
//     UnmarshalJSON. Fallback for MySQL without parseTime=true, or for any
//     driver that returns a raw byte slice for a time column.
//   - string    → wrapped in double-quote bytes and delegated to
//     UnmarshalJSON. Fallback for drivers or query results that stringify
//     time columns, such as a character column holding ISO 8601 text.
//
// Any other type returns a descriptive error that includes the concrete
// type, so a driver change is diagnosable from one log line rather than
// from a silent success.
func (d *Datetime) Scan(src any) error {
	if src == nil {
		d.Time = time.Time{}
		return nil
	}
	switch v := src.(type) {
	case time.Time:
		d.Time = v
		return nil
	case []byte:
		// Wrap the raw bytes in double quotes so they satisfy JSON string
		// syntax and can be forwarded to UnmarshalJSON without duplicating
		// the layout-parsing logic.
		//
		// Build the quoted slice in a freshly-allocated buffer rather than
		// appending into v: v is owned by the driver and may be a scan
		// buffer it reuses for the next row, so appending '"' into it could
		// write past len(v) into shared backing memory whenever
		// cap(v) > len(v). Allocating len(v)+2 up front keeps both quote
		// writes and the copy of v confined to our own buffer.
		quoted := make([]byte, 0, len(v)+2)
		quoted = append(quoted, '"')
		quoted = append(quoted, v...)
		quoted = append(quoted, '"')
		return d.UnmarshalJSON(quoted)
	case string:
		// Safe to concatenate here, unlike the []byte case above: string is
		// immutable and this allocates a new one regardless, so there is no
		// driver-owned buffer to write into.
		return d.UnmarshalJSON([]byte(`"` + v + `"`))
	}
	// Deliberately NOT handled: *time.Time and sql.RawBytes. Adding
	// speculative cases for values no configured driver produces would hide
	// a driver change behind a silent success. The %T in this message is
	// what makes such a change diagnosable in one look at the log.
	return fmt.Errorf(
		"datetime.Datetime: cannot scan type %T into Datetime",
		src)
}

// Value implements driver.Valuer so database/sql can convert a Datetime into a
// driver-native value for INSERT/UPDATE statements (e.g. via
// sqlx.NamedExecContext). A zero Datetime returns nil, which database/sql maps
// to SQL NULL.
//
// The instant is converted into time.Local before it is handed over. .In()
// converts rather than relabels, so the moment in time is untouched; what is
// pinned is the WALL CLOCK the driver gets to see. This is the write-side
// twin of MarshalJSON and exists for the same reason: every value leaving
// the process states its time in one zone, whichever zone it happened to
// arrive in.
//
// # Why this is not left to the driver
//
// Because drivers differ on whether they convert at all, and one of the two
// in use here does not:
//
//   - MySQL converts. writeExecutePacket and interpolateParams both format
//     v.In(mc.cfg.Loc), so with Loc set to the configured location the
//     value lands correctly whatever zone it carried. This conversion is a
//     no-op there.
//
//   - go-mssqldb does NOT convert. Its "timezone" DSN parameter is read on
//     the DECODE path only. A plain time.Time goes through makeParam, which
//     sends DATETIMEOFFSET(7) carrying the value's OWN offset, and SQL
//     Server's implicit conversion to DATETIME2 or DATE copies the local
//     date and time and truncates the offset. So the zone the value happens
//     to hold decides the wall clock that gets stored.
//
//     DATETIMEOFFSET is conditional on the negotiated protocol: makeParam
//     picks it only when loginAck.TDSVersion >= verTDS73, and otherwise
//     falls back to DATETIME via encodeDateTime. Nothing changes for this
//     conversion either way — that fallback also encodes the value's own
//     wall clock — and TDS 7.3 means SQL Server 2008, so the branch is not
//     reachable against a supported server. Noted because the paragraph
//     above reads as unconditional and is not.
//
// The failure this prevents: a body carrying "2024-03-15T20:00:00Z" is
// legal RFC 3339, parse accepts it and keeps it in UTC, and unconverted it
// would write 2024-03-15 into a DATE column — while every read path in this
// package calls that same instant 2024-03-16 in a zone ahead of UTC. It is
// the write-side twin of the off-by-one-day that ParseInLocation rules out
// on the way IN. Only offset-bearing input is exposed to it; naive input
// already resolves in time.Local, which is why a form-only schema can hide
// the gap indefinitely.
//
// Receiver asymmetry, stated once: Value and MarshalJSON take a VALUE
// receiver so both Datetime and *Datetime satisfy driver.Valuer and
// json.Marshaler, while Scan and UnmarshalJSON take a POINTER receiver
// because they must mutate. That is the conventional split and the reason a
// Datetime works whether a model holds it directly or behind a pointer.
func (d Datetime) Value() (driver.Value, error) {
	// IsZero, not a nil check: this is the write-side counterpart to Scan's
	// nil case, and it means a genuine zero instant (January 1, year 1,
	// 00:00:00 UTC) cannot be stored — it becomes NULL. No real date is
	// affected, and the tradeoff buys "empty form field" round-tripping to
	// NULL without a separate flag.
	//
	// Checked BEFORE the conversion, though the order happens not to
	// matter: IsZero compares the instant the value represents, which is
	// independent of the location it is labelled with, so .In() cannot
	// make a zero value non-zero or the reverse.
	if d.Time.IsZero() {
		return nil, nil
	}
	return d.Time.In(time.Local), nil
}
