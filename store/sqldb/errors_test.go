package sqldb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"github.com/credo-go/credo/store"
)

func TestMapError_Nil(t *testing.T) {
	if got := mapError(nil); got != nil {
		t.Errorf("mapError(nil) = %v, want nil", got)
	}
}

func TestMapError_ErrNoRows(t *testing.T) {
	got := mapError(sql.ErrNoRows)
	if !errors.Is(got, store.ErrNotFound) {
		t.Errorf("mapError(sql.ErrNoRows) = %v, want store.ErrNotFound", got)
	}
}

func TestMapError_WrappedErrNoRows(t *testing.T) {
	err := fmt.Errorf("repo: %w", sql.ErrNoRows)
	got := mapError(err)
	if !errors.Is(got, store.ErrNotFound) {
		t.Errorf("mapError(wrapped sql.ErrNoRows) = %v, want store.ErrNotFound", got)
	}
}

func TestMapError_DeadlineExceeded(t *testing.T) {
	got := mapError(context.DeadlineExceeded)
	if !errors.Is(got, store.ErrTimeout) {
		t.Errorf("mapError(DeadlineExceeded) = %v, want store.ErrTimeout", got)
	}
}

// mockSQLStateError simulates a driver error with SQLSTATE code.
type mockSQLStateError struct {
	state string
	msg   string
}

func (e *mockSQLStateError) Error() string    { return e.msg }
func (e *mockSQLStateError) SQLState() string { return e.state }

func TestMapError_PostgresUniqueViolation(t *testing.T) {
	err := &mockSQLStateError{state: "23505", msg: "unique_violation"}
	got := mapError(err)
	if !errors.Is(got, store.ErrDuplicate) {
		t.Errorf("mapError(PG 23505) = %v, want store.ErrDuplicate", got)
	}
	if !errors.Is(got, err) {
		t.Errorf("mapError(PG 23505) should preserve original cause")
	}
	if got.Error() != err.Error() {
		t.Errorf("mapError(PG 23505) message = %q, want %q", got.Error(), err.Error())
	}
}

func TestMapError_PostgresForeignKey(t *testing.T) {
	err := &mockSQLStateError{state: "23503", msg: "foreign_key_violation"}
	got := mapError(err)
	if !errors.Is(got, store.ErrConflict) {
		t.Errorf("mapError(PG 23503) = %v, want store.ErrConflict", got)
	}
}

func TestMapError_PostgresReadOnly(t *testing.T) {
	err := &mockSQLStateError{state: "25006", msg: "read_only_sql_transaction"}
	got := mapError(err)
	if !errors.Is(got, store.ErrReadOnly) {
		t.Errorf("mapError(PG 25006) = %v, want store.ErrReadOnly", got)
	}
}

func TestMapError_SQLStateTable(t *testing.T) {
	tests := []struct {
		state string
		want  error
	}{
		{"23502", store.ErrConflict}, // not_null_violation
		{"40001", store.ErrConflict}, // serialization_failure
		{"40P01", store.ErrConflict}, // deadlock_detected
		{"57014", store.ErrTimeout},  // query_canceled (statement_timeout)
	}
	for _, tt := range tests {
		t.Run(tt.state, func(t *testing.T) {
			err := &mockSQLStateError{state: tt.state, msg: "sqlstate " + tt.state}
			got := mapError(err)
			if !errors.Is(got, tt.want) {
				t.Errorf("mapError(SQLSTATE %s) = %v, want %v", tt.state, got, tt.want)
			}
		})
	}
}

// mockPgCodeError simulates lib/pq error with Code() method.
type mockPgCodeError struct {
	code string
	msg  string
}

func (e *mockPgCodeError) Error() string { return e.msg }
func (e *mockPgCodeError) Code() string  { return e.code }

func TestMapError_LibPqUniqueViolation(t *testing.T) {
	err := &mockPgCodeError{code: "23505", msg: "pq: duplicate key"}
	got := mapError(err)
	if !errors.Is(got, store.ErrDuplicate) {
		t.Errorf("mapError(lib/pq 23505) = %v, want store.ErrDuplicate", got)
	}
}

func TestExtractSQLState_RejectsNonSQLStateCodes(t *testing.T) {
	// Any error with a Code() string method satisfies pgCodeError —
	// non-SQLSTATE codes must not be treated as one.
	tests := []string{"ECONNRESET", "404", "23505x", "2350", "23 05"}
	for _, code := range tests {
		err := &mockPgCodeError{code: code, msg: "some failure"}
		if got := extractSQLState(err); got != "" {
			t.Errorf("extractSQLState(Code()=%q) = %q, want \"\"", code, got)
		}
	}
}

func TestMapError_MySQLDuplicate(t *testing.T) {
	err := fmt.Errorf("Error 1062: Duplicate entry '1' for key 'PRIMARY'")
	got := mapError(err)
	if !errors.Is(got, store.ErrDuplicate) {
		t.Errorf("mapError(MySQL 1062) = %v, want store.ErrDuplicate", got)
	}
}

