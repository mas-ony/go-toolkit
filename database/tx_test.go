package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
)

// ----------------------------------------------------------------------------
// Test helpers
// ----------------------------------------------------------------------------

// counts is the observable behaviour of one transaction, as the driver saw it.
type counts struct {
	connects  int
	begins    int
	commits   int
	rollbacks int
}

// errReadOnlyUnsupported reproduces go-mssqldb's own message so the assertion
// in TestWithTxContextSurfacesReadOnlyRejection is recognisably the real one.
var errReadOnlyUnsupported = errors.New(
	"read-only transactions are not supported")

// fakeDB is the shared state behind the connector, connections and
// transactions below. Every field is guarded by mu because database/sql calls
// Rollback from its own awaitDone goroutine, concurrently with the test.
type fakeDB struct {
	mu        sync.Mutex
	connects  int
	begins    int
	commits   int
	rollbacks int
	opts      []driver.TxOptions
	ctx       context.Context

	// Injected failures, set before open and not mutated afterwards.
	beginErr       error
	commitErr      error
	rollbackErr    error
	rejectReadOnly bool

	// rolledBack carries one token per driver-level rollback, so a test can
	// wait for the asynchronous one instead of sleeping.
	rolledBack chan struct{}
}

func newFakeDB() *fakeDB {
	return &fakeDB{rolledBack: make(chan struct{}, 8)}
}

// open returns a *sqlx.DB backed by this fake.
//
// sql.OpenDB with a custom connector avoids sql.Register, which is a
// process-global map: registering per test would either collide or force a
// unique name scheme, and both are worse than passing the connector directly.
func (f *fakeDB) open(t *testing.T) *sqlx.DB {
	t.Helper()
	db := sqlx.NewDb(sql.OpenDB(&fakeConnector{db: f}), "fake")
	t.Cleanup(func() { _ = db.Close() })
	return db
}

type snapshotResult struct {
	counts
	opts []driver.TxOptions
	ctx  context.Context
}

func (f *fakeDB) snapshot() snapshotResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	return snapshotResult{
		counts: counts{
			connects:  f.connects,
			begins:    f.begins,
			commits:   f.commits,
			rollbacks: f.rollbacks,
		},
		opts: append([]driver.TxOptions(nil), f.opts...),
		ctx:  f.ctx,
	}
}

// assert compares the three transaction-lifecycle counters, ignoring connects
// (which only the nesting tests care about).
func (f *fakeDB) assert(t *testing.T, want counts) {
	t.Helper()
	got := f.snapshot().counts
	if got.begins != want.begins {
		t.Errorf("BEGINs: got %d, want %d", got.begins, want.begins)
	}
	if got.commits != want.commits {
		t.Errorf("COMMITs: got %d, want %d", got.commits, want.commits)
	}
	if got.rollbacks != want.rollbacks {
		t.Errorf("ROLLBACKs: got %d, want %d", got.rollbacks, want.rollbacks)
	}
}

// waitForRollback blocks until the driver sees a rollback, or the timeout
// expires. It reports whether one arrived.
func (f *fakeDB) waitForRollback(timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-f.rolledBack:
		return true
	case <-timer.C:
		return false
	}
}

type fakeConnector struct{ db *fakeDB }

func (c *fakeConnector) Connect(context.Context) (driver.Conn, error) {
	c.db.mu.Lock()
	c.db.connects++
	c.db.mu.Unlock()
	return &fakeConn{db: c.db}, nil
}

func (c *fakeConnector) Driver() driver.Driver { return fakeDriver{} }

type fakeDriver struct{}

func (fakeDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New(
		"fake: this driver is only reachable through its connector")
}

// fakeConn implements driver.ConnBeginTx as well as driver.Conn. That is the
// point: database/sql only forwards the context and the isolation options to a
// driver that implements the Tx variant, so a conn with a bare Begin would
// make half the assertions in this file untestable — and, on a real driver,
// would silently drop the very options WithTxContext exists to pass.
type fakeConn struct{ db *fakeDB }

