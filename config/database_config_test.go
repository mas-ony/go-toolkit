package config

// Tests for database_config.go.
//
// What the section promises: every required key reported by name, the
// naming rules each driver brings, the encrypt vocabulary with case forgiven
// and nothing else, duration floors that catch a bare number, the pool
// invariants, and a log line that names the target and never the
// credentials.
//
// Rule cases are built through NewDatabaseConfig from a Viper, the way a
// deployment builds the section, so a passing case has exercised the key
// name and the cast as well as the rule. Configured is a question about the
// struct itself, so its test builds literals as well.

import (
	"strings"
	"testing"

	"github.com/spf13/viper"
)

// sqlServerBase is a configuration the database section accepts on SQL
// Server, which each rule case changes one key at a time.
var sqlServerBase = map[string]any{
	"database.driver":             "sqlserver",
	"database.host":               "db.internal",
	"database.port":               1433,
	"database.catalog":            "app",
	"database.schema":             "dbo",
	"database.username":           "svc",
	"database.password":           "pw",
	"database.encrypt":            "true",
	"database.conn_max_idle_time": "1m",
	"database.conn_max_lifetime":  "5m",
	"database.max_idle_conns":     2,
	"database.max_open_conns":     10,
}

// buildDatabase adapts NewDatabaseConfig to the constructor shape runRules
// takes.
func buildDatabase(v *viper.Viper) SectionConfig {
	return NewDatabaseConfig(v)
}

// TestDatabaseValidate holds the section's rules: the required keys, the
// naming rules each driver brings, the encrypt vocabulary, the duration
// floors, and the pool invariants.
func TestDatabaseValidate(t *testing.T) {
	t.Parallel()
	runRules(t, sqlServerBase, buildDatabase, []ruleCase{
		{"an unknown driver", map[string]any{
			"database.driver": "oracle"}, "database.driver"},
		{"no host", map[string]any{"database.host": nil}, "database.host"},
		{"no port", map[string]any{"database.port": nil}, "database.port"},
		{"no catalog", map[string]any{
			"database.catalog": nil}, "database.catalog"},
		{"no username", map[string]any{
			"database.username": nil}, "database.username"},
		{"no password", map[string]any{
			"database.password": nil}, "database.password"},
		// Schema is required on SQL Server, where a namespace has two
		// parts, and refused on MySQL, where it has one.
		{"sqlserver without a schema", map[string]any{
			"database.schema": nil}, "database.schema"},
		{"mysql with a schema", map[string]any{
			"database.driver": "mysql"}, "database.schema"},
		{"mysql without one is fine", map[string]any{
			"database.driver": "mysql", "database.schema": nil}, ""},
		{"no encrypt mode", map[string]any{
			"database.encrypt": nil}, "database.encrypt"},
		{"an unknown encrypt mode", map[string]any{
			"database.encrypt": "maybe"}, "database.encrypt"},
		// The base carries "true"; the other three modes are as good.
		{"disable", map[string]any{"database.encrypt": "disable"}, ""},
		{"false", map[string]any{"database.encrypt": "false"}, ""},
		{"strict", map[string]any{"database.encrypt": "strict"}, ""},
		// Case is forgiven: the value is lowercased before the lookup.
		{"a mode in capitals", map[string]any{
			"database.encrypt": "TRUE"}, ""},
		// A spelling go-mssqldb would parse on its own has no MySQL
		// translation, so it is refused by name.
		{"a ParseBool spelling", map[string]any{
			"database.encrypt": "1"}, "database.encrypt must be one of"},
		{"no idle time", map[string]any{
			"database.conn_max_idle_time": nil},
			"database.conn_max_idle_time"},
		{"no lifetime", map[string]any{
			"database.conn_max_lifetime": nil},
			"database.conn_max_lifetime"},
		// database/sql reads a negative duration as "never", and a bare
		// number is nanoseconds.
		{"a negative idle time", map[string]any{
			"database.conn_max_idle_time": "-5m"},
			"database.conn_max_idle_time must be at least 1s"},
		{"a bare-number lifetime", map[string]any{
			"database.conn_max_lifetime": 1800},
			"database.conn_max_lifetime must be at least 1s"},
		{"negative idle connections", map[string]any{
			"database.max_idle_conns": -1}, "database.max_idle_conns"},
		{"negative open connections", map[string]any{
			"database.max_open_conns": -1}, "database.max_open_conns"},
		// More idle connections than the pool may hold can never be
		// reached, so it is a misreading of one of the two settings.
		{"more idle than open", map[string]any{
			"database.max_idle_conns": 20}, "database.max_idle_conns"},
		{"unlimited open allows any idle", map[string]any{
			"database.max_open_conns": 0,
			"database.max_idle_conns": 20}, ""},
	})
}

