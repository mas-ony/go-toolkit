//go:build integration

package database

// Integration tests for tx.go. They share the DSN variables, the build tag
// and the helpers of dialect_integration_test.go and run the same way.
//
// tx_test.go covers the control flow against a fake driver, where begin,
// commit and rollback are countable and a panic can be staged on demand.
// What a fake cannot show is whether a rolled-back row is actually GONE,
// which is the whole point of the guard, so every test here asserts on what
// a second connection can see rather than on how tx.go got there.
//
// The isolation-level tests are the other half: doc.go claims the two
// sql.TxOptions extremes are each usable on exactly the engine the other is
// not, and only a real driver can be held to that.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jmoiron/sqlx"
)

// txFixture creates a one-column table for the tests below to write to.
func txFixture(t *testing.T, db *sqlx.DB) string {
	t.Helper()
	return scratchTable(t, db, "tx",
		"CREATE TABLE %s (id INT NOT NULL PRIMARY KEY)")
}

// rowCount reports how many rows table holds, read outside any transaction
// the caller may still be holding open.
func rowCount(t *testing.T, db *sqlx.DB, table string) int {
	t.Helper()
	var n int
	if err := db.Get(&n, "SELECT COUNT(*) FROM "+table); err != nil {
		t.Fatalf("counting %s: %v", table, err)
	}
	return n
}

// insertID inserts one row through whatever ext it is given, so the same
// line serves a transaction and a pool.
func insertID(ctx context.Context, ext sqlx.ExtContext,
	table string, id int) error {
	_, err := sqlx.NamedExecContext(ctx, ext,
		"INSERT INTO "+table+" (id) VALUES (:id)",
		map[string]any{"id": id})
	return err
}

// A committed write is visible from outside the transaction, and a write
// followed by fn's error is not, with that error returned untouched.
func TestIntegrationWithTxCommitsAndRollsBack(t *testing.T) {
	forEachEngine(t, func(t *testing.T, db *sqlx.DB) {
		table := txFixture(t, db)
		ctx := context.Background()

		if err := WithTx(db, func(tx *sqlx.Tx) error {
			return insertID(ctx, tx, table, 1)
		}); err != nil {
			t.Fatalf("committing tx: %v", err)
		}
		if got := rowCount(t, db, table); got != 1 {
			t.Fatalf("after commit: %d rows, want 1", got)
		}

		// fn's error must come back untouched, because callers match
		// their own sentinels against it with errors.Is.
		want := errors.New("no")
		err := WithTx(db, func(tx *sqlx.Tx) error {
			if err := insertID(ctx, tx, table, 2); err != nil {
				return err
			}
			return want
		})
		if !errors.Is(err, want) {
			t.Errorf("err = %v, want %v", err, want)
		}
		if got := rowCount(t, db, table); got != 1 {
			t.Errorf("after rollback: %d rows, want 1 — the second "+
				"insert survived", got)
		}
	})
}

// The panic guard exists so a panicking handler does not abandon a
// checked-out connection with an open transaction. Both halves are
// observable here: the row is gone, and the connection is back in the pool.
func TestIntegrationWithTxRollsBackOnPanic(t *testing.T) {
	forEachEngine(t, func(t *testing.T, db *sqlx.DB) {
		table := txFixture(t, db)
		ctx := context.Background()

		func() {
			defer func() {
				if p := recover(); p == nil {
					t.Error("the panic was swallowed")
				}
			}()
			_ = WithTx(db, func(tx *sqlx.Tx) error {
				if err := insertID(ctx, tx, table, 1); err != nil {
					t.Fatalf("insert: %v", err)
				}
				panic("from fn")
			})
		}()

		if got := rowCount(t, db, table); got != 0 {
			t.Errorf("after a panic: %d rows, want 0", got)
		}
		if got := db.Stats().InUse; got != 0 {
			t.Errorf("InUse = %d after a panic, want 0 — the "+
				"connection was not returned to the pool", got)
		}
	})
}