func (c *fakeConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("fake: statements are not supported")
}

func (c *fakeConn) Close() error { return nil }

func (c *fakeConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *fakeConn) BeginTx(
	ctx context.Context,
	opts driver.TxOptions,
) (driver.Tx, error) {
	f := c.db
	f.mu.Lock()
	defer f.mu.Unlock()

	f.opts = append(f.opts, opts)
	f.ctx = ctx

	// Mirrors go-mssqldb, which refuses the flag before reading any other
	// option — including before the isolation level, so a call that is wrong
	// in both ways reports this one. (Its only earlier check is that the
	// connection is still good, which no option can influence.)
	if opts.ReadOnly && f.rejectReadOnly {
		return nil, errReadOnlyUnsupported
	}
	if f.beginErr != nil {
		return nil, f.beginErr
	}
	f.begins++
	return &fakeTx{db: f}, nil
}

type fakeTx struct{ db *fakeDB }

func (t *fakeTx) Commit() error {
	f := t.db
	f.mu.Lock()
	f.commits++
	err := f.commitErr
	f.mu.Unlock()
	return err
}

func (t *fakeTx) Rollback() error {
	f := t.db
	f.mu.Lock()
	f.rollbacks++
	err := f.rollbackErr
	f.mu.Unlock()

	// Non-blocking so a test that never waits cannot wedge the awaitDone
	// goroutine on a full channel.
	select {
	case f.rolledBack <- struct{}{}:
	default:
	}
	return err
}

// capturePanic runs fn and returns whatever it panicked with, or nil.
func capturePanic(fn func()) (recovered any) {
	defer func() { recovered = recover() }()
	fn()
	return nil
}

// assertConnectionsReturned is the leak check that motivates the panic guard.
//
// It polls rather than reading once, because database/sql returns a connection
// to the pool from the goroutine that rolled the transaction back, which is
// not necessarily the test's goroutine.
func assertConnectionsReturned(t *testing.T, db *sqlx.DB) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for {
		if inUse := db.Stats().InUse; inUse == 0 {
			return
		} else if time.Now().After(deadline) {
			t.Errorf("%d connection(s) still checked out; the pool would "+
				"drain under repeated failures", inUse)
			return
		}
		time.Sleep(time.Millisecond)
	}
}

// These tests run against a fake driver defined at the bottom of this file
// rather than against a real SQL Server, for two reasons.
//
// First, everything WithTxContext actually does is decide WHEN database/sql is
// told to begin, commit and roll back. A fake conn records exactly those
// calls, which is the behaviour under test; a real database would add a
// network, a schema and a container to observe the same three method calls.
//
// Second, several of the guarantees in the doc comment are properties of
// database/sql itself — the async rollback on context cancellation, the
// ErrTxDone no-op after a failed commit, the second connection checked out by
// a nested call. Those are observable at the DRIVER seam and nowhere else. A
// test against a live server would see the outcome without being able to
// attribute it.
//
// The one thing the fake cannot show is the self-deadlock that nesting causes
// on a real server, because the fake holds no row locks.
// TestNestedCallChecksOutASecondConnection pins the mechanism that leads to it
// instead, which is the part this package controls.

// ----------------------------------------------------------------------------
// Success and failure paths
// ----------------------------------------------------------------------------

