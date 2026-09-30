//go:build integration

package datetime

// Integration tests for Scan and Value against real servers.
//
// Everything else in this package can be tested offline, because
// everything else is string handling. These two methods cannot: what they
// are FOR is a disagreement between drivers, and the argument in Value's
// doc comment — that go-mssqldb sends a value's own offset while
// go-sql-driver converts to the DSN's location — is argued from driver
// source. This is where it is observed, and each half only on its own
// engine's run: the MySQL half needs a MySQL or MariaDB DSN, and the SQL
// Server half a SQL Server one.
//
// The DSN variables and the build tag are the database package's, so one
// scratch server serves both suites:
//
//	DB_TEST_MYSQL_DSN
//		user:pass@tcp(host:3306)/scratch?parseTime=true&loc=Asia%2FJakarta
//	DB_TEST_SQLSERVER_DSN
//		sqlserver://user:pass@host?database=scratch&timezone=Asia%2FJakarta
//
//	go test -tags integration -run Integration ./datetime
//
// Each DSN must carry the SAME zone the test process runs in, since that
// agreement is the precondition this package documents rather than
// enforces: parseTime=true with loc= on MySQL, timezone= on SQL Server.
// A mismatch fails these tests, which is the correct outcome — it is the
// misconfiguration the whole package exists to make visible.
//
// The tests create and drop their own tables. Point the DSNs at a scratch
// database, never at one that holds data.

import (
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
	_ "github.com/microsoft/go-mssqldb"
)

// engine names one server and the column types it spells differently.
type engine struct {
	name     string
	driver   string
	dsnEnv   string
	dateCol  string
	stampCol string
	quote    func(string) string
}

// row is one row of the scratch table, both columns scanned through Scan.
type row struct {
	D  *Datetime `db:"d"`
	TS *Datetime `db:"ts"`
}

// engines lists both servers this suite can run against; forEachEngine
// runs each one whose DSN is set.
var engines = []engine{
	{
		name:     "mysql",
		driver:   "mysql",
		dsnEnv:   "DB_TEST_MYSQL_DSN",
		dateCol:  "DATE",
		stampCol: "DATETIME(0)",
		quote:    func(s string) string { return "`" + s + "`" },
	},
	{
		name:     "sqlserver",
		driver:   "sqlserver",
		dsnEnv:   "DB_TEST_SQLSERVER_DSN",
		dateCol:  "DATE",
		stampCol: "DATETIME2(0)",
		quote:    func(s string) string { return "[" + s + "]" },
	},
}

// tableSeq keeps scratch table names unique within one run.
var tableSeq atomic.Int64

// zoneOffsetAt reports time.Local's offset on the given date, which is not
// today's offset wherever the zone observes DST.
func zoneOffsetAt(year int, month time.Month, day int) int {
	_, off := time.Date(year, month, day, 12, 0, 0, 0, time.Local).Zone()
	return off
}

// scratchTable creates a two-column table and drops it when the test ends.
func scratchTable(t *testing.T, e engine, db *sqlx.DB) string {
	t.Helper()
	name := e.quote(fmt.Sprintf("datetime_it_%d_%d",
		time.Now().UnixNano()%1e9, tableSeq.Add(1)))
	ddl := fmt.Sprintf(
		"CREATE TABLE %s (id INT NOT NULL PRIMARY KEY, d %s NULL, "+
			"ts %s NULL)", name, e.dateCol, e.stampCol)
	if _, err := db.Exec(ddl); err != nil {
		t.Fatalf("%v\n%s", err, ddl)
	}
	t.Cleanup(func() { _, _ = db.Exec("DROP TABLE " + name) })
	return name
}

// forEachEngine runs fn once per engine whose DSN is set.
func forEachEngine(t *testing.T, fn func(t *testing.T, e engine,
	db *sqlx.DB)) {
	t.Helper()
	ran := false
	for _, e := range engines {
		dsn := os.Getenv(e.dsnEnv)
		if dsn == "" {
			continue
		}
		ran = true
		t.Run(e.name, func(t *testing.T) {
			db, err := sqlx.Connect(e.driver, dsn)
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			t.Cleanup(func() { _ = db.Close() })
			fn(t, e, db)
		})
	}
	if !ran {
		t.Skip("no DB_TEST_*_DSN variable is set")
	}
}

