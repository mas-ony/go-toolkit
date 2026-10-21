// Package testutil holds the test helpers more than one package of the
// toolkit needs: a package variable set for one test and restored, a panic
// caught as a value, what a fake server recorded reduced to the one entry
// a test expects, a file written with the directories above it, the files
// a suite that reads its own package's source lists and each one parsed,
// a mode set for one test and restored, the two things a filesystem test
// has to ask of the machine it runs on, and the one fake credential two
// packages are tested with.
//
// A helper one package needs stays in that package's own _test.go files,
// beside the tests that use it. One here is used by two or more, and lives
// here so the copies cannot differ: a skip condition written twice is two
// conditions, and the day they stop agreeing a test is skipped on one
// machine and run on another for no reason anyone chose.
//
// Only _test.go files import this package and the ones under it, so none
// of it reaches a service's binary. They are split by what they link, so
// a test binary carries only what its own helpers need: this one the
// standard library alone, dbtest both database drivers, xlsxtest excelize,
// and fibertest Fiber. None of them imports a package whose tests import
// it, which would be a cycle; this one imports nothing of the toolkit's at
// all, so the tests of any package may use it.
//
// Like everything under internal/, it is not an API a service may depend
// on, and it moves whenever a test needs it to.
package testutil