// TestWithTxCommitsOnSuccess covers the happy path, including the negative
// half: Rollback must not be called at all when Commit succeeds.
//
// A conditional rollback that fired unconditionally would still return the
// right error to callers — the second Rollback is a harmless ErrTxDone — so
// the call count is the only thing that distinguishes correct from merely
// working.
func TestWithTxCommitsOnSuccess(t *testing.T) {
	t.Parallel()

	f := newFakeDB()
	db := f.open(t)

	called := false
	err := WithTx(db, func(tx *sqlx.Tx) error {
		called = true
		if tx == nil {
			t.Error("fn received a nil *sqlx.Tx")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WithTx: %v", err)
	}
	if !called {
		t.Fatal("fn was never called")
	}

	f.assert(t, counts{begins: 1, commits: 1, rollbacks: 0})
	assertConnectionsReturned(t, db)
}

// TestWithTxRollsBackAndReturnsFnError checks the ordinary failure path: fn's
// error reaches the caller unchanged (errors.Is identity, not a wrapped
// lookalike) and Commit is never attempted.
func TestWithTxRollsBackAndReturnsFnError(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("repository: constraint violated")

	f := newFakeDB()
	db := f.open(t)

	err := WithTx(db, func(*sqlx.Tx) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("got %v, want the error fn returned", err)
	}

	f.assert(t, counts{begins: 1, commits: 0, rollbacks: 1})
	assertConnectionsReturned(t, db)
}

// TestWithTxWrapsBeginError checks the first return in the function.
//
// The wrap must preserve the cause — a failed BEGIN is almost always a pool or
// connectivity problem, and errors.Is on the driver's sentinel is how a caller
// tells that apart from a business failure. The prefix is asserted too, since
// it is the only thing in the message that says which of the three round trips
// failed.
func TestWithTxWrapsBeginError(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("driver: no connection available")

	f := newFakeDB()
	f.beginErr = sentinel
	db := f.open(t)

	fnRan := false
	err := WithTx(db, func(*sqlx.Tx) error {
		fnRan = true
		return nil
	})
	if err == nil {
		t.Fatal("WithTx succeeded despite a failing BEGIN")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("got %v, want an error wrapping the driver's cause", err)
	}
	if got, want := err.Error(), "begin tx: "; !strings.HasPrefix(got, want) {
		t.Errorf("message %q does not start with %q", got, want)
	}
	if fnRan {
		t.Error("fn ran even though no transaction was started")
	}

	// Nothing was started, so there is nothing to clean up — and in particular
	// no rollback against a transaction that does not exist.
	f.assert(t, counts{begins: 0, commits: 0, rollbacks: 0})
	assertConnectionsReturned(t, db)
}

// TestWithTxReturnsCommitError covers the last line of the function and the
// subtlest claim in its doc comment: after a failed Commit, the deferred
// Rollback runs but reaches the driver ZERO times.
//
// database/sql marks the Tx done the moment Commit returns, success or
// failure, so tx.Rollback() short-circuits to sql.ErrTxDone before touching
// the connection. The rollback count is the assertion that proves it — without
// it, "harmless no-op" is a claim about code nobody has watched execute.
func TestWithTxReturnsCommitError(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("driver: commit failed")

	f := newFakeDB()
	f.commitErr = sentinel
	db := f.open(t)

	err := WithTx(db, func(*sqlx.Tx) error { return nil })
	if !errors.Is(err, sentinel) {
		t.Fatalf("got %v, want the commit error", err)
	}

	f.assert(t, counts{begins: 1, commits: 1, rollbacks: 0})
	assertConnectionsReturned(t, db)
}

// TestWithTxRollsBackAndRepanics is the guard that keeps a panicking handler
// from leaking a connection.
//
// Both halves matter. The rollback returns the checked-out connection to the
// pool, without which repeated panics exhaust MaxOpenConns and the service
// stops serving healthy requests too. The re-panic keeps the failure visible:
// a transaction helper that swallowed a panic would convert a crash into a
// silently missing write.
//
// The panic value is compared by identity so that a helper "improving" it into
// a wrapped error fails here.
func TestWithTxRollsBackAndRepanics(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value any
	}{
		{"error value", errors.New("handler exploded")},
		{"string value", "something went very wrong"},
		{"struct value", struct{ Code int }{Code: 42}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newFakeDB()
			db := f.open(t)

			got := capturePanic(func() {
				_ = WithTx(db, func(*sqlx.Tx) error { panic(tc.value) })
			})
			if got != tc.value {
				t.Fatalf(
					"recovered %#v, want the original panic value %#v",
					got,
					tc.value)
			}

			f.assert(t, counts{begins: 1, commits: 0, rollbacks: 1})
			assertConnectionsReturned(t, db)
		})
	}
}

