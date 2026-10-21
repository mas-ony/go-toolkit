package dbtest

// Tests for dbtest.go that need no server: the engines are the two the
// database package opens, a suite runs once per DSN that is set and is
// skipped when none is, and scratch names do not repeat.

import (
	"strings"
	"testing"

	"github.com/mas-ony/go-toolkit/internal/dbdriver"
)

// Both engines are listed, MySQL first, each under a driver name the
// database package resolves and the variable the README documents.
func TestEngines(t *testing.T) {
	t.Parallel()
	want := []Engine{
		{Driver: "mysql", DSNEnv: "DB_TEST_MYSQL_DSN"},
		{Driver: "sqlserver", DSNEnv: "DB_TEST_SQLSERVER_DSN"},
	}
	if len(Engines) != len(want) {
		t.Fatalf("Engines = %v, want %v", Engines, want)
	}
	for i, e := range Engines {
		if e != want[i] {
			t.Errorf("Engines[%d] = %+v, want %+v", i, e, want[i])
		}
		if dbdriver.Engine(e.Driver) != e.Driver {
			t.Errorf("%q is not an engine the database package opens",
				e.Driver)
		}
	}
}

// With neither variable set, the suite is skipped and nothing runs.
func TestEachSkipsWithoutADSN(t *testing.T) {
	for _, e := range Engines {
		t.Setenv(e.DSNEnv, "")
	}
	ran := false
	skipped := false
	t.Run("suite", func(t *testing.T) {
		t.Cleanup(func() { skipped = t.Skipped() })
		Each(t, func(*testing.T, Engine, string) { ran = true })
	})
	if ran || !skipped {
		t.Errorf("ran %t, skipped %t; want a skip and no run", ran, skipped)
	}
}

// With one variable set, the suite runs once, against that engine, and is
// handed the DSN as it was set.
func TestEachRunsPerDSN(t *testing.T) {
	t.Setenv("DB_TEST_MYSQL_DSN", "user:pass@tcp(db:3306)/scratch")
	t.Setenv("DB_TEST_SQLSERVER_DSN", "")
	var seen []string
	Each(t, func(t *testing.T, e Engine, dsn string) {
		seen = append(seen, e.Driver+" "+dsn)
		if t.Name() != "TestEachRunsPerDSN/mysql" {
			t.Errorf("subtest named %q", t.Name())
		}
	})
	if len(seen) != 1 || seen[0] != "mysql user:pass@tcp(db:3306)/scratch" {
		t.Errorf("ran %v, want one mysql run with its DSN", seen)
	}
}

// Names carry the prefix and never repeat within a run.
func TestUnique(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for range 100 {
		name := Unique("scratch_")
		if !strings.HasPrefix(name, "scratch_") {
			t.Fatalf("%q lost its prefix", name)
		}
		if seen[name] {
			t.Fatalf("%q was handed out twice", name)
		}
		seen[name] = true
	}
}