// read returns the row with the given id, scanned through Scan.
func read(t *testing.T, db *sqlx.DB, table string, id int) row {
	t.Helper()
	var got row
	q, args, err := sqlx.Named(
		"SELECT d, ts FROM "+table+" WHERE id = :id",
		map[string]any{"id": id})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Get(&got, db.Rebind(q), args...); err != nil {
		t.Fatalf("read %d: %v", id, err)
	}
	return got
}

// insert writes one row through Value and returns nothing; reading it back
// is the assertion, so every test goes out and in through this package.
func insert(t *testing.T, db *sqlx.DB, table string, id int,
	d, ts *Datetime) {
	t.Helper()
	q, args, err := sqlx.Named(
		"INSERT INTO "+table+" (id, d, ts) VALUES (:id, :d, :ts)",
		map[string]any{"id": id, "d": d, "ts": ts})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(db.Rebind(q), args...); err != nil {
		t.Fatalf("insert %d: %v", id, err)
	}
}

// The headline claim: a value that arrives carrying UTC must not shift a
// day when it lands in a DATE column.
//
// It is only visible against a server in a zone ahead of or behind UTC. In
// a UTC process the two spellings of the same instant coincide, so the test
// skips rather than passing vacuously — a green run that proves nothing is
// worse than no run.
//
// What a pass proves differs by engine, and the difference is the one
// Value's comment describes. go-sql-driver converts every time.Time to the
// DSN's location itself, so on MySQL this passes with or without Value's
// own conversion to time.Local: removing that conversion changes nothing
// there. go-mssqldb sends the value's own offset, so on
// SQL Server Value's conversion is the only thing standing between this
// input and the wrong day. A MySQL-only run therefore confirms the
// end-to-end agreement, and says nothing about whether Value's conversion
// is still needed; only a SQL Server run can.
func TestIntegrationOffsetBearingInputKeepsItsLocalDate(t *testing.T) {
	_, offset := time.Now().In(time.Local).Zone()
	if offset == 0 {
		t.Skip("time.Local is at UTC, where this cannot go wrong")
	}

	forEachEngine(t, func(t *testing.T, e engine, db *sqlx.DB) {
		table := scratchTable(t, e, db)

		// 2024-03-15T20:00:00Z is legal RFC 3339 and parse keeps it in
		// UTC. In a zone ahead of UTC the same instant is already the
		// 16th locally, and that local date is what every read path in
		// this package will call it.
		var d Datetime
		if err := d.UnmarshalJSON(
			[]byte(`"2024-03-15T20:00:00Z"`)); err != nil {
			t.Fatalf("UnmarshalJSON: %v", err)
		}
		wantLocal := d.Time.In(time.Local)

		insert(t, db, table, 1, &d, &d)
		got := read(t, db, table, 1)

		if got.D == nil || got.TS == nil {
			t.Fatalf("row = %+v, want both columns populated", got)
		}
		if y, m, day := got.D.Time.Date(); y != wantLocal.Year() ||
			m != wantLocal.Month() || day != wantLocal.Day() {
			t.Errorf("DATE came back %04d-%02d-%02d, want %s — the "+
				"instant was stored in the wrong zone",
				y, int(m), day, wantLocal.Format("2006-01-02"))
		}
		if !got.TS.Time.Equal(wantLocal.Truncate(time.Second)) {
			t.Errorf("stamp = %s, want %s",
				got.TS.Time.Format(time.RFC3339),
				wantLocal.Truncate(time.Second).Format(time.RFC3339))
		}
	})
}

