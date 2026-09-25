//go:build integration

package database

// Integration tests for namespace.go. They share the DSN variables and the
// helpers of dialect_integration_test.go and run the same way.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
)

// serverReservedWords returns the words this server refuses as a bare
// name, and says how it found out.
//
// MySQL 8 answers directly: INFORMATION_SCHEMA.KEYWORDS carries a RESERVED
// column. MariaDB's table of the same name lists every keyword with no such
// column, so on MariaDB the answer is MEASURED — each keyword is used as a
// bare table name in CREATE TEMPORARY TABLE, and a syntax error means the
// parser will not accept it there. That is the position Qualify protects,
// so the measurement asks exactly the question the list exists to answer.
//
// Temporary tables belong to one connection, so the probe pins one: a
// CREATE and its DROP on two pooled connections would leave the table
// behind on the first. The scratch login needs CREATE TEMPORARY TABLES,
// which any grant of ALL on the scratch database includes.
func serverReservedWords(db *sqlx.DB) ([]string, string, error) {
	var words []string
	err := db.Select(&words, "SELECT WORD FROM INFORMATION_SCHEMA.KEYWORDS"+
		" WHERE RESERVED = 1")
	if err == nil {
		return words, "reported by the server", nil
	}

	var all []string
	if err := db.Select(&all,
		"SELECT WORD FROM INFORMATION_SCHEMA.KEYWORDS"); err != nil {
		return nil, "", err
	}

	ctx := context.Background()
	conn, err := db.Connx(ctx)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = conn.Close() }()

	const syntaxError = 1064
	for _, w := range all {
		// Operators such as <=> are keywords too, but never a name:
		// needsQuoting quotes anything that is not plain already.
		if !isPlainName(w) {
			continue
		}
		_, err := conn.ExecContext(ctx,
			"CREATE TEMPORARY TABLE "+w+" (x INT)")
		var myErr *mysql.MySQLError
		switch {
		case err == nil:
			if _, err := conn.ExecContext(ctx,
				"DROP TEMPORARY TABLE "+w); err != nil {
				return nil, "", err
			}
		case errors.As(err, &myErr) && myErr.Number == syntaxError:
			words = append(words, w)
		default:
			return nil, "", fmt.Errorf("probing %s: %w", w, err)
		}
	}
	return words, "measured by using each keyword as a bare table name",
		nil
}

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
		words, how, err := serverReservedWords(db)
		if err != nil {
			t.Skipf("this server has no keyword catalog: %v", err)
		}
		if len(words) == 0 {
			t.Fatalf("the server reported no reserved words (%s)", how)
		}

		var missing []string
		for _, w := range words {
			if _, ok := reservedWords[strings.ToUpper(w)]; !ok {
				missing = append(missing, w)
			}
		}
		if len(missing) > 0 {
			t.Errorf("reserved on this server (%s) but not in "+
				"reservedWords: %s", how, strings.Join(missing, " "))
		}
	})
}
