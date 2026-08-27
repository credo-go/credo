package sqldb

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"reflect"
	"strconv"
	"strings"

	"github.com/credo-go/credo/fault"
	"github.com/credo-go/credo/store"
)

type errorClassification struct {
	kind      store.Kind
	transient bool
}

func (db *DB) mapError(ctx context.Context, err error) error {
	family := driverFamilyUnknown
	if db != nil {
		family = db.family
	}
	return mapError(ctx, family, err)
}

// mapError classifies a driver error without discarding its original cause.
// SQLSTATE is the primary cross-driver path. A MySQL number wins when both are
// present because it is more specific than broad classes such as 23 or 40001.
// MySQL's strict envelope and SQLite's numeric Code are inspected only for the
// DB's configured family, so arbitrary domain/hook messages are never
// classified by loose substrings.
//
// context.Canceled remains an application cancellation, not a store timeout.
// PostgreSQL query_canceled and SQLite interrupt consult ctx because their
// driver codes alone do not distinguish caller cancellation from a timeout.
func mapError(ctx context.Context, family driverFamily, err error) error {
	if err == nil {
		return nil
	}
	if _, ok := fault.ProviderOf(err); ok {
		return err
	}
	if errors.Is(err, context.Canceled) {
		return err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return wrapMappedError(
			errorClassification{kind: store.KindTimeout, transient: true},
			"",
			err,
		)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return wrapMappedError(
			errorClassification{kind: store.KindNotFound},
			"",
			err,
		)
	}
	if errors.Is(err, driver.ErrBadConn) || errors.Is(err, sql.ErrConnDone) {
		return wrapMappedError(
			errorClassification{kind: store.KindUnavailable, transient: true},
			"",
			err,
		)
	}

	// A MySQL number is more specific than its broad SQLSTATE class (for
	// example 1062 vs class 23, or 1213 vs 40001), so inspect it first for a
	// configured MySQL family.
	if family == driverFamilyMySQL {
		if number := extractMySQLErrNum(err); number > 0 {
			if classification, ok := mapMySQLErrNum(number); ok {
				return wrapMappedError(classification, strconv.FormatUint(uint64(number), 10), err)
			}
		}
	}

	if code := extractSQLState(err); code != "" {
		if code == "57014" {
			return mapPostgresQueryCanceled(ctx, code, err)
		}
		if classification, ok := mapSQLState(code); ok {
			return wrapMappedError(classification, code, err)
		}
	}

	if family == driverFamilySQLite {
		if code, ok := extractSQLiteCode(err); ok {
			if primarySQLiteCode(code) == sqliteInterrupt {
				return mapSQLiteInterrupt(ctx, code, err)
			}
			if classification, ok := mapSQLiteCode(code); ok {
				return wrapMappedError(classification, strconv.Itoa(code), err)
			}
		}
	}

	return err
}

// sqlStateError is satisfied by pgx and other drivers exposing SQLSTATE.
type sqlStateError interface {
	error
	SQLState() string
}

// pgCodeError is satisfied by lib/pq, which exposes Code() string.
type pgCodeError interface {
	error
	Code() string
}

func extractSQLState(err error) string {
	if stateError, ok := errors.AsType[sqlStateError](err); ok && !isNilDynamicValue(stateError) {
		if code := stateError.SQLState(); isSQLState(code) {
			return code
		}
	}
	if codeError, ok := errors.AsType[pgCodeError](err); ok && !isNilDynamicValue(codeError) {
		if code := codeError.Code(); isSQLState(code) {
			return code
		}
	}
	return ""
}

func isSQLState(code string) bool {
	if len(code) != 5 {
		return false
	}
	for i := range len(code) {
		char := code[i]
		if (char < '0' || char > '9') && (char < 'A' || char > 'Z') {
			return false
		}
	}
	return true
}

func mapSQLState(code string) (errorClassification, bool) {
	switch code {
	case "23505":
		return errorClassification{kind: store.KindAlreadyExists}, true
	case "40001":
		return errorClassification{kind: store.KindSerialization, transient: true}, true
	case "40P01":
		return errorClassification{kind: store.KindDeadlock, transient: true}, true
	case "55P03":
		return errorClassification{kind: store.KindContention, transient: true}, true
	case "25006":
		return errorClassification{kind: store.KindReadOnly}, true
	case "57P01", "57P02", "57P03", "53300":
		return errorClassification{kind: store.KindUnavailable, transient: true}, true
	}

	if strings.HasPrefix(code, "23") {
		return errorClassification{kind: store.KindConstraint}, true
	}
	if strings.HasPrefix(code, "08") {
		return errorClassification{kind: store.KindUnavailable, transient: true}, true
	}
	return errorClassification{}, false
}