// A naive form value means the same wall clock everywhere. This is the
// case that works whatever the drivers do, and it is here so that a
// failure of the test above can be read as a ZONE problem rather than as
// the table being broken.
func TestIntegrationNaiveInputRoundTrips(t *testing.T) {
	forEachEngine(t, func(t *testing.T, e engine, db *sqlx.DB) {
		table := scratchTable(t, e, db)

		var d, ts Datetime
		if err := d.UnmarshalJSON([]byte(`"2024-03-15"`)); err != nil {
			t.Fatalf("UnmarshalJSON date: %v", err)
		}
		if err := ts.UnmarshalJSON(
			[]byte(`"2024-03-15T14:30:00"`)); err != nil {
			t.Fatalf("UnmarshalJSON stamp: %v", err)
		}

		insert(t, db, table, 1, &d, &ts)
		got := read(t, db, table, 1)

		if got.D == nil || got.TS == nil {
			t.Fatalf("row = %+v, want both columns populated", got)
		}
		if want := "2024-03-15"; got.D.Time.Format("2006-01-02") !=
			want {
			t.Errorf("DATE = %s, want %s",
				got.D.Time.Format("2006-01-02"), want)
		}
		if want := "2024-03-15 14:30:00"; got.TS.Time.Format(
			time.DateTime) != want {
			t.Errorf("stamp = %s, want %s",
				got.TS.Time.Format(time.DateTime), want)
		}

		// What a response would carry. MarshalJSON pins the output
		// zone, so this is the same string on both engines.
		out, err := got.TS.MarshalJSON()
		if err != nil {
			t.Fatal(err)
		}
		want := `"` + time.Date(2024, 3, 15, 14, 30, 0, 0,
			time.Local).Format(time.RFC3339) + `"`
		if string(out) != want {
			t.Errorf("MarshalJSON = %s, want %s", out, want)
		}
	})
}

// The emptiness collapse, end to end: a zero Datetime writes SQL NULL and
// a NULL column scans back to a zero Datetime. Both halves are claims in
// doc.go, and only a real column can close the loop.
func TestIntegrationZeroValueRoundTripsThroughNull(t *testing.T) {
	forEachEngine(t, func(t *testing.T, e engine, db *sqlx.DB) {
		table := scratchTable(t, e, db)

		insert(t, db, table, 1, &Datetime{}, &Datetime{})
		got := read(t, db, table, 1)

		// A NULL column scans into a nil *Datetime, which is one of the
		// two routes to null the type documents.
		if got.D != nil && !got.D.Time.IsZero() {
			t.Errorf("DATE = %v, want NULL or the zero value", got.D)
		}
		if got.TS != nil && !got.TS.Time.IsZero() {
			t.Errorf("stamp = %v, want NULL or the zero value", got.TS)
		}

		var count int
		if err := db.Get(&count, "SELECT COUNT(*) FROM "+table+
			" WHERE d IS NULL AND ts IS NULL"); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Errorf("%d rows with both columns NULL, want 1 — the "+
				"zero value was stored as a real date", count)
		}
	})
}

// Scan's []byte and string paths are fallbacks for a driver that hands
// back a stringified time column, which neither driver here does under the
// DSN settings this package assumes. A CAST is what reaches them, so the
// fallbacks are exercised rather than merely believed.
func TestIntegrationScanFromAStringifiedColumn(t *testing.T) {
	forEachEngine(t, func(t *testing.T, e engine, db *sqlx.DB) {
		table := scratchTable(t, e, db)

		var ts Datetime
		if err := ts.UnmarshalJSON(
			[]byte(`"2024-03-15T14:30:00"`)); err != nil {
			t.Fatalf("UnmarshalJSON: %v", err)
		}
		insert(t, db, table, 1, &ts, &ts)

		cast := "CAST(ts AS CHAR(19))"
		if e.name == "sqlserver" {
			cast = "CONVERT(VARCHAR(19), ts, 120)"
		}

		var got Datetime
		if err := db.Get(&got, "SELECT "+cast+" FROM "+table+
			" WHERE id = 1"); err != nil {
			t.Fatalf("scanning %s: %v", cast, err)
		}
		if want := "2024-03-15 14:30:00"; got.Time.Format(
			time.DateTime) != want {
			t.Errorf("scanned %s, want %s",
				got.Time.Format(time.DateTime), want)
		}
		// The SQL text layout resolves in time.Local like every other
		// offsetless shape, which is what keeps the fallback path
		// agreeing with the primary one.
		if _, off := got.Time.Zone(); off !=
			zoneOffsetAt(2024, 3, 15) {
			t.Errorf("scanned value is at offset %ds, want the "+
				"local offset", off)
		}
	})
}
