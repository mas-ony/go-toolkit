//go:build integration

package database

// Integration tests for namespace.go. They share the DSN variables and the
// helpers of dialect_integration_test.go and run the same way.

import (
	"strings"
	"testing"

	"github.com/jmoiron/sqlx"
)

// Each name below either needs quoting on at least one engine or must work
// bare, so writing and reading a table under it, with and without a
// namespace, exercises both outcomes of needsQuoting. The tables carry these
// exact names, and the namespace is the connection's own database, whose
// name must be a plain identifier.
func TestIntegrationQualifiedNamesParse(t *testing.T) {
	names := []string{
		"user", "rank", "order", "file", "system",
		"status", "line item", "2024_data",
	}
	forEachEngine(t, func(t *testing.T, db *sqlx.DB) {
		var ns string
		if err := db.Get(&ns, onEngine(
			"SELECT DATABASE()",
			"SELECT DB_NAME() + '.' + SCHEMA_NAME()",
		)); err != nil {
			t.Fatal(err)
		}

		for _, name := range names {
			qualified := Qualify(ns, name)
			bare := Qualify("", name)
			mustExec(t, db,
				"DROP TABLE IF EXISTS "+qualified,
				"CREATE TABLE "+qualified+" (id INT NOT NULL)",
			)
			t.Cleanup(func() {
				_, _ = db.Exec("DROP TABLE IF EXISTS " + qualified)
			})
			mustExec(t, db,
				"INSERT INTO "+qualified+" (id) VALUES (1)",
				"INSERT INTO "+bare+" (id) VALUES (2)",
				"UPDATE "+bare+" SET id = 3 WHERE id = 2",
				"DELETE FROM "+qualified+" WHERE id = 3",
			)

			var n int
			query := "SELECT COUNT(*) FROM " + bare + " b" +
				" JOIN " + qualified + " q ON q.id = b.id"
			if err := db.Get(&n, query); err != nil {
				t.Fatalf("%v\n%s", err, query)
			}
			if n != 1 {
				t.Errorf("%s: %d rows, want 1", name, n)
			}
		}
	})
}

// mysqlReserved is copied from one server version. Every word the server
// under test reports as reserved must be in reservedWords, so a newer server
// that reserves more words fails here and names them.
func TestIntegrationReservedWordsCoverServer(t *testing.T) {
	forEachEngine(t, func(t *testing.T, db *sqlx.DB) {
		if activeDialect() != DialectMySQL {
			t.Skip("SQL Server offers no catalog of reserved words")
		}
		const query = "SELECT WORD FROM INFORMATION_SCHEMA.KEYWORDS" +
			" WHERE RESERVED = 1"
		var words []string
		if err := db.Select(&words, query); err != nil {
			t.Skipf("this server has no keyword catalog: %v", err)
		}
		if len(words) == 0 {
			t.Fatal("the server reported no reserved words")
		}

		var missing []string
		for _, w := range words {
			if _, ok := reservedWords[strings.ToUpper(w)]; !ok {
				missing = append(missing, w)
			}
		}
		if len(missing) > 0 {
			t.Errorf("reserved on this server but not in mysqlReserved: %s",
				strings.Join(missing, " "))
		}
	})
}