// The supported alternative to nesting: one *sqlx.Tx passed down to two
// callers that each behave like a repository. Both writes have to share the
// outer transaction's fate, which is what makes a failure in the second one
// undo the first.
func TestIntegrationTxIsPassedDownNotNested(t *testing.T) {
	forEachEngine(t, func(t *testing.T, db *sqlx.DB) {
		table := txFixture(t, db)
		ctx := context.Background()

		repoA := func(ext sqlx.ExtContext) error {
			return insertID(ctx, ext, table, 10)
		}
		repoB := func(ext sqlx.ExtContext) error {
			return insertID(ctx, ext, table, 11)
		}

		if err := WithTx(db, func(tx *sqlx.Tx) error {
			if err := repoA(tx); err != nil {
				return err
			}
			return repoB(tx)
		}); err != nil {
			t.Fatalf("committing both: %v", err)
		}
		if got := rowCount(t, db, table); got != 2 {
			t.Fatalf("after commit: %d rows, want 2", got)
		}

		// A duplicate key in the second call must undo the first.
		err := WithTx(db, func(tx *sqlx.Tx) error {
			if err := insertID(ctx, tx, table, 12); err != nil {
				return err
			}
			return insertID(ctx, tx, table, 10)
		})
		if err == nil {
			t.Fatal("the duplicate key was accepted")
		}
		if got := rowCount(t, db, table); got != 2 {
			t.Errorf("after the failed pair: %d rows, want 2 — the "+
				"first insert of the pair survived", got)
		}
	})
}

// A cancelled context does not merely stop fn: database/sql rolls the
// transaction back on its own, so the work never becomes visible and Commit
// is where the cancellation surfaces.
func TestIntegrationCancelledContextRollsBack(t *testing.T) {
	forEachEngine(t, func(t *testing.T, db *sqlx.DB) {
		table := txFixture(t, db)
		ctx, cancel := context.WithCancel(context.Background())

		err := WithTxContext(ctx, db, nil, func(tx *sqlx.Tx) error {
			if err := insertID(ctx, tx, table, 1); err != nil {
				return err
			}
			cancel()
			return nil
		})
		if err == nil {
			t.Error("Commit succeeded on a cancelled transaction")
		}
		// Either value is correct and which one appears depends on how
		// far database/sql got before Commit was attempted.
		if !errors.Is(err, context.Canceled) &&
			!errors.Is(err, sql.ErrTxDone) {
			t.Errorf("err = %v, want context.Canceled or ErrTxDone",
				err)
		}
		if got := rowCount(t, db, table); got != 0 {
			t.Errorf("after cancellation: %d rows, want 0", got)
		}
	})
}

// A level both drivers accept has to work on both, which is what makes it
// the portable choice doc.go recommends.
func TestIntegrationPortableIsolationLevelsAreAccepted(t *testing.T) {
	forEachEngine(t, func(t *testing.T, db *sqlx.DB) {
		table := txFixture(t, db)
		ctx := context.Background()

		for i, level := range []sql.IsolationLevel{
			sql.LevelDefault,
			sql.LevelReadCommitted,
			sql.LevelRepeatableRead,
			sql.LevelSerializable,
		} {
			id := i + 1
			opts := &sql.TxOptions{Isolation: level}
			if err := WithTxContext(ctx, db, opts,
				func(tx *sqlx.Tx) error {
					return insertID(ctx, tx, table, id)
				}); err != nil {
				t.Errorf("%v: %v", level, err)
			}
		}
		if got := rowCount(t, db, table); got != 4 {
			t.Errorf("%d rows, want 4", got)
		}
	})
}