func TestMapError_MySQLForeignKey(t *testing.T) {
	err := fmt.Errorf("Error 1451: Cannot delete or update a parent row")
	got := mapError(err)
	if !errors.Is(got, store.ErrConflict) {
		t.Errorf("mapError(MySQL 1451) = %v, want store.ErrConflict", got)
	}
}

func TestMapError_StringFallback_UniqueConstraint(t *testing.T) {
	err := fmt.Errorf("unique constraint violation on table users")
	got := mapError(err)
	if !errors.Is(got, store.ErrDuplicate) {
		t.Errorf("mapError(string unique) = %v, want store.ErrDuplicate", got)
	}
}

func TestMapError_StringFallback_ForeignKey(t *testing.T) {
	err := fmt.Errorf("foreign key constraint failed")
	got := mapError(err)
	if !errors.Is(got, store.ErrConflict) {
		t.Errorf("mapError(string foreign key) = %v, want store.ErrConflict", got)
	}
}

func TestMapError_StringFallback_ReadOnly(t *testing.T) {
	err := fmt.Errorf("cannot execute INSERT in a read-only transaction")
	got := mapError(err)
	if !errors.Is(got, store.ErrReadOnly) {
		t.Errorf("mapError(string read-only) = %v, want store.ErrReadOnly", got)
	}
}

func TestMapError_UnmappedPassthrough(t *testing.T) {
	orig := fmt.Errorf("some unknown error")
	got := mapError(orig)
	if got != orig {
		t.Errorf("mapError(unknown) = %v, want original error %v", got, orig)
	}
}

func TestMapError_MySQLDuplicate_WithSQLState(t *testing.T) {
	// Real go-sql-driver/mysql format: "Error 1062 (23000): Duplicate entry '1' for key 'PRIMARY'"
	err := errors.New("Error 1062 (23000): Duplicate entry '1' for key 'PRIMARY'")
	got := mapError(err)
	if !errors.Is(got, store.ErrDuplicate) {
		t.Errorf("mapError(MySQL 1062 with SQLSTATE) = %v, want store.ErrDuplicate", got)
	}
}

func TestMapError_MySQLForeignKey_WithSQLState(t *testing.T) {
	err := errors.New("Error 1451 (23000): Cannot delete or update a parent row: a foreign key constraint fails")
	got := mapError(err)
	if !errors.Is(got, store.ErrConflict) {
		t.Errorf("mapError(MySQL 1451 with SQLSTATE) = %v, want store.ErrConflict", got)
	}
}

func TestMapError_WrappedMySQLError(t *testing.T) {
	// "Deadlock found" matches no string-fallback pattern, so this only maps
	// if the error number is found through the wrap chain.
	inner := errors.New("Error 1213 (40001): Deadlock found when trying to get lock; try restarting transaction")
	err := fmt.Errorf("create order: %w", inner)
	got := mapError(err)
	if !errors.Is(got, store.ErrConflict) {
		t.Errorf("mapError(wrapped MySQL 1213) = %v, want store.ErrConflict", got)
	}
}

func TestMapError_MySQLErrNumTable(t *testing.T) {
	tests := []struct {
		msg  string
		want error
	}{
		{"Error 1048 (23000): Column 'name' cannot be null", store.ErrConflict},
		{"Error 1213 (40001): Deadlock found when trying to get lock", store.ErrConflict},
		{"Error 1205 (HY000): Lock wait timeout exceeded", store.ErrTimeout},
	}
	for _, tt := range tests {
		got := mapError(errors.New(tt.msg))
		if !errors.Is(got, tt.want) {
			t.Errorf("mapError(%q) = %v, want %v", tt.msg, got, tt.want)
		}
	}
}

func TestMapError_ContextCanceled_Passthrough(t *testing.T) {
	// Deliberate decision: cancellation is not a data-access classification.
	got := mapError(context.Canceled)
	if got != context.Canceled {
		t.Errorf("mapError(context.Canceled) = %v, want passthrough", got)
	}
}

func TestExtractMySQLErrNum(t *testing.T) {
	tests := []struct {
		msg  string
		want uint16
	}{
		{"Error 1062: Duplicate entry", 1062},
		{"Error 1062 (23000): Duplicate entry", 1062},
		{"Error 1451: Cannot delete", 1451},
		{"Error 1451 (23000): Cannot delete", 1451},
		{"Error 1452: Cannot add", 1452},
		{"Error 1290: Read-only", 1290},
		{"not a mysql error", 0},
		{"Error abc: bad number", 0},
		{"Error : empty number", 0},
	}
	for _, tt := range tests {
		got := extractMySQLErrNum(errors.New(tt.msg))
		if got != tt.want {
			t.Errorf("extractMySQLErrNum(%q) = %d, want %d", tt.msg, got, tt.want)
		}
	}

	// Wrapped driver error: number must be found through the chain.
	wrapped := fmt.Errorf("db: %w", errors.New("Error 1062 (23000): Duplicate entry"))
	if got := extractMySQLErrNum(wrapped); got != 1062 {
		t.Errorf("extractMySQLErrNum(wrapped) = %d, want 1062", got)
	}
}
