// Package datetime carries one date/time value from a browser form,
// through JSON, into a SQL column and back out again without the wall
// clock moving.
//
// It exists because the standard library is strict at both ends of that
// path, in two unrelated ways, and neither strictness is negotiable where
// it lives:
//
//   - encoding/json accepts RFC 3339 for a time.Time and nothing else. An
//     <input type="date"> submits "2024-03-15" and an <input
//     type="datetime-local"> submits "2024-03-15T14:30", so a plain
//     time.Time field rejects the two shapes a browser actually sends.
//   - time.Time implements neither sql.Scanner nor driver.Valuer, so what
//     a driver hands back and what it is given are whatever that driver
//     decided, and the two drivers in common use decided differently.
//
// Datetime is a time.Time that fixes both: it accepts every shape a form
// or a driver produces on the way in, and emits exactly one shape on the
// way out.
//
// # time.Local is the pivot, and it is a process-wide decision
//
// Every offsetless value in this package resolves in time.Local. A naive
// "2024-03-15" from a form, a DATETIME column with no offset, and the
// string in an API response all mean the same wall clock only while the
// process, the driver and the database agree on one zone.
//
// That agreement is not this package's to enforce. What it does is take
// one side of it consistently, so the remaining work is a startup
// decision rather than a per-field one:
//
//	parse          offsetless input resolves in time.Local
//	MarshalJSON    converts to time.Local before formatting
//	Value          converts to time.Local before handing over
//
// The process sets time.Local once — from TZ, or by assignment at
// startup — and the driver is configured with the same location. Get
// those two right and a date entered as the 15th is the 15th at every
// layer. Get them out of step and the error is a whole offset: a value
// crossing midnight lands on the wrong day, which is the failure mode
// every comment in this package is ultimately about.
//
// Nothing here reads a location from configuration, and no location can
// be passed in. A per-call location would make the zone a property of
// each field rather than of the deployment, which is the opposite of what
// a shared pipeline needs, and a package-level location variable would be
// a second global competing with the one Go already provides.
//
// # What a value does at each boundary
//
//	in    UnmarshalJSON  every layout in parseLayouts, plus null and ""
//	in    UnmarshalText   the same, for query, form and path binding
//	in    Scan            nil, time.Time, []byte, string
//	out   MarshalJSON     RFC 3339 with an explicit offset, or null
//	out   Value           time.Time in time.Local, or nil for SQL NULL
//
// The in-list is deliberately wide and the out-list deliberately narrow.
// Anything a browser or a driver can produce is accepted; exactly one
// shape leaves. That asymmetry is the whole design: a response never
// varies with how the value arrived.
//
// Emptiness collapses on the way in and stays collapsed on the way out.
// JSON null, an empty string, and SQL NULL all become the zero Datetime,
// and a zero Datetime marshals as null and writes as NULL. A model
// holding *Datetime therefore has two routes to null — a nil pointer and
// a pointer to a zero value — and nothing downstream has to tell them
// apart.
//
// # Where the round trip is not exact
//
// Two places, both documented where they happen and both cheaper to
// state than to fix:
//
//   - Fractional seconds are parsed and kept in memory but dropped by
//     MarshalJSON, which formats with time.RFC3339. So
//     marshal(unmarshal(x)) is not always x for a sub-second value.
//   - A map[Datetime]T encodes through time.Time's text form rather than
//     through MarshalJSON, because encoding/json uses TextUnmarshaler for
//     map keys and there is deliberately no matching MarshalText. A zero
//     key comes out as "0001-01-01T00:00:00Z" rather than null.
//
// One value is unrepresentable by design: the zero instant itself,
// January 1 of year 1, which Value turns into NULL. No real date is
// affected, and the trade buys empty-form-field round-tripping without a
// separate flag.
//
// # What this package is not
//
// There is no date-only type, no time-of-day type and no duration type.
// A DATE column scans into the same Datetime as a DATETIME one and
// carries midnight in time.Local. If a schema needs the distinction
// enforced rather than implied, that is a second type and not a flag on
// this one.
//
// # What the tests hold in place
//
// The unit suite needs no server and covers the layout table, the
// emptiness collapse, both receiver forms and each Scan source type.
//
// datetime_integration_test.go is the other half, and it exists because
// the argument in Value's doc comment is about driver BEHAVIOUR rather
// than about anything in this file. It writes a Datetime into real DATE
// and DATETIME columns on each engine whose DSN is set and reads it back,
// which is the only way to show that the conversion Value performs is the
// one keeping the two engines in step. It shares the DSN variables and
// build tag of the database package's integration suite:
//
//	DB_TEST_MYSQL_DSN
//		user:pass@tcp(host:3306)/scratch?parseTime=true&loc=Asia%2FJakarta
//	DB_TEST_SQLSERVER_DSN
//		sqlserver://user:pass@host?database=scratch&timezone=Asia%2FJakarta
//
// The zone in each DSN must be the zone the test PROCESS runs in — here,
// run with TZ=Asia/Jakarta — because that agreement is the precondition
// this package documents. It is not optional decoration. Against MariaDB
// with the zone left off, a naive "2024-03-15" came back as the 14th and
// 14:30 came back as 07:30: the whole-offset drift this package exists to
// make visible, reproduced by following a DSN that omitted it. In a UTC
// process the offset tests skip, since the two readings coincide there, so
// a run that is meant to test anything sets a zone that is not UTC.
//
//	go test -tags integration -run Integration ./datetime
package datetime
