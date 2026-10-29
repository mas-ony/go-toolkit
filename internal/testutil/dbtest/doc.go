// Package dbtest is the harness the integration suites that need a real
// server share: which servers a suite can run against, the variable each
// one's DSN is read from, how a test runs once per server whose DSN is set
// and skips when none is, how a statement is run against it, and how a
// scratch table is named and dropped, including one whose name only a
// quoted identifier can hold.
//
// The database, crud and datetime suites read the same two variables, so
// one scratch server serves all three:
//
//	DB_TEST_MYSQL_DSN
//		user:pass@tcp(host:3306)/scratch?parseTime=true&loc=Asia%2FJakarta
//	DB_TEST_SQLSERVER_DSN
//		sqlserver://user:pass@host?database=scratch&timezone=Asia%2FJakarta
//
// The zone parameters are for datetime, which needs them to name the zone
// the test process runs in; database and crud pass with or without them.
//
// The suites create and drop their own tables. Point the DSNs at a scratch
// database, never at one that holds data.
//
// The package links both drivers, so a suite that imports it can open
// either. Only suites behind the integration build tag import it, which
// keeps the drivers and a DDL round trip out of the default test build.
package dbtest
