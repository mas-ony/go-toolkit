package database

// Tests for refusal.go. The errors are built as each driver builds
// them, so no server is needed: what Rejected reads is the error's type
// and number, and both are the driver's own.

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
	mssql "github.com/microsoft/go-mssqldb"
)

// messageOf returns the server's message an error of either engine
// carries.
func messageOf(err error) string {
	if sqlServer, ok := errors.AsType[mssql.Error](err); ok {
		return sqlServer.Message
	}
	if mySQL, ok := errors.AsType[*mysql.MySQLError](err); ok {
		return mySQL.Message
	}
	return ""
}

// TestRejectedTellsARowFromTheDatabase hands Rejected errors of both
// engines. One that faults the row is ErrRejected with the server's
// message; any other comes back as it was.
func TestRejectedTellsARowFromTheDatabase(t *testing.T) {
	for _, c := range []struct {
		name     string
		err      error
		rejected bool
	}{
		{"sqlserver null", mssql.Error{Number: 515,
			Message: "no NULL"}, true},
		{"sqlserver too long", mssql.Error{Number: 2628,
			Message: "truncated"}, true},
		{"sqlserver old too long", mssql.Error{Number: 8152,
			Message: "truncated"}, true},
		{"sqlserver wrong type", mssql.Error{Number: 8114,
			Message: "convert"}, true},
		{"sqlserver overflow", mssql.Error{Number: 8115,
			Message: "overflow"}, true},
		{"sqlserver unique index", mssql.Error{Number: 2601,
			Message: "duplicate"}, true},
		{"sqlserver unique constraint", mssql.Error{Number: 2627,
			Message: "duplicate"}, true},
		{"sqlserver denied", mssql.Error{Number: 229,
			Message: "denied"}, false},
		{"sqlserver deadlock", mssql.Error{Number: 1205,
			Message: "deadlock"}, false},
		{"mysql null", &mysql.MySQLError{Number: 1048,
			Message: "no NULL"}, true},
		{"mysql too long", &mysql.MySQLError{Number: 1406,
			Message: "too long"}, true},
		{"mysql wrong type", &mysql.MySQLError{Number: 1366,
			Message: "incorrect"}, true},
		{"mysql bad date", &mysql.MySQLError{Number: 1292,
			Message: "incorrect"}, true},
		{"mysql out of range", &mysql.MySQLError{Number: 1264,
			Message: "range"}, true},
		{"mysql duplicate", &mysql.MySQLError{Number: 1062,
			Message: "taken"}, true},
		{"mysql denied", &mysql.MySQLError{Number: 1142,
			Message: "denied"}, false},
		{"no engine's", errors.New("connection reset"), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := Rejected(c.err)
			if errors.Is(got, ErrRejected) != c.rejected {
				t.Fatalf("answered %v; ErrRejected: %t, "+
					"want %t", got, !c.rejected,
					c.rejected)
			}
			if !c.rejected {
				// mssql.Error holds a slice, so it is told by
				// its type and text rather than by ==.
				if fmt.Sprintf("%T %v", got, got) !=
					fmt.Sprintf("%T %v", c.err, c.err) {
					t.Errorf("answered %v, want the "+
						"error as it came", got)
				}
				return
			}
			want := "the table refused the row: "
			if !strings.HasPrefix(got.Error(), want) ||
				!strings.Contains(got.Error(),
					messageOf(c.err)) {
				t.Errorf("answered %q, want %q and the "+
					"server's message", got, want)
			}
		})
	}
}

// TestRejectedFindsAWrappedRefusal wraps a refusal as a caller on the
// way up might. It is still read for its number.
func TestRejectedFindsAWrappedRefusal(t *testing.T) {
	err := fmt.Errorf("insert trx_itm: %w",
		&mysql.MySQLError{Number: 1062, Message: "taken"})
	if !errors.Is(Rejected(err), ErrRejected) {
		t.Errorf("answered %v, want ErrRejected", Rejected(err))
	}
}

// TestRejectedOfNoError is nil, so the result of any statement can be
// handed to it.
func TestRejectedOfNoError(t *testing.T) {
	if err := Rejected(nil); err != nil {
		t.Errorf("answered %v, want nil", err)
	}
}
