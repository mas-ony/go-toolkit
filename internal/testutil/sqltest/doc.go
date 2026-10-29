// Package sqltest holds what the unit suites of the SQL packages share
// when they test against a fake database/sql driver rather than a
// server: the parts of a driver every fake has to supply whether it uses
// them or not, a result set replayed from memory, a log of the
// statements a fake is handed, the *sqlx.DB a fake is opened as, and the
// SELECT list the database package spells.
//
// The database and crud suites each fake a driver of their own, because
// each wants something different from one: the transaction fake counts
// BEGIN, COMMIT and ROLLBACK and refuses statements, and the query fakes
// answer the statements they are handed from canned rows. What more than
// one fake needs lives here, so the copies cannot differ: the parts no
// fake is about, and the Log the query fakes, and dbtest's own suite,
// keep the statements they are handed in.
//
// It links sqlx and no driver, so a unit suite that imports it carries no
// driver and needs no server. The suites that run against a real server
// build on dbtest instead. It imports nothing of the toolkit's, so the
// database package's own tests may use it.
package sqltest
