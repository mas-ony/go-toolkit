package database

// Telling a row the table refuses from a database that has failed.

import (
	"errors"
	"fmt"

	"github.com/go-sql-driver/mysql"
	mssql "github.com/microsoft/go-mssqldb"
)

// ErrRejected is what Rejected wraps for a statement the table refused
// for the values it was sent: a NULL in a NOT NULL column, a value too
// long or out of range for its column, a value of the wrong type, or a
// key that is already stored.
//
// It is about the row rather than the database, so a caller storing many
// rows records it against the row and goes on to the next. Any other
// error is the database's own, and the next row would most likely fail
// the same way.
var ErrRejected = errors.New("the table refused the row")

// Rejected returns err wrapped in ErrRejected, with the server's own
// message, when err is the answer of either engine to a row the table
// refused; see ErrRejected for which those are. It returns every other
// error as it came, and nil for nil, so it can be applied to the result
// of any statement:
//
//	_, err = tx.ExecContext(ctx, query, args...)
//	return database.Rejected(err)
//
// The server's message names the column or the key, and is kept because
// that is what the person who has to fix the row needs. It can quote the
// value that was refused, so it is no more fit for a client that did not
// send the row than the row itself is.
//
// The answer is told by the error's number, never by its text, which
// changes with the server's version and language. A duplicate key is
// among the refusals: where a table has a unique key, it is how a row
// that is already stored is kept from being stored again.
func Rejected(err error) error {
	if err == nil {
		return nil
	}
	var sqlServer mssql.Error
	if errors.As(err, &sqlServer) {
		switch sqlServer.Number {
		case 515, // NULL in a NOT NULL column
			2628, 8152, // value too long
			8114, 8115, // wrong type, out of range
			2601, 2627: // duplicate key: unique index, constraint
			return fmt.Errorf("%w: %s", ErrRejected,
				sqlServer.Message)
		}
	}
	var mySQL *mysql.MySQLError
	if errors.As(err, &mySQL) {
		switch mySQL.Number {
		case 1048, // NULL in a NOT NULL column
			1406,       // value too long
			1366, 1292, // value of the wrong type
			1264, // value out of range
			1062: // duplicate key
			return fmt.Errorf("%w: %s", ErrRejected,
				mySQL.Message)
		}
	}
	return err
}