func mapPostgresQueryCanceled(ctx context.Context, code string, err error) error {
	ctxErr := contextError(ctx)
	switch {
	case errors.Is(ctxErr, context.Canceled):
		return joinContextCause(context.Canceled, err)
	case errors.Is(ctxErr, context.DeadlineExceeded):
		return wrapMappedError(
			errorClassification{kind: store.KindTimeout, transient: true},
			code,
			joinContextCause(context.DeadlineExceeded, err),
		)
	}

	// SQLSTATE 57014 means query_canceled, not necessarily timeout. The
	// structured code makes this narrow message check safe: loose matching is
	// never applied to arbitrary application errors.
	if strings.Contains(strings.ToLower(err.Error()), "statement timeout") {
		return wrapMappedError(
			errorClassification{kind: store.KindTimeout, transient: true},
			code,
			err,
		)
	}
	return err
}

func extractMySQLErrNum(err error) uint16 {
	if err == nil {
		return 0
	}
	if number := parseMySQLErrNum(err.Error()); number > 0 {
		return number
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, child := range joined.Unwrap() {
			if number := extractMySQLErrNum(child); number > 0 {
				return number
			}
		}
		return 0
	}
	return extractMySQLErrNum(errors.Unwrap(err))
}

func parseMySQLErrNum(message string) uint16 {
	if !strings.HasPrefix(message, "Error ") {
		return 0
	}
	rest := message[len("Error "):]
	digitCount := 0
	var number uint32
	for digitCount < len(rest) {
		char := rest[digitCount]
		if char < '0' || char > '9' {
			break
		}
		number = number*10 + uint32(char-'0')
		if number > 65535 {
			return 0
		}
		digitCount++
	}
	if digitCount == 0 {
		return 0
	}

	suffix := rest[digitCount:]
	if strings.HasPrefix(suffix, ":") {
		return uint16(number)
	}
	if len(suffix) < len(" (00000):") || suffix[0] != ' ' || suffix[1] != '(' ||
		suffix[7] != ')' || suffix[8] != ':' || !isSQLState(suffix[2:7]) {
		return 0
	}
	return uint16(number)
}

func mapMySQLErrNum(number uint16) (errorClassification, bool) {
	switch number {
	case 1022, 1062:
		return errorClassification{kind: store.KindAlreadyExists}, true
	case 1048, 1216, 1217, 1451, 1452, 3819:
		return errorClassification{kind: store.KindConstraint}, true
	case 1213:
		return errorClassification{kind: store.KindDeadlock, transient: true}, true
	case 1205, 3572:
		return errorClassification{kind: store.KindContention, transient: true}, true
	case 1792, 1836:
		return errorClassification{kind: store.KindReadOnly}, true
	case 1040:
		return errorClassification{kind: store.KindUnavailable, transient: true}, true
	default:
		return errorClassification{}, false
	}
}

type sqliteCodeError interface {
	error
	Code() int
}

func extractSQLiteCode(err error) (int, bool) {
	if codeError, ok := errors.AsType[sqliteCodeError](err); ok && !isNilDynamicValue(codeError) {
		return codeError.Code(), true
	}
	return extractDriverSQLiteCode(err)
}

func isNilDynamicValue(value any) bool {
	reflected := reflect.ValueOf(value)
	if !reflected.IsValid() {
		return true
	}
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

// extractDriverSQLiteCode recognizes SQLite driver errors that do not satisfy
// sqliteCodeError: mattn/go-sqlite3 exposes plain Code/ExtendedCode fields,
// while ncruces/go-sqlite3 returns typed codes (Code() ErrorCode,
// ExtendedCode() ExtendedErrorCode). Both are matched by package path via
// reflection so neither driver becomes a dependency.
func extractDriverSQLiteCode(err error) (int, bool) {
	if err == nil {
		return 0, false
	}
	pointer := reflect.ValueOf(err)
	value := pointer
	typeOfError := value.Type()
	if typeOfError.Kind() == reflect.Pointer {
		if value.IsNil() {
			return 0, false
		}
		value = value.Elem()
		typeOfError = typeOfError.Elem()
	}
	if typeOfError.Name() == "Error" {
		switch typeOfError.PkgPath() {
		case "github.com/mattn/go-sqlite3":
			return extractSQLiteCodeFields(value)
		case "github.com/ncruces/go-sqlite3":
			return extractSQLiteCodeMethods(pointer)
		}
	}

	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, child := range joined.Unwrap() {
			if code, found := extractDriverSQLiteCode(child); found {
				return code, true
			}
		}
		return 0, false
	}
	return extractDriverSQLiteCode(errors.Unwrap(err))
}