// The asymmetry doc.go warns about, asserted from the side that is
// deterministic.
//
// ReadOnly is rejected by go-mssqldb before it reads any other option and
// issued as START TRANSACTION READ ONLY by go-sql-driver, so BOTH halves
// hold regardless of server configuration: SQL Server fails at BEGIN, and
// MySQL begins and then refuses the write. That makes this the one place
// the trap is visible rather than argued.
func TestIntegrationReadOnlyIsMySQLOnly(t *testing.T) {
	forEachEngine(t, func(t *testing.T, db *sqlx.DB) {
		table := txFixture(t, db)
		ctx := context.Background()
		opts := &sql.TxOptions{ReadOnly: true}

		if activeDialect() == DialectSQLServer {
			called := false
			err := WithTxContext(ctx, db, opts,
				func(tx *sqlx.Tx) error {
					called = true
					return nil
				})
			if err == nil {
				t.Error("go-mssqldb accepted ReadOnly")
			}
			if called {
				t.Error("fn ran despite a failed BEGIN")
			}
			return
		}

		// MySQL: BEGIN succeeds and the write is what fails, so the
		// error surfaces from fn rather than from tx.go.
		err := WithTxContext(ctx, db, opts, func(tx *sqlx.Tx) error {
			return insertID(ctx, tx, table, 1)
		})
		if err == nil {
			t.Fatal("a read-only transaction accepted an INSERT")
		}
		if got := rowCount(t, db, table); got != 0 {
			t.Errorf("%d rows, want 0", got)
		}

		// Reading inside one is the case ReadOnly exists for.
		if err := WithTxContext(ctx, db, opts,
			func(tx *sqlx.Tx) error {
				var n int
				return tx.GetContext(ctx, &n,
					"SELECT COUNT(*) FROM "+table)
			}); err != nil {
			t.Errorf("a read-only SELECT failed: %v", err)
		}
	})
}

// LevelSnapshot is the mirror image, and only its MySQL half is asserted.
//
// go-sql-driver has no mapping for it and rejects it outright, which is a
// property of the driver and always holds. SQL Server's half is not: the
// driver accepts the level, and whether the TRANSACTION is accepted depends
// on ALLOW_SNAPSHOT_ISOLATION being ON for the scratch database, which this
// test does not control and must not silently require. So the SQL Server
// branch reports what happened and fails nothing.
func TestIntegrationSnapshotIsolationIsSQLServerOnly(t *testing.T) {
	forEachEngine(t, func(t *testing.T, db *sqlx.DB) {
		ctx := context.Background()
		opts := &sql.TxOptions{Isolation: sql.LevelSnapshot}
		err := WithTxContext(ctx, db, opts, func(tx *sqlx.Tx) error {
			var one int
			return tx.GetContext(ctx, &one, "SELECT 1")
		})

		if activeDialect() == DialectMySQL {
			if err == nil {
				t.Error("go-sql-driver accepted LevelSnapshot")
			}
			return
		}
		if err != nil {
			t.Logf("LevelSnapshot failed on this server (%v) — "+
				"expected unless ALLOW_SNAPSHOT_ISOLATION is ON", err)
		}
	})
}

// Every error tx.go returns has to keep a shape callers can act on: a
// begin failure is wrapped with a prefix, and fn's error is not. The begin
// failure is staged with an isolation level the driver cannot express,
// which is the only way to make BEGIN fail against a healthy server.
func TestIntegrationBeginErrorIsWrappedAndFnErrorIsNot(t *testing.T) {
	forEachEngine(t, func(t *testing.T, db *sqlx.DB) {
		ctx := context.Background()
		opts := &sql.TxOptions{Isolation: sql.LevelLinearizable}

		err := WithTxContext(ctx, db, opts, func(tx *sqlx.Tx) error {
			t.Error("fn ran despite a failed BEGIN")
			return nil
		})
		if err == nil {
			t.Fatal("LevelLinearizable was accepted")
		}
		if want := "begin tx: "; !strings.HasPrefix(err.Error(), want) {
			t.Errorf("err = %q, want it to start with %q", err, want)
		}

		table := txFixture(t, db)
		sentinel := fmt.Errorf("repository said no")
		err = WithTxContext(ctx, db, nil, func(tx *sqlx.Tx) error {
			if ierr := insertID(ctx, tx, table, 1); ierr != nil {
				return ierr
			}
			return sentinel
		})
		if !errors.Is(err, sentinel) {
			t.Errorf("err = %v, want %v unwrapped", err, sentinel)
		}
		if err != nil && err.Error() != sentinel.Error() {
			t.Errorf("fn's error was edited to %q", err)
		}
	})
}