// TestWithTxHandlesRuntimePanics checks the same guard against a panic nobody
// wrote deliberately, since that is the realistic case: a nil map write or a
// slice bounds error inside a repository call.
func TestWithTxHandlesRuntimePanics(t *testing.T) {
	t.Parallel()

	f := newFakeDB()
	db := f.open(t)

	got := capturePanic(func() {
		_ = WithTx(db, func(*sqlx.Tx) error {
			var m map[string]int
			m["boom"] = 1 // assignment to entry in nil map
			return nil
		})
	})
	if got == nil {
		t.Fatal("the runtime panic did not propagate")
	}
	if _, ok := got.(error); !ok {
		t.Errorf("recovered %T, want a runtime error", got)
	}

	f.assert(t, counts{begins: 1, commits: 0, rollbacks: 1})
	assertConnectionsReturned(t, db)
}

// TestRollbackErrorsAreDiscardedNotReturned covers the two `_ = tx.Rollback()`
// lines, which the doc comment justifies twice and which nothing observed.
//
// The rule is that the ORIGINAL failure is the actionable one. A rollback that
// also fails is a symptom of the same broken connection, and returning it
// would replace "the constraint you violated" with "the network went away" —
// the caller would then log the wrong cause and, worse, could no longer match
// fn's own sentinel with errors.Is.
//
// Both entry conditions are covered: an ordinary fn error, and a panic, where
// swallowing the panic in favour of a rollback error would be the more serious
// of the two mistakes.
func TestRollbackErrorsAreDiscardedNotReturned(t *testing.T) {
	t.Parallel()

	rollbackFailure := errors.New("driver: connection reset during rollback")

	t.Run("fn error survives a failing rollback", func(t *testing.T) {
		t.Parallel()

		fnFailure := errors.New("repository: constraint violated")

		f := newFakeDB()
		f.rollbackErr = rollbackFailure
		db := f.open(t)

		err := WithTx(db, func(*sqlx.Tx) error { return fnFailure })
		if !errors.Is(err, fnFailure) {
			t.Fatalf("got %v, want fn's error", err)
		}
		if errors.Is(err, rollbackFailure) {
			t.Error("the rollback error reached the caller and displaced " +
				"the real cause")
		}

		f.assert(t, counts{begins: 1, commits: 0, rollbacks: 1})
		assertConnectionsReturned(t, db)
	})

	t.Run("panic survives a failing rollback", func(t *testing.T) {
		t.Parallel()

		panicValue := errors.New("handler exploded")

		f := newFakeDB()
		f.rollbackErr = rollbackFailure
		db := f.open(t)

		got := capturePanic(func() {
			_ = WithTx(db, func(*sqlx.Tx) error { panic(panicValue) })
		})
		if got != panicValue {
			t.Fatalf("recovered %#v, want the original panic value %#v",
				got,
				panicValue)
		}

		f.assert(t, counts{begins: 1, commits: 0, rollbacks: 1})
		assertConnectionsReturned(t, db)
	})
}

// TestNamedReturnRoutesEveryPathThroughOneRollback is the assertion behind the
// long comment on the `tx, err := db.BeginTxx(...)` line.
//
// If that `:=` ever shadows the named return — by being moved inside a nested
// block — the deferred guard reads an err that stays nil forever and NO
// failure path rolls back. The compiler says nothing. This table is what says
// something: every non-panic failure mode must produce exactly one rollback.
func TestNamedReturnRoutesEveryPathThroughOneRollback(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		commitErr error
		fnErr     error
		rollbacks int
	}{
		{"fn error", nil, errors.New("fn"), 1},

		// Tx already done; see TestWithTxReturnsCommitError.
		{"commit error", errors.New("commit"), nil, 0},

		{"success", nil, nil, 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newFakeDB()
			f.commitErr = tc.commitErr
			db := f.open(t)

			err := WithTx(db, func(*sqlx.Tx) error { return tc.fnErr })
			wantErr := tc.fnErr != nil || tc.commitErr != nil
			if (err != nil) != wantErr {
				t.Fatalf("error: got %v, want error=%t", err, wantErr)
			}
			if got := f.snapshot().rollbacks; got != tc.rollbacks {
				t.Errorf(
					"driver rollbacks: got %d, want %d",
					got,
					tc.rollbacks)
			}
			assertConnectionsReturned(t, db)
		})
	}
}

