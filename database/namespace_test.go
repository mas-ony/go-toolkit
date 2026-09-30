package database

// Tests for namespace.go.
//
// What the file promises: a plain name stays bare without consulting the
// dialect, a name reserved on either engine or not a plain identifier is
// quoted, the pasted word lists keep their shape, and NamespaceForYear
// rewrites the database segment alone, idempotently for the names it
// documents.

import (
	"strings"
	"testing"
)

// Configure stores exactly the prefix it is given, the empty one included.
func TestConfigure(t *testing.T) {
	prev := DefaultNamespace
	t.Cleanup(func() { DefaultNamespace = prev })

	for _, ns := range []string{"app2026.dbo", "app2026", ""} {
		Configure(ns)
		if DefaultNamespace != ns {
			t.Errorf("after Configure(%q), DefaultNamespace = %q",
				ns, DefaultNamespace)
		}
	}
}

// Plain names must stay bare without consulting the dialect, so this runs
// with none selected: a change that quoted one of them would panic here.
func TestQualifyLeavesPlainNamesBare(t *testing.T) {
	withoutDialect(t)
	cases := []struct{ ns, table, want string }{
		{"app2026.dbo", "invoice", "app2026.dbo.invoice"},
		{"app2026", "invoice_line", "app2026.invoice_line"},
		{"", "invoice", "invoice"},
		{"app2026", "_staging", "app2026._staging"},
		{"app2026", "Report2026", "app2026.Report2026"},
		{"app2026", "status", "app2026.status"},
		{"app2026", "users", "app2026.users"},
	}
	for _, c := range cases {
		if got := Qualify(c.ns, c.table); got != c.want {
			t.Errorf("Qualify(%q, %q) = %q, want %q",
				c.ns, c.table, got, c.want)
		}
	}
}

// A reserved word on either engine, and anything that is not a plain
// ASCII identifier, is quoted in the selected grammar.
func TestQualifyQuotesNamesThatCannotBeBare(t *testing.T) {
	cases := []struct {
		name, ns, table  string
		mysql, sqlserver string
	}{
		{
			"reserved in T-SQL only", "app.dbo", "user",
			"app.dbo.`user`", "app.dbo.[user]",
		},
		{
			"reserved in MySQL only", "app", "rank",
			"app.`rank`", "app.[rank]",
		},
		{
			"reserved on both, in any case", "app", "Order",
			"app.`Order`", "app.[Order]",
		},
		{
			"bare reserved name", "", "user",
			"`user`", "[user]",
		},
		{
			"leading digit", "app", "2024_data",
			"app.`2024_data`", "app.[2024_data]",
		},
		{
			"punctuation", "app", "line-item",
			"app.`line-item`", "app.[line-item]",
		},
		{
			"space", "app", "line item",
			"app.`line item`", "app.[line item]",
		},
		{
			"non-ASCII letter", "app", "café",
			"app.`café`", "app.[café]",
		},
	}
	for _, c := range cases {
		withDialect(t, DialectMySQL)
		if got := Qualify(c.ns, c.table); got != c.mysql {
			t.Errorf("%s on MySQL: Qualify(%q, %q) = %q, want %q",
				c.name, c.ns, c.table, got, c.mysql)
		}
		withDialect(t, DialectSQLServer)
		if got := Qualify(c.ns, c.table); got != c.sqlserver {
			t.Errorf("%s on SQL Server: Qualify(%q, %q) = %q, want %q",
				c.name, c.ns, c.table, got, c.sqlserver)
		}
	}
}

// isPlainName accepts an ASCII letter or underscore followed by letters,
// digits and underscores, and nothing else.
func TestIsPlainName(t *testing.T) {
	cases := map[string]bool{
		"a":          true,
		"_":          true,
		"invoice_2":  true,
		"A1_b2":      true,
		"":           false,
		"1a":         false,
		"a-b":        false,
		"a b":        false,
		"a$b":        false,
		"a.b":        false,
		"#temp":      false,
		"@var":       false,
		"caf\u00e9":  false,
		"tab\u200bx": false,
	}
	for name, want := range cases {
		if got := isPlainName(name); got != want {
			t.Errorf("isPlainName(%q) = %v, want %v", name, got, want)
		}
	}
}

