package dbdriver

// Tests for dbdriver.go: which driver names select which engine, what each
// encrypt value asks of each driver, the spellings both lookups forgive
// and the ones they refuse, and lists that name exactly what the lookups
// accept.

import (
	"slices"
	"testing"
)

// Every accepted name selects its engine, whatever its case and the space
// around it, and anything else selects none.
func TestEngine(t *testing.T) {
	t.Parallel()
	for driver, want := range map[string]string{
		"sqlserver":     SQLServer,
		"mssql":         SQLServer,
		"mysql":         MySQL,
		" SQLServer\t":  SQLServer,
		"MSSQL":         SQLServer,
		"\nMySQL ":      MySQL,
		"":              "",
		"azuresql":      "",
		"postgres":      "",
		"my sql":        "",
		"sqlserver;x=1": "",
	} {
		if got := Engine(driver); got != want {
			t.Errorf("Engine(%q) = %q, want %q", driver, got, want)
		}
	}
}

// The translation, row by row, as the database package documents it.
func TestEncryptionFor(t *testing.T) {
	t.Parallel()
	for mode, want := range map[string]Encryption{
		"disable": {SQLServer: "disable", MySQL: "false"},
		"false":   {SQLServer: "false", MySQL: "preferred"},
		"true":    {SQLServer: "true", MySQL: "true"},
		"strict":  {SQLServer: "strict", MySQL: "true"},
	} {
		got, ok := EncryptionFor(mode)
		if !ok || got != want {
			t.Errorf("EncryptionFor(%q) = %+v, %t, want %+v, true",
				mode, got, ok, want)
		}
	}
}

// Case and surrounding space are forgiven; the short spellings go-mssqldb
// would take on its own are not, and neither is skip-verify, which would
// make "true" mean unauthenticated on MySQL.
func TestEncryptionForSpellings(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"DISABLE", " disable ", "\tStrict\n"} {
		if _, ok := EncryptionFor(mode); !ok {
			t.Errorf("EncryptionFor(%q) refused a canonical value", mode)
		}
	}
	for _, mode := range []string{
		"", "0", "1", "t", "f", "yes", "no", "mandatory", "optional",
		"require", "verify-full", "skip-verify", "preferred",
	} {
		if e, ok := EncryptionFor(mode); ok {
			t.Errorf("EncryptionFor(%q) = %+v, want none", mode, e)
		}
	}
}

// The lists are the order errors name the values in, every listed value
// is accepted by its lookup, and each call hands out a slice of its own.
func TestListsNameWhatTheLookupsAccept(t *testing.T) {
	t.Parallel()
	names := Names()
	if want := []string{"sqlserver", "mssql", "mysql"}; !slices.Equal(
		names, want) {
		t.Errorf("Names() = %v, want %v", names, want)
	}
	for _, n := range names {
		if Engine(n) == "" {
			t.Errorf("Names() lists %q, which Engine refuses", n)
		}
	}

	modes := EncryptModes()
	if want := []string{"disable", "false", "true", "strict"}; !slices.Equal(
		modes, want) {
		t.Errorf("EncryptModes() = %v, want %v", modes, want)
	}
	for _, m := range modes {
		if _, ok := EncryptionFor(m); !ok {
			t.Errorf("EncryptModes() lists %q, which EncryptionFor refuses",
				m)
		}
	}

	names[0], modes[0] = "mutated", "mutated"
	if Names()[0] == "mutated" || EncryptModes()[0] == "mutated" {
		t.Error("a list is shared: one caller's edit reached the next")
	}
}