func extractSQLiteCodeFields(value reflect.Value) (int, bool) {
	if value.Kind() != reflect.Struct {
		return 0, false
	}
	for _, fieldName := range []string{"ExtendedCode", "Code"} {
		field := value.FieldByName(fieldName)
		if field.IsValid() && field.CanInt() {
			code := int(field.Int())
			if code != 0 {
				return code, true
			}
		}
	}
	return 0, false
}

// extractSQLiteCodeMethods reads nullary ExtendedCode/Code methods whose
// result is any integer kind, covering drivers whose codes are named integer
// types rather than int. ExtendedCode is preferred because it carries the
// specific constraint variant.
func extractSQLiteCodeMethods(value reflect.Value) (int, bool) {
	for _, methodName := range []string{"ExtendedCode", "Code"} {
		method := value.MethodByName(methodName)
		if !method.IsValid() {
			continue
		}
		methodType := method.Type()
		if methodType.NumIn() != 0 || methodType.NumOut() != 1 {
			continue
		}
		result := method.Call(nil)[0]
		var code int
		switch {
		case result.CanInt():
			code = int(result.Int())
		case result.CanUint():
			code = int(result.Uint())
		default:
			continue
		}
		if code != 0 {
			return code, true
		}
	}
	return 0, false
}

const (
	sqliteBusy       = 5
	sqliteLocked     = 6
	sqliteReadOnly   = 8
	sqliteInterrupt  = 9
	sqliteIOError    = 10
	sqliteCantOpen   = 14
	sqliteConstraint = 19

	sqliteConstraintCheck      = 275
	sqliteBusyRecovery         = 261
	sqliteLockedSharedCache    = 262
	sqliteBusySnapshot         = 517
	sqliteLockedVirtualTable   = 518
	sqliteBusyTimeout          = 773
	sqliteConstraintForeignKey = 787
	sqliteReadOnlyRollback     = 776
	sqliteReadOnlyDirectory    = 1544
	sqliteConstraintNotNull    = 1299
	sqliteConstraintPrimaryKey = 1555
	sqliteConstraintUnique     = 2067
	sqliteConstraintRowID      = 2579
)

func primarySQLiteCode(code int) int {
	return code & 0xff
}

func mapSQLiteCode(code int) (errorClassification, bool) {
	switch code {
	case sqliteConstraintUnique, sqliteConstraintPrimaryKey, sqliteConstraintRowID:
		return errorClassification{kind: store.KindAlreadyExists}, true
	case sqliteConstraintCheck, sqliteConstraintForeignKey, sqliteConstraintNotNull:
		return errorClassification{kind: store.KindConstraint}, true
	case sqliteBusySnapshot:
		return errorClassification{kind: store.KindSerialization, transient: true}, true
	case sqliteBusy, sqliteBusyRecovery, sqliteBusyTimeout,
		sqliteLocked, sqliteLockedSharedCache, sqliteLockedVirtualTable:
		return errorClassification{kind: store.KindContention, transient: true}, true
	case sqliteReadOnlyRollback, sqliteReadOnlyDirectory:
		return errorClassification{kind: store.KindReadOnly}, true
	}

	switch primarySQLiteCode(code) {
	case sqliteConstraint:
		return errorClassification{kind: store.KindConstraint}, true
	case sqliteReadOnly:
		return errorClassification{kind: store.KindReadOnly}, true
	case sqliteIOError, sqliteCantOpen:
		return errorClassification{kind: store.KindUnavailable, transient: true}, true
	default:
		return errorClassification{}, false
	}
}

func mapSQLiteInterrupt(ctx context.Context, code int, err error) error {
	ctxErr := contextError(ctx)
	switch {
	case errors.Is(ctxErr, context.Canceled):
		return joinContextCause(context.Canceled, err)
	case errors.Is(ctxErr, context.DeadlineExceeded):
		return wrapMappedError(
			errorClassification{kind: store.KindTimeout, transient: true},
			strconv.Itoa(code),
			joinContextCause(context.DeadlineExceeded, err),
		)
	default:
		return err
	}
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}

func joinContextCause(contextErr error, driverErr error) error {
	if errors.Is(driverErr, contextErr) {
		return driverErr
	}
	return errors.Join(contextErr, driverErr)
}

func wrapMappedError(classification errorClassification, code string, cause error) error {
	if classification.kind == store.KindUnknown {
		return cause
	}
	return &store.Error{
		Kind:      classification.kind,
		Code:      code,
		Transient: classification.transient,
		Cause:     cause,
	}
}
