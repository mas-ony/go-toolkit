package database

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/jmoiron/sqlx"
)

// WithTx executes fn inside a database transaction using the background
// context and the server's default isolation level.
//
// This is the shortcut form of WithTxContext, for a caller that holds no
// context to pass on. A caller that DOES hold a request context should call
// WithTxContext instead — see there for what a real context buys.
//
// fn receives a *sqlx.Tx (not sqlx.ExtContext) so that repository
// implementations can call their own .WithTx(tx) method to return a
// transaction-scoped copy of themselves:
//
//	err := database.WithTx(db, func(tx *sqlx.Tx) error {
//	    if err := repoA.WithTx(tx).Delete(id); err != nil {
//	        return err
//	    }
//	    return repoB.WithTx(tx).Delete(id)
//	})
func WithTx(db *sqlx.DB, fn func(tx *sqlx.Tx) error) error {
	return WithTxContext(context.Background(), db, nil, fn)
}

// WithTxContext is WithTx with an explicit context and isolation level,
// mirroring the Begin / BeginTx split in database/sql.
//
// # What ctx governs
//
// The TRANSACTION'S LIFETIME, not merely the BEGIN round trip. database/sql
// starts a goroutine at Begin that waits on ctx.Done() and rolls the
// transaction back when it fires, so a cancelled request releases its row
// locks at once instead of holding them until fn happens to return.
//
// Pass a request context and an abandoned HTTP call stops holding rows. Pass
// context.Background(), as WithTx does, and nothing can cancel the
// transaction early: it lives until fn returns.
//
// After cancellation the Tx is done: further statements on it fail, and the
// deferred Rollback returns sql.ErrTxDone, which is discarded.
//
// Commit's result is NOT discarded, and the distinction matters because it is
// what the caller actually sees. Depending on how far database/sql got before
// Commit was attempted, it returns either context.Canceled or sql.ErrTxDone,
// and that value becomes this function's return value. fn sees an ordinary
// error and needs no special case for it.
//
// # opts
//
// nil leaves the session's isolation level exactly as it is. What "as it is"
// MEANS is the engine's own default and differs across the two drivers this
// package can open: READ COMMITTED on SQL Server, REPEATABLE READ on MySQL's
// InnoDB. Nothing here changes it, so a query relying on non-repeatable reads
// not happening is relying on the engine, not on this package.
//
// Everything below is PER DRIVER, and the two disagree with each other in
// opposite directions on both fields:
//
//	                        go-mssqldb        go-sql-driver/mysql
//	LevelDefault            session default   session default
//	LevelReadUncommitted    ✓                 ✓
//	LevelReadCommitted      ✓                 ✓
//	LevelRepeatableRead     ✓                 ✓
//	LevelSerializable       ✓                 ✓
//	LevelSnapshot           ✓ (see below)     rejected
//	LevelWriteCommitted     rejected          rejected
//	LevelLinearizable       rejected          rejected
//	ReadOnly: true          rejected          ✓
//
// Both reject what they cannot express with an error out of Begin rather than
// a silent downgrade — the behaviour to want, since a downgrade would stay
// invisible until it corrupted something. The consequence is that an option is
// portable only if both rows accept it: LevelSnapshot and ReadOnly are each
// usable on exactly the engine the other is not, so code meant to run on both
// fails at BEGIN on one of them.
//
// LevelSnapshot additionally needs ALLOW_SNAPSHOT_ISOLATION ON for the
// database on SQL Server; the driver accepting the level says nothing about
// the server accepting the transaction.
//
// ReadOnly, in detail, because the asymmetry is the trap. SQL Server has no
// read-only transaction mode for the flag to map onto, and go-mssqldb refuses
// it before it reads any other option — including before the isolation level,
// so a call that is wrong in both ways reports this one (the only prior check
// is that the connection is still good):
//
//	if opts.ReadOnly {
//	    return nil, errors.New("read-only transactions are not supported")
//	}
//
// MySQL, by contrast, issues START TRANSACTION READ ONLY, where it is a real
// optimisation for query-only work.
//
// So &sql.TxOptions{ReadOnly: true} is a portability hazard rather than a
// free win: it fails at BEGIN on one engine and succeeds on the other, so
// code written against either one breaks on the other with nothing in the
// source to say why.
//
// One MySQL-specific mechanic worth knowing before reaching for a level there:
// go-sql-driver applies it as a SEPARATE "SET TRANSACTION ISOLATION LEVEL"
// round trip issued BEFORE the START TRANSACTION. That statement arms the next
// transaction on the session, so if the START then fails, the level is left
// armed on a connection that goes back to the pool and applies to whatever
// transaction checks it out next. Rare, but it is a cross-request effect, and
// nothing in database/sql resets it.
//
// # Still no nesting
//
// database/sql has no savepoints, so calling either function from inside fn
// does NOT nest. BeginTxx checks out a SECOND connection, which then blocks
// on the row locks the outer transaction is still holding — a self-deadlock
// lasting until the lock timeout, not an error. Pass the *sqlx.Tx down
// instead, through each repository's own WithTx method.
//
// Outcome by path:
//   - BeginTxx fails   → the error is returned immediately; no transaction was
//     started, so there is nothing to clean up.
//   - fn returns error → err is set, the deferred rollback fires, and fn's
//     error is returned.
//   - fn returns nil   → Commit is called and its result becomes err. If
//     Commit fails, the deferred Rollback still runs but is a harmless no-op:
//     database/sql marks the Tx done the moment Commit returns (success or
//     failure), so that Rollback returns sql.ErrTxDone, which is discarded.
//   - ctx is cancelled → database/sql rolls back on its own, asynchronously.
//     Any statement fn issues afterwards fails and takes the path above. If fn
//     issues none, it returns nil and Commit is where the cancellation
//     surfaces, as context.Canceled or sql.ErrTxDone. Either way the deferred
//     Rollback finds the Tx already done.
//   - fn panics        → the transaction is rolled back and the panic is
//     re-raised unchanged. Without this branch, a panicking handler would
//     abandon the checked-out connection with an open transaction — the pool
//     never gets the connection back and any row locks it holds stay held, so
//     repeated panics would eventually exhaust MaxOpenConns. It matters most
//     behind a recovery middleware, which catches the panic, converts it to an
//     error, and lets the process carry on serving with one fewer connection
//     than it started with; the guard is what keeps that arithmetic from being
//     permanent. With no such middleware it is still correct, for a smaller
//     reason — the transaction is cleaned up before the process dies.
//
// The named return variable `err` is intentional: the deferred closure reads
// it by reference after fn / Commit have assigned it, which routes every error
// path through the single rollback guard below instead of scattering Rollback
// calls at each return site.
func WithTxContext(
	ctx context.Context,
	db *sqlx.DB,
	opts *sql.TxOptions,
	fn func(tx *sqlx.Tx) error,
) (err error) {
	// `:=` here ASSIGNS to the named return rather than shadowing it: a short
	// variable declaration only creates variables that are new in the current
	// scope, and this is the function's top-level scope, where err already
	// exists as the named result. Only tx is new.
	//
	// Move this line inside any nested block and err becomes a fresh local.
	// The deferred guard would then read an outer err that stays nil forever,
	// and every failure would return without rolling back. The compiler says
	// nothing; the symptom is connections leaking out of the pool under load.
	tx, err := db.BeginTxx(ctx, opts)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}

	// Single cleanup point for all failure modes.
	//
	// The panic branch runs first: recover() returns non-nil only while
	// panicking, in which case the transaction is rolled back and the panic is
	// re-raised so neither entry point swallows it — callers observe exactly
	// the panic fn produced.
	//
	// The error branch is a conditional safety net: it fires only when
	// err != nil. On a successful Commit, err is nil, the condition is false,
	// and Rollback is never called at all. Rollback errors are discarded in
	// both branches because the original failure (fn's error, Commit's error,
	// or the panic value) is the actionable one for the caller.
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	// Assign to the named return `err` so the deferred closure sees fn's error
	// and rolls back before this function returns it.
	if err = fn(tx); err != nil {
		return err
	}

	// Commit's result is assigned to the named return for uniformity. If it
	// fails, the deferred Rollback fires but is a no-op (see the doc comment
	// above) and the commit error is returned as-is.
	//
	// As-is, and NOT wrapped as "commit tx: %w", which is the asymmetry with
	// the begin error above. The begin error is wrapped because nothing else
	// in the call could have produced it, so the prefix adds information. fn's
	// error is passed through untouched so callers can errors.Is their own
	// sentinels without this helper editorialising, and Commit follows the
	// same rule for consistency with it. The cost is real and worth naming: a
	// caller holding a bare driver error cannot tell whether it came from the
	// work or from the commit. If that ever needs answering, wrap here rather
	// than at fn — a prefix leaves errors.Is intact.
	return tx.Commit()
}