// ----------------------------------------------------------------------------
// Context and options
// ----------------------------------------------------------------------------

// TestWithTxUsesBackgroundContextAndNoOptions pins what the shortcut form
// delegates.
//
// The context assertion works because context.Background().Done() is nil — a
// context that can never be cancelled. That is the exact property the doc
// comment claims (nothing can cancel a WithTx transaction early), and it is
// invisible from the caller's side.
//
// The options assertion is that database/sql turns a nil *sql.TxOptions into a
// zero driver.TxOptions, which go-mssqldb reads as "leave the session's
// isolation level alone" — READ COMMITTED on SQL Server.
func TestWithTxUsesBackgroundContextAndNoOptions(t *testing.T) {
	t.Parallel()

	f := newFakeDB()
	db := f.open(t)

	if err := WithTx(db, func(*sqlx.Tx) error { return nil }); err != nil {
		t.Fatalf("WithTx: %v", err)
	}

	s := f.snapshot()
	if len(s.opts) != 1 {
		t.Fatalf("BEGIN was issued %d times, want 1", len(s.opts))
	}
	wantDefault := driver.IsolationLevel(sql.LevelDefault)
	if got := s.opts[0].Isolation; got != wantDefault {
		t.Errorf("isolation: got %v, want LevelDefault — nil opts must not "+
			"change the session", got)
	}
	if s.opts[0].ReadOnly {
		t.Error("ReadOnly: got true, want false")
	}
	if s.ctx == nil {
		t.Fatal("the driver never saw a context")
	}
	if s.ctx.Done() != nil {
		t.Error("the transaction was started with a cancellable context; " +
			"WithTx must pass context.Background()")
	}
}

// TestWithTxContextPassesIsolationLevelThrough checks that opts reach the
// driver untouched, for the levels go-mssqldb accepts.
//
// The levels are asserted by value rather than by name because database/sql
// converts sql.IsolationLevel to driver.IsolationLevel by a plain numeric
// cast: a reordering of either enum would silently map LevelSerializable onto
// something weaker, and a downgrade that stays invisible is the failure mode
// the doc comment singles out as worth preventing.
func TestWithTxContextPassesIsolationLevelThrough(t *testing.T) {
	t.Parallel()

	levels := []sql.IsolationLevel{
		sql.LevelDefault,
		sql.LevelReadUncommitted,
		sql.LevelReadCommitted,
		sql.LevelRepeatableRead,
		sql.LevelSnapshot,
		sql.LevelSerializable,
	}

	for _, level := range levels {
		t.Run(level.String(), func(t *testing.T) {
			t.Parallel()

			f := newFakeDB()
			db := f.open(t)

			opts := &sql.TxOptions{Isolation: level}
			err := WithTxContext(
				context.Background(), db, opts,
				func(*sqlx.Tx) error { return nil })
			if err != nil {
				t.Fatalf("WithTxContext: %v", err)
			}

			s := f.snapshot()
			if len(s.opts) != 1 {
				t.Fatalf("BEGIN was issued %d times, want 1", len(s.opts))
			}
			got, want := s.opts[0].Isolation, driver.IsolationLevel(level)
			if got != want {
				t.Errorf("isolation: got %v, want %v", got, want)
			}
		})
	}
}

