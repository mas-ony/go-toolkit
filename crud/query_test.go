package crud

// Tests for query.go. The builders inspect strings and open nothing, so
// each test asserts the text and the arguments a Query holds; repo_test.go
// shows the same text reaching a driver.

import (
	"reflect"
	"testing"
)

// wantQuery fails t unless q holds exactly sql and args.
func wantQuery(t *testing.T, q *Query, sql string, args map[string]any) {
	t.Helper()
	if q.sql != sql {
		t.Errorf("sql:\ngot  %q\nwant %q", q.sql, sql)
	}
	if !reflect.DeepEqual(q.args, args) {
		t.Errorf("args = %#v, want %#v", q.args, args)
	}
}

// NewSelect opens the WHERE clause with a tautology, so every predicate
// after it is appended the same way.
func TestNewSelect(t *testing.T) {
	q := NewSelect("t.id, t.code", "app.item AS t")
	wantQuery(t, q, "SELECT t.id, t.code\nFROM app.item AS t\nWHERE (1=1)",
		map[string]any{})
	if q.single || q.prefix != "" {
		t.Errorf("a new query is single %t with prefix %q",
			q.single, q.prefix)
	}
}

// And appends a parenthesised predicate, Bind sets its parameter, and One
// marks a lookup.
func TestAndBindAndOne(t *testing.T) {
	q := NewSelect("t.id", "t").
		And("t.a = :a OR t.b = :a").Bind("a", 1).One()
	wantQuery(t, q, "SELECT t.id\nFROM t\nWHERE (1=1)"+
		"\n  AND (t.a = :a OR t.b = :a)", map[string]any{"a": 1})
	if !q.single {
		t.Error("One did not mark the query")
	}
}

// Each value helper filters only when it has a value: a nil pointer and
// an empty slice add nothing, so an unset field of a read option is no
// filter.
func TestValueHelpersSkipAnAbsentValue(t *testing.T) {
	q := NewSelect("t.id", "t")
	AndEq[int](q, "t.a", "a", nil)
	AndCmp[int](q, "t.b", ">=", "b", nil)
	AndLike(q, "t.c", "c", nil)
	AndAnyLike(q, "d", nil, "t.d")
	AndAnyLike(q, "e", new("x"))
	AndIn[int](q, "t.f", "f", nil)
	wantQuery(t, q, "SELECT t.id\nFROM t\nWHERE (1=1)", map[string]any{})
}

// The value helpers bind what they are given under the parameter name
// they are given, and a LIKE escapes the pattern's own metacharacters.
func TestValueHelpers(t *testing.T) {
	q := NewSelect("t.id", "t")
	AndEq(q, "t.a", "a", new(1))
	AndCmp(q, "t.b", ">=", "b", new("2026-01-01"))
	AndLike(q, "t.c", "c", new("50%_off"))
	AndAnyLike(q, "s", new("x"), "t.d", "t.e")
	AndIn(q, "t.f", "f", []int{3, 4})
	wantQuery(t, q, "SELECT t.id\nFROM t\nWHERE (1=1)"+
		"\n  AND (t.a = :a)"+
		"\n  AND (t.b >= :b)"+
		"\n  AND (t.c LIKE :c ESCAPE '!')"+
		"\n  AND (t.d LIKE :s ESCAPE '!'\n    OR t.e LIKE :s ESCAPE '!')"+
		"\n  AND (t.f IN (:f_0, :f_1))",
		map[string]any{
			"a":   1,
			"b":   "2026-01-01",
			"c":   "%50!%!_off%",
			"s":   "%x%",
			"f_0": 3,
			"f_1": 4,
		})
}

// A later Prepend goes in front of an earlier one, and neither touches
// the SELECT.
func TestPrepend(t *testing.T) {
	q := NewSelect("t.id", "t").Prepend("B").Prepend("A")
	if q.prefix != "A\nB\n" {
		t.Errorf("prefix = %q, want %q", q.prefix, "A\nB\n")
	}
	wantQuery(t, q, "SELECT t.id\nFROM t\nWHERE (1=1)", map[string]any{})
}