// Every problem at once, which is the promise every Validate makes.
func TestDatabaseValidateReportsEverythingAtOnce(t *testing.T) {
	t.Parallel()
	err := NewDatabaseConfig(viper.New()).Validate()
	if err == nil {
		t.Fatal("an empty database section validated")
	}
	for _, key := range []string{"database.driver", "database.host",
		"database.port", "database.catalog", "database.username",
		"database.password"} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("error does not mention %s:\n%v", key, err)
		}
	}
}

// Namespace is what a service hands to database.Configure, and its shape
// differs by engine: SQL Server qualifies a table by catalog AND schema,
// MySQL by the catalog alone. Stray whitespace and dots are trimmed, since
// either would produce a qualifier the engine reads as a different name.
func TestDatabaseNamespace(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, driver, catalog, schema, want string
	}{
		{"sqlserver, both parts", "sqlserver", "app", "dbo", "app.dbo"},
		{"sqlserver, schema only", "sqlserver", "", "dbo", "dbo"},
		{"sqlserver, catalog only", "sqlserver", "app", "", "app"},
		{"sqlserver, neither", "sqlserver", "", "", ""},
		{"sqlserver, stray dots and space", "sqlserver", " app. ", ".dbo",
			"app.dbo"},
		{"mysql ignores the schema", "mysql", "app", "dbo", "app"},
		{"mysql, no catalog", "mysql", "", "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			v := viper.New()
			v.Set("database.driver", c.driver)
			v.Set("database.catalog", c.catalog)
			v.Set("database.schema", c.schema)
			if got := NewDatabaseConfig(v).Namespace(); got != c.want {
				t.Errorf("Namespace() = %q, want %q", got, c.want)
			}
		})
	}
}

// String names the connection target — which is what an operator reads
// the startup line for — while the contract suite separately pins that
// the password never appears.
func TestDatabaseStringNamesTheTarget(t *testing.T) {
	t.Parallel()
	got := NewDatabaseConfig(withKeys(sqlServerBase, nil)).String()
	for _, want := range []string{"sqlserver", "db.internal", "1433"} {
		if !strings.Contains(got, want) {
			t.Errorf("String does not show %q:\n%s", want, got)
		}
	}
}

// Configured tells an absent section from a present one, a nil section
// included, and one setting is enough to count as present.
func TestDatabaseConfigured(t *testing.T) {
	t.Parallel()
	var none *DatabaseConfig
	for name, c := range map[string]*DatabaseConfig{
		"nil":         none,
		"zero":        {},
		"from no key": NewDatabaseConfig(viper.New()),
	} {
		if c.Configured() {
			t.Errorf("%s: Configured() = true, want false", name)
		}
	}
	for name, c := range map[string]*DatabaseConfig{
		"a full section": NewDatabaseConfig(withKeys(sqlServerBase, nil)),
		"one setting":    {Host: "db.internal"},
	} {
		if !c.Configured() {
			t.Errorf("%s: Configured() = false, want true", name)
		}
	}
}