// The word lists are pasted from their sources, so their shape is checked
// here: sorted, without repeats, every entry an upper-case plain name. The
// counts catch a list cut short while editing, and the spot checks pin the
// entries whose absence would reopen a known failure.
func TestReservedWords(t *testing.T) {
	lists := []struct {
		name  string
		words string
		count int
	}{
		{"tsqlReserved", tsqlReserved, 184},
		{"mysqlReserved", mysqlReserved, 262},
		{"mariadbReserved", mariadbReserved, 15},
	}
	for _, l := range lists {
		words := strings.Fields(l.words)
		if len(words) != l.count {
			t.Errorf("%s has %d words, want %d", l.name, len(words), l.count)
		}
		for i, w := range words {
			if w != strings.ToUpper(w) || !isPlainName(w) {
				t.Errorf("%s: malformed word %q", l.name, w)
			}
			if i > 0 && words[i-1] >= w {
				t.Errorf("%s: %q does not sort after %q",
					l.name, w, words[i-1])
			}
		}
	}

	for _, w := range []string{"USER", "FILE", "RANK", "SYSTEM", "ORDER",
		"OFFSET", "RETURNING", "PORTION"} {
		if _, ok := reservedWords[w]; !ok {
			t.Errorf("reservedWords lacks %s", w)
		}
	}
	for _, w := range []string{"STATUS", "NAME", "TYPE", "ROLE", "USERS"} {
		if _, ok := reservedWords[w]; ok {
			t.Errorf("reservedWords contains %s, which is not reserved", w)
		}
	}
}

// NamespaceForYear rewrites only the database segment, replacing a year
// already there, and leaves the prefix alone for an empty prefix or a year
// below 1.
func TestNamespaceForYear(t *testing.T) {
	cases := []struct {
		name, ns string
		year     int
		want     string
	}{
		{"SQL Server prefix", "app2026.dbo", 2025, "app2025.dbo"},
		{"MySQL prefix", "app2026", 2025, "app2025"},
		{"prefix without a year", "app.dbo", 2025, "app2025.dbo"},
		{"schema untouched", "app2026.dbo", 2030, "app2030.dbo"},
		{"only the first segment", "app2026.s2026", 2025, "app2025.s2026"},
		{"digit that is not a year", "app_v2", 2025, "app_v22025"},
		{"year zero", "app2026.dbo", 0, "app2026.dbo"},
		{"negative year", "app2026", -1, "app2026"},
		{"empty prefix", "", 2025, ""},
	}
	for _, c := range cases {
		if got := NamespaceForYear(c.ns, c.year); got != c.want {
			t.Errorf("%s: NamespaceForYear(%q, %d) = %q, want %q",
				c.name, c.ns, c.year, got, c.want)
		}
	}
}

// Idempotence is what lets a scoped copy be scoped again without the year
// compounding into a database nobody created. It holds for four-digit years
// and names that do not end in a digit before the year; see
// NamespaceForYear for the two exceptions.
func TestNamespaceForYearIsIdempotent(t *testing.T) {
	prefixes := []string{"app2026.dbo", "app2026", "app", "app_v2_2026"}
	for _, ns := range prefixes {
		for _, year := range []int{1999, 2025, 9999} {
			once := NamespaceForYear(ns, year)
			if twice := NamespaceForYear(once, year); twice != once {
				t.Errorf("NamespaceForYear(%q, %d) = %q, then %q",
					ns, year, once, twice)
			}
		}
	}
}

// trimYearSuffix removes exactly four trailing digits after a non-digit,
// and nothing otherwise.
func TestTrimYearSuffix(t *testing.T) {
	cases := []struct{ in, want string }{
		{"app2026", "app"},
		{"app", "app"},
		{"app_v2", "app_v2"},
		{"app_v20", "app_v20"},
		{"app12026", "app12026"},
		{"app1234", "app"},
		{"app_2026", "app_"},
		{"caf\u00e92026", "caf\u00e9"},
		{"2026", ""},
		{"202", "202"},
		{"", ""},
	}
	for _, c := range cases {
		if got := trimYearSuffix(c.in); got != c.want {
			t.Errorf("trimYearSuffix(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// Qualify reads the grammar only for a name that cannot appear bare, so the
// method form needs a valid receiver only in that case. Both halves matter:
// the lazy path is what lets a repository of plain names work before
// SetDialect has run, and the quoted path is what the receiver is for.
func TestQualifyMethodResolvesTheGrammarLazily(t *testing.T) {
	withoutDialect(t)

	var noGrammar Dialect
	if got := noGrammar.Qualify("app2026", "invoice"); got !=
		"app2026.invoice" {
		t.Errorf("plain name = %q, want app2026.invoice", got)
	}
	mustPanic(t, "Qualify with a reserved name on Dialect(0)", func() {
		_ = noGrammar.Qualify("app2026", "user")
	})

	for _, c := range []struct {
		d    Dialect
		want string
	}{
		{DialectMySQL, "app2026.`user`"},
		{DialectSQLServer, "app2026.[user]"},
	} {
		if got := c.d.Qualify("app2026", "user"); got != c.want {
			t.Errorf("%v: got %q, want %q", c.d, got, c.want)
		}
	}
}