// TestWithTxContextSurfacesReadOnlyRejection backs the "ReadOnly is NOT usable
// on this driver" paragraph.
//
// The rejection itself lives in go-mssqldb, which this package does not
// import, so the fake reproduces the driver's own first-line check:
//
//	if opts.ReadOnly {
//	    return nil, errors.New("read-only transactions are not supported")
//	}
//
// What is under test here is that WithTxContext does not absorb or retry that
// error but returns it wrapped, so a caller reaching for ReadOnly as a cheap
// optimisation fails loudly at BEGIN rather than getting a silent read-write
// transaction.
func TestWithTxContextSurfacesReadOnlyRejection(t *testing.T) {
	t.Parallel()

	f := newFakeDB()
	f.rejectReadOnly = true
	db := f.open(t)

	fnRan := false
	err := WithTxContext(
		context.Background(), db, &sql.TxOptions{ReadOnly: true},
		func(*sqlx.Tx) error {
			fnRan = true
			return nil
		})
	if err == nil {
		t.Fatal("a read-only transaction was accepted; the driver rejects it")
	}
	if !errors.Is(err, errReadOnlyUnsupported) {
		t.Errorf("got %v, want the driver's rejection", err)
	}
	if fnRan {
		t.Error("fn ran despite BEGIN failing")
	}

	s := f.snapshot()
	if len(s.opts) != 1 || !s.opts[0].ReadOnly {
		t.Errorf("the ReadOnly flag did not reach the driver: %+v", s.opts)
	}
}

