//go:build integration

package database

// Integration tests for db.go. They share the DSN variables and the build
// tag of dialect_integration_test.go and run the same way.
//
// Those DSNs are the only configuration an operator supplies, so this file
// converts each one back into the config pair New takes, using the same
// parser the driver itself uses. Two consequences worth knowing before
// reading a failure here: the conversion is only as faithful as that
// parser, and the encrypt mode it recovers is the one the DSN carries
// rather than one the test chose.
//
// What these tests are FOR is the set of claims in doc.go that no string
// comparison can reach. db_test.go already proves what buildDSN WRITES; an
// engine deciding what that DSN MEANS is a different question, and
// ClientFoundRows is the clearest case of the gap — nothing offline can
// show that an UPDATE changing no column still reports one row matched.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
	"github.com/mas-ony/go-toolkit/config"
	"github.com/microsoft/go-mssqldb/msdsn"
)

// itLocation is the zone the converted AppConfig carries.
//
// Deliberately not UTC, because UTC is the value the timezone test has to
// rule OUT: a driver that ignores the setting and labels everything UTC
// would pass against a UTC configuration and fail against any other. Asia/
// Jakarta is a whole-hour offset with no DST, so the expected offset is the
// same at every instant and the assertion needs no seasonal arithmetic.
const itLocation = "Asia/Jakarta"

// itPoolLimits are small enough that the open limit can be reached, and
// reaching it is the only way to show the limit was applied at all.
const (
	itMaxOpen = 3
	itMaxIdle = 2
)

// encryptFromMySQL inverts the mysql column of encryptModes.
//
// The inverse is lossy in one place and the table is the reason: "true" and
// "strict" both emit tls=true, so a DSN carrying it converts back to "true"
// whichever of the two produced it. That costs the tests nothing — both
// rows verify the certificate chain on MySQL, so the connection behaves
// identically — and it is the same collapse encryptModes documents.
//
// An empty value means the DSN named no tls parameter, which is the
// driver's plaintext default and therefore "disable". Any other value is a
// name registered with mysql.RegisterTLSConfig, which this package never
// emits; converting it to "disable" would quietly downgrade the connection,
// so it is an error instead.
func encryptFromMySQL(tlsConfig string) (string, error) {
	switch tlsConfig {
	case "", "false":
		return "disable", nil
	case "preferred":
		return "false", nil
	case "true":
		return "true", nil
	}
	return "", fmt.Errorf(
		"tls=%q in the DSN is not a mode this package emits", tlsConfig)
}

// encryptFromMSSQL inverts the mssql column of encryptModes. Unlike the
// mysql side this mapping is exact: all four modes survive the round trip.
func encryptFromMSSQL(encryption msdsn.Encryption) (string, error) {
	switch encryption {
	case msdsn.EncryptionDisabled:
		return "disable", nil
	case msdsn.EncryptionOff:
		return "false", nil
	case msdsn.EncryptionRequired:
		return "true", nil
	case msdsn.EncryptionStrict:
		return "strict", nil
	}
	return "", fmt.Errorf("encryption %d is not a mode this package emits",
		encryption)
}

// configFromDSN converts a DSN back into the pair New takes, with the pool
// limits above rather than whatever the DSN implies, since a DSN carries
// none.
func configFromDSN(
	driver, dsn string,
) (*config.DatabaseConfig, *config.AppConfig, error) {
	cfg := &config.DatabaseConfig{
		Driver:          driver,
		MaxOpenConns:    itMaxOpen,
		MaxIdleConns:    itMaxIdle,
		ConnMaxIdleTime: time.Minute,
		ConnMaxLifetime: 5 * time.Minute,
	}
	app := &config.AppConfig{Name: "go-toolkit-it", Location: itLocation}

	switch driver {
	case "mysql":
		my, err := mysql.ParseDSN(dsn)
		if err != nil {
			return nil, nil, err
		}
		// ParseDSN normalises Addr to host:port for tcp, so the split
		// below cannot fail on a DSN this driver accepted.
		host, port, err := net.SplitHostPort(my.Addr)
		if err != nil {
			return nil, nil, err
		}
		n, err := strconv.Atoi(port)
		if err != nil {
			return nil, nil, err
		}
		enc, err := encryptFromMySQL(my.TLSConfig)
		if err != nil {
			return nil, nil, err
		}
		cfg.Host, cfg.Port = host, n
		cfg.Database, cfg.Username, cfg.Password = my.DBName, my.User,
			my.Passwd
		cfg.Encrypt = enc

	case "sqlserver":
		p, err := msdsn.Parse(dsn)
		if err != nil {
			return nil, nil, err
		}
		enc, err := encryptFromMSSQL(p.Encryption)
		if err != nil {
			return nil, nil, err
		}
		cfg.Host, cfg.Port = p.Host, int(p.Port)
		if cfg.Port == 0 {
			cfg.Port = 1433
		}
		cfg.Database, cfg.Username, cfg.Password = p.Database, p.User,
			p.Password
		cfg.Encrypt = enc

	default:
		return nil, nil, fmt.Errorf("no DSN parser for driver %q", driver)
	}
	return cfg, app, nil
}

