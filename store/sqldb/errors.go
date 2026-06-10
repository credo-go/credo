package sqldb

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/credo-go/credo/store"
)

// mapError maps Bun/driver errors to store.Err* sentinels.
// Unmapped errors pass through unwrapped.
//
// Error mapping is best-effort: driver-specific codes are detected via
// string/code matching without importing driver packages directly.
//
// context.Canceled is deliberately passed through unmapped: a cancelled
// request is not a database timeout, and cancellation is handled at a
// higher level (request pipeline), not as a data-access classification.
func mapError(err error) error {
	if err == nil {
		return nil
	}

	// sql.ErrNoRows → store.ErrNotFound
	if errors.Is(err, sql.ErrNoRows) {
		return store.Wrap(store.ErrNotFound, err)
	}

	// Context deadline exceeded → store.ErrTimeout
	if errors.Is(err, context.DeadlineExceeded) {
		return store.Wrap(store.ErrTimeout, err)
	}

	// Driver-specific error detection via interface and string matching.
	// We avoid importing driver packages by inspecting error strings/codes.

	// Try to extract a SQLSTATE or error code via common driver interfaces.
	code := extractSQLState(err)
	if code != "" {
		return wrapMappedError(mapSQLState(code), err)
	}

	// MySQL error number detection.
	if num := extractMySQLErrNum(err); num > 0 {
		return wrapMappedError(mapMySQLErrNum(num), err)
	}

	// Fallback: string-based detection for drivers without structured errors.
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "unique constraint") ||
		strings.Contains(msg, "duplicate key") ||
		strings.Contains(msg, "unique_violation") ||
		strings.Contains(msg, "duplicate entry"):
		return store.Wrap(store.ErrDuplicate, err)

	case strings.Contains(msg, "foreign key constraint") ||
		strings.Contains(msg, "foreign_key_violation"):
		return store.Wrap(store.ErrConflict, err)

	case strings.Contains(msg, "read-only") ||
		strings.Contains(msg, "readonly") ||
		strings.Contains(msg, "cannot execute") && strings.Contains(msg, "read-only"):
		return store.Wrap(store.ErrReadOnly, err)
	}

	return err
}

// sqlStateError is satisfied by pgx, lib/pq, and other drivers that expose
// SQLSTATE codes via a SQLState() method. It embeds error so it can be used
// with errors.AsType (every error in a chain implements Error, so this does
// not change which values match).
type sqlStateError interface {
	error
	SQLState() string
}

// pgCodeError is satisfied by lib/pq which uses Code instead of SQLState.
type pgCodeError interface {
	error
	Code() string
}

func extractSQLState(err error) string {
	if se, ok := errors.AsType[sqlStateError](err); ok {
		if code := se.SQLState(); isSQLState(code) {
			return code
		}
	}

	// lib/pq uses .Code() instead of .SQLState(). Any error type with a
	// Code() string method satisfies pgCodeError, so the SQLSTATE shape
	// check is what keeps unrelated codes (e.g. "ECONNRESET") out.
	if pe, ok := errors.AsType[pgCodeError](err); ok {
		if code := pe.Code(); isSQLState(code) {
			return code
		}
	}

	return ""
}

// isSQLState reports whether code has the SQLSTATE shape: exactly five
// digits or uppercase letters.
func isSQLState(code string) bool {
	if len(code) != 5 {
		return false
	}
	for i := 0; i < len(code); i++ {
		c := code[i]
		if (c < '0' || c > '9') && (c < 'A' || c > 'Z') {
			return false
		}
	}
	return true
}

func mapSQLState(code string) error {
	switch code {
	// unique_violation
	case "23505":
		return store.ErrDuplicate
	// foreign_key_violation, not_null_violation
	case "23503", "23502":
		return store.ErrConflict
	// serialization_failure, deadlock_detected — transient conflicts,
	// the transaction is safe to retry
	case "40001", "40P01":
		return store.ErrConflict
	// query_canceled — raised by statement_timeout
	case "57014":
		return store.ErrTimeout
	// read_only_sql_transaction
	case "25006":
		return store.ErrReadOnly
	}
	return nil
}

func extractMySQLErrNum(err error) uint16 {
	// The driver error is usually wrapped by the time it gets here
	// (fmt.Errorf("...: %w", ...)), and the "Error NNNN" prefix only
	// appears on the driver error itself — probe every error in the chain.
	for ; err != nil; err = errors.Unwrap(err) {
		if num := parseMySQLErrNum(err.Error()); num > 0 {
			return num
		}
	}
	return 0
}

func parseMySQLErrNum(msg string) uint16 {
	// MySQL error formats:
	//   go-sql-driver/mysql: "Error 1062 (23000): Duplicate entry..."
	//   Simple format:       "Error 1062: Duplicate entry..."
	if !strings.HasPrefix(msg, "Error ") {
		return 0
	}
	// Extract digits immediately after "Error ".
	rest := msg[6:]
	var num uint16
	for i := 0; i < len(rest); i++ {
		c := rest[i]
		if c >= '0' && c <= '9' {
			num = num*10 + uint16(c-'0')
		} else {
			break
		}
	}
	return num
}

func mapMySQLErrNum(num uint16) error {
	switch num {
	case 1062: // Duplicate entry
		return store.ErrDuplicate
	case 1451, 1452: // Foreign key constraint
		return store.ErrConflict
	case 1048: // Column cannot be null
		return store.ErrConflict
	case 1213: // Deadlock found — transaction is safe to retry
		return store.ErrConflict
	case 1205: // Lock wait timeout exceeded
		return store.ErrTimeout
	case 1290: // Read-only server
		return store.ErrReadOnly
	}
	return nil
}

func wrapMappedError(kind error, original error) error {
	if kind == nil {
		return original
	}
	return store.Wrap(kind, original)
}