// TestCancelledContextRollsBackWithoutWaitingForFn is the test for the claim
// that separates WithTxContext from WithTx.
//
// The point is not that the transaction eventually ends — every path does
// that. It is that database/sql's own goroutine rolls back AT CANCELLATION,
// while fn is still running, so an abandoned HTTP request releases its row
// locks immediately instead of holding them until fn happens to notice.
//
// fn blocks on the rollback signal, which makes the ordering observable rather
// than a race: if the rollback did not happen during fn, the wait times out
// and the test fails with that fact instead of hanging.
func TestCancelledContextRollsBackWithoutWaitingForFn(t *testing.T) {
	t.Parallel()

	f := newFakeDB()
	db := f.open(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rolledBackDuringFn := false
	err := WithTxContext(ctx, db, nil, func(*sqlx.Tx) error {
		cancel()
		rolledBackDuringFn = f.waitForRollback(2 * time.Second)
		return nil
	})

	if !rolledBackDuringFn {
		t.Error("no rollback reached the driver while fn was still running; " +
			"cancellation is not releasing locks early")
	}
	if err == nil {
		t.Fatal("WithTxContext returned nil after its context was cancelled")
	}
	// Which of the two arrives depends on how far database/sql got before
	// Commit was attempted; both mean the same thing to a caller, and neither
	// requires fn to special-case cancellation.
	if !errors.Is(err, context.Canceled) && !errors.Is(err, sql.ErrTxDone) {
		t.Errorf("got %v, want context.Canceled or sql.ErrTxDone", err)
	}

	// Exactly one rollback: database/sql's own, with the deferred guard in
	// WithTxContext short-circuiting on an already-done Tx.
	if got := f.snapshot().rollbacks; got != 1 {
		t.Errorf("driver rollbacks: got %d, want 1", got)
	}
	assertConnectionsReturned(t, db)
}

// TestAlreadyCancelledContextFailsAtBegin covers the degenerate case, where
// the request was abandoned before the transaction started. No BEGIN should
// reach the driver at all.
func TestAlreadyCancelledContextFailsAtBegin(t *testing.T) {
	t.Parallel()

	f := newFakeDB()
	db := f.open(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	fnRan := false
	err := WithTxContext(ctx, db, nil, func(*sqlx.Tx) error {
		fnRan = true
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("got %v, want context.Canceled", err)
	}
	if fnRan {
		t.Error("fn ran with an already-cancelled context")
	}
	f.assert(t, counts{begins: 0, commits: 0, rollbacks: 0})
}

// ----------------------------------------------------------------------------
// Nesting
// ----------------------------------------------------------------------------

// TestNestedCallChecksOutASecondConnection pins the mechanism behind the
// "Still no nesting" section.
//
// On a real server the second connection blocks on the row locks the outer
// transaction holds, and the request hangs until the lock timeout — a
// self-deadlock, not an error, which is what makes it so expensive to
// diagnose. The fake holds no locks, so what is observable here is the cause
// rather than the symptom: a nested call opens a SECOND connection instead of
// joining the first, because database/sql has no savepoints.
//
// If a future version ever did make nesting work, this test fails and the
// whole section of the doc comment needs rewriting — which is the correct
// outcome.
func TestNestedCallChecksOutASecondConnection(t *testing.T) {
	t.Parallel()

	f := newFakeDB()
	db := f.open(t)

	err := WithTx(db, func(*sqlx.Tx) error {
		return WithTx(db, func(*sqlx.Tx) error { return nil })
	})
	if err != nil {
		t.Fatalf("WithTx: %v", err)
	}

	s := f.snapshot()
	if s.connects != 2 {
		t.Errorf("connections opened: got %d, want 2 — a nested call must "+
			"not join the outer transaction", s.connects)
	}
	if s.begins != 2 {
		t.Errorf("BEGINs: got %d, want 2", s.begins)
	}
	assertConnectionsReturned(t, db)
}

// TestPassingTheTxDownIsTheSupportedPattern is the positive counterpart: the
// documented alternative uses one connection, which is what makes it safe.
func TestPassingTheTxDownIsTheSupportedPattern(t *testing.T) {
	t.Parallel()

	f := newFakeDB()
	db := f.open(t)

	// Stands in for a repository's own WithTx(tx) method.
	step := func(tx *sqlx.Tx) error {
		if tx == nil {
			return errors.New("no transaction was passed down")
		}
		return nil
	}

	err := WithTx(db, func(tx *sqlx.Tx) error {
		if err := step(tx); err != nil {
			return err
		}
		return step(tx)
	})
	if err != nil {
		t.Fatalf("WithTx: %v", err)
	}

	s := f.snapshot()
	if s.connects != 1 {
		t.Errorf("connections opened: got %d, want 1", s.connects)
	}
	f.assert(t, counts{begins: 1, commits: 1, rollbacks: 0})
}

// TestFnReceivesAConcreteTxNotAnInterface guards the signature choice.
//
// fn takes *sqlx.Tx rather than sqlx.ExtContext specifically so repositories
// can call their own .WithTx(tx) method. Narrowing the parameter to an
// interface would compile everywhere it is used today and break that pattern
// for the next repository added.
func TestFnReceivesAConcreteTxNotAnInterface(t *testing.T) {
	t.Parallel()

	f := newFakeDB()
	db := f.open(t)

	err := WithTx(db, func(tx *sqlx.Tx) error {
		// The assertion is the assignment itself: it only compiles while the
		// parameter is the concrete type a repository's WithTx method expects.
		var concrete *sqlx.Tx = tx
		if concrete == nil {
			t.Error("fn received a nil transaction")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WithTx: %v", err)
	}
}

// TestWithTxDelegatesToWithTxContext checks the two entry points agree on
// everything a caller can observe, so the shortcut cannot drift into its own
// half-maintained copy of the logic.
func TestWithTxDelegatesToWithTxContext(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("fn failed")

	direct := newFakeDB()
	directDB := direct.open(t)
	directErr := WithTxContext(
		context.Background(), directDB, nil,
		func(*sqlx.Tx) error { return sentinel })

	shortcut := newFakeDB()
	shortcutDB := shortcut.open(t)
	shortcutErr := WithTx(
		shortcutDB, func(*sqlx.Tx) error { return sentinel })

	if !errors.Is(directErr, sentinel) || !errors.Is(shortcutErr, sentinel) {
		t.Fatalf("errors differ: direct=%v shortcut=%v",
			directErr, shortcutErr)
	}

	d, s := direct.snapshot(), shortcut.snapshot()
	if d.begins != s.begins ||
		d.commits != s.commits ||
		d.rollbacks != s.rollbacks {
		t.Errorf("driver calls differ: WithTxContext=%+v WithTx=%+v", d, s)
	}
}