// forEachEngineConfig is forEachEngine for tests that need to call New
// themselves rather than be handed an open pool. fn receives a config pair
// it may mutate before opening anything.
func forEachEngineConfig(
	t *testing.T,
	fn func(t *testing.T, cfg *config.DatabaseConfig, app *config.AppConfig),
) {
	t.Helper()
	ran := false
	for _, e := range integrationEngines {
		dsn := os.Getenv(e.dsnEnv)
		if dsn == "" {
			continue
		}
		ran = true
		t.Run(e.dialect.String(), func(t *testing.T) {
			cfg, app, err := configFromDSN(e.driver, dsn)
			if err != nil {
				t.Fatalf("converting %s: %v", e.dsnEnv, err)
			}
			withDialect(t, e.dialect)
			fn(t, cfg, app)
		})
	}
	if !ran {
		t.Skip("no DB_TEST_*_DSN variable is set")
	}
}

// mustOpen calls New and closes the pool when the test ends.
func mustOpen(
	t *testing.T,
	cfg *config.DatabaseConfig,
	app *config.AppConfig,
) *sqlx.DB {
	t.Helper()
	db, err := New(cfg, app)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// A pool New opens answers a query, and carries the open limit it was
// given rather than database/sql's unlimited default.
func TestIntegrationNewOpensAPingedPool(t *testing.T) {
	forEachEngineConfig(t, func(
		t *testing.T,
		cfg *config.DatabaseConfig,
		app *config.AppConfig,
	) {
		db := mustOpen(t, cfg, app)

		var one int
		if err := db.Get(&one, "SELECT 1"); err != nil {
			t.Fatalf("SELECT 1: %v", err)
		}
		if one != 1 {
			t.Errorf("SELECT 1 = %d, want 1", one)
		}

		if got := db.Stats().MaxOpenConnections; got != itMaxOpen {
			t.Errorf("MaxOpenConnections = %d, want %d", got, itMaxOpen)
		}
	})
}

// The open limit is only observably APPLIED when it is reached, so this
// holds every permitted connection and then asks for one more under a short
// deadline. The extra request must wait rather than dial, which is what
// distinguishes an applied limit from an ignored one.
func TestIntegrationPoolCapsConcurrentConnections(t *testing.T) {
	forEachEngineConfig(t, func(
		t *testing.T,
		cfg *config.DatabaseConfig,
		app *config.AppConfig,
	) {
		db := mustOpen(t, cfg, app)
		ctx := context.Background()

		for i := 0; i < itMaxOpen; i++ {
			conn, err := db.Connx(ctx)
			if err != nil {
				t.Fatalf("connection %d: %v", i+1, err)
			}
			defer func() { _ = conn.Close() }()
		}
		if got := db.Stats().InUse; got != itMaxOpen {
			t.Fatalf("InUse = %d, want %d", got, itMaxOpen)
		}

		// 250ms is far longer than a checkout from a pool with a free
		// slot and far shorter than dialTimeout, so only a pool that
		// ignored the limit could satisfy this.
		waitCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
		defer cancel()
		if _, err := db.Connx(waitCtx); !errors.Is(
			err, context.DeadlineExceeded) {
			t.Errorf("connection %d: err = %v, want DeadlineExceeded",
				itMaxOpen+1, err)
		}
	})
}

// The row-count agreement doc.go promises, and the one claim in this
// package that cannot be checked without a server on both sides.
//
// An UPDATE that sets a column to the value it already holds reports rows
// MATCHED on SQL Server and, by default, rows CHANGED on MySQL. A
// repository reading RowsAffected() == 0 as "no such id" therefore turns an
// unedited re-save into a 404 on MySQL alone. ClientFoundRows in the DSN is
// what removes the difference, so the assertion is deliberately identical
// for both engines.
func TestIntegrationUpdateReportsRowsMatched(t *testing.T) {
	forEachEngineConfig(t, func(
		t *testing.T,
		cfg *config.DatabaseConfig,
		app *config.AppConfig,
	) {
		db := mustOpen(t, cfg, app)
		table := scratchTable(t, db, "rowcount", onEngine(
			"CREATE TABLE %s (id INT PRIMARY KEY, label VARCHAR(20))",
			"CREATE TABLE %s (id INT PRIMARY KEY, label NVARCHAR(20))",
		))
		mustExec(t, db,
			"INSERT INTO "+table+" (id, label) VALUES (1, 'same')")

		for _, c := range []struct {
			name, stmt string
			want       int64
		}{
			{
				name: "unchanged row still matches",
				stmt: "UPDATE " + table +
					" SET label = 'same' WHERE id = 1",
				want: 1,
			},
			{
				name: "changed row matches",
				stmt: "UPDATE " + table +
					" SET label = 'other' WHERE id = 1",
				want: 1,
			},
			{
				name: "absent row matches nothing",
				stmt: "UPDATE " + table +
					" SET label = 'x' WHERE id = 99",
				want: 0,
			},
		} {
			t.Run(c.name, func(t *testing.T) {
				res, err := db.Exec(c.stmt)
				if err != nil {
					t.Fatalf("%v\n%s", err, c.stmt)
				}
				got, err := res.RowsAffected()
				if err != nil {
					t.Fatal(err)
				}
				if got != c.want {
					t.Errorf("RowsAffected = %d, want %d", got, c.want)
				}
			})
		}
	})
}

// The time agreement: an offsetless column read back through a pool New
// opened carries app.Location rather than UTC.
//
// The assertion is on the OFFSET rather than the zone name, because the two
// drivers reach it by different routes — go-mssqldb from the timezone= DSN
// key, go-sql-driver from parseTime with loc — and only the offset is what
// a mislabelled value gets wrong. A whole-hour zone is what makes the
// expected offset a constant.
func TestIntegrationOffsetlessColumnsCarryAppLocation(t *testing.T) {
	forEachEngineConfig(t, func(
		t *testing.T,
		cfg *config.DatabaseConfig,
		app *config.AppConfig,
	) {
		loc, err := time.LoadLocation(app.Location)
		if err != nil {
			t.Skipf("no tzdata for %s: %v", app.Location, err)
		}
		db := mustOpen(t, cfg, app)
		table := scratchTable(t, db, "zone", onEngine(
			"CREATE TABLE %s (stamp DATETIME(0) NOT NULL)",
			"CREATE TABLE %s (stamp DATETIME2(0) NOT NULL)",
		))
		mustExec(t, db, "INSERT INTO "+table+
			" (stamp) VALUES ("+NowExpr()+")")

		var got time.Time
		if err := db.Get(&got, "SELECT stamp FROM "+table); err != nil {
			t.Fatalf("reading stamp: %v", err)
		}

		// The driver labels the wall clock the server sent, so the
		// expected offset is the one loc applies to that same wall clock.
		wall := time.Date(got.Year(), got.Month(), got.Day(),
			got.Hour(), got.Minute(), got.Second(), 0, loc)
		_, want := wall.Zone()
		_, have := got.Zone()
		if have != want {
			t.Errorf("stamp %s has offset %ds, want %ds (%s)",
				got, have, want, app.Location)
		}
		if have == 0 {
			t.Errorf("stamp %s came back at offset 0 — the timezone "+
				"setting did not reach the driver", got)
		}
	})
}

// doc.go claims a successful New proves nothing about a namespace a query
// qualifies itself with, because the ping never exercises one. A catalog
// that certainly does not exist is the cheapest way to hold that claim in
// place: New must succeed and the query must be the thing that fails.
//
// Qualify is called as a METHOD here rather than through the selected
// dialect, so the test also covers the form that reads no process-wide
// state.
func TestIntegrationPingProvesNothingAboutAQualifiedNamespace(t *testing.T) {
	forEachEngineConfig(t, func(
		t *testing.T,
		cfg *config.DatabaseConfig,
		app *config.AppConfig,
	) {
		db := mustOpen(t, cfg, app)

		d := activeDialect()
		absent := fmt.Sprintf("no_such_catalog_%d",
			time.Now().UnixNano()%1e9)
		if d == DialectSQLServer {
			absent += ".dbo"
		}
		query := "SELECT 1 FROM " + d.Qualify(absent, "probe")

		if err := db.Get(new(int), query); err == nil {
			t.Errorf("querying %s succeeded, want an error", absent)
		}
	})
}

// A cancelled context ends the wait at once instead of spending
// pingTimeout, which is the one thing NewContext offers that New cannot.
func TestIntegrationNewContextStopsOnACancelledContext(t *testing.T) {
	forEachEngineConfig(t, func(
		t *testing.T,
		cfg *config.DatabaseConfig,
		app *config.AppConfig,
	) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		start := time.Now()
		db, err := NewContext(ctx, cfg, app)
		elapsed := time.Since(start)
		if db != nil {
			_ = db.Close()
			t.Error("NewContext returned a pool, want nil")
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
		if elapsed >= pingTimeout {
			t.Errorf("took %s, want well under pingTimeout (%s)",
				elapsed, pingTimeout)
		}
	})
}

// A wrong password must fail, and the failure must be safe to log. The
// second half is the point: a ping error WRAPS its cause, unlike the Open
// error beside it, so this is where a driver bump could start quoting a
// credential into something a caller hands straight to a fatal log call.
func TestIntegrationNewRejectsBadCredentialsWithoutLeakingThem(t *testing.T) {
	const wrong = "Tr0ub4dor&3-not-the-real-password"

	forEachEngineConfig(t, func(
		t *testing.T,
		cfg *config.DatabaseConfig,
		app *config.AppConfig,
	) {
		if cfg.Password == "" {
			t.Skip("the DSN carries no password to get wrong")
		}
		real := cfg.Password
		cfg.Password = wrong

		db, err := New(cfg, app)
		if db != nil {
			_ = db.Close()
			t.Error("New returned a pool, want nil")
		}
		if err == nil {
			t.Fatal("New accepted a wrong password")
		}
		msg := err.Error()
		for _, secret := range []struct{ name, value string }{
			{"the wrong password", wrong},
			{"the real password", real},
		} {
			if secret.value != "" &&
				strings.Contains(msg, secret.value) {
				t.Errorf("the error quotes %s: %s", secret.name, msg)
			}
		}
	})
}

// pingTimeout exists for a host that DROPS packets rather than refusing
// them, where the alternative is the OS TCP timeout — minutes of silence
// before the process dies. 192.0.2.1 is TEST-NET-1 (RFC 5737), reserved for
// documentation and routed nowhere.
//
// This test needs no server, so it runs whether or not a DSN is set; the
// build tag is what keeps its five seconds out of a normal `go test`.
//
// The assertion is on the ELAPSED time rather than on the error, because a
// network that answers TEST-NET-1 with an ICMP unreachable produces a
// refusal instead of a deadline, and that is not a failure of the constant.
// What would be a failure is either path taking longer than the deadline
// this package sets.
func TestIntegrationUnreachableHostFailsWithinPingTimeout(t *testing.T) {
	for _, e := range integrationEngines {
		t.Run(e.dialect.String(), func(t *testing.T) {
			cfg := &config.DatabaseConfig{
				Driver:   e.driver,
				Host:     "192.0.2.1",
				Port:     1433,
				Database: "scratch",
				Username: "probe",
				Password: "probe",
				Encrypt:  "disable",
			}
			app := &config.AppConfig{Name: "go-toolkit-it",
				Location: "UTC"}

			start := time.Now()
			db, err := New(cfg, app)
			elapsed := time.Since(start)
			if db != nil {
				_ = db.Close()
				t.Error("New returned a pool, want nil")
			}
			if err == nil {
				t.Fatal("New reached 192.0.2.1")
			}
			// The slack covers the ping deadline firing and the pool
			// being closed behind it, not another dial.
			if limit := pingTimeout + 3*time.Second; elapsed > limit {
				t.Errorf("took %s, want under %s — the ping "+
					"deadline did not bound the wait", elapsed, limit)
			}
			if errors.Is(err, context.DeadlineExceeded) &&
				!strings.Contains(err.Error(), "ping timed out") {
				t.Errorf("a deadline was reported as %q, want the "+
					"unreachable-host message", err)
			}
		})
	}
}
