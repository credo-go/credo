package sqldb

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/credo-go/credo/fault"
	"github.com/credo-go/credo/store"
)

type mockOuterFault struct {
	kind  fault.Kind
	cause error
}

func (e *mockOuterFault) Error() string         { return "outer semantic fault" }
func (e *mockOuterFault) FaultKind() fault.Kind { return e.kind }
func (e *mockOuterFault) Unwrap() error         { return e.cause }

func TestMapError_Baseline(t *testing.T) {
	if got := mapError(t.Context(), driverFamilyUnknown, nil); got != nil {
		t.Fatalf("mapError(nil) = %v, want nil", got)
	}

	tests := []struct {
		name      string
		err       error
		want      error
		kind      store.Kind
		transient bool
	}{
		{"no rows", sql.ErrNoRows, store.ErrNotFound, store.KindNotFound, false},
		{"wrapped no rows", fmt.Errorf("repo: %w", sql.ErrNoRows), store.ErrNotFound, store.KindNotFound, false},
		{"deadline", context.DeadlineExceeded, store.ErrTimeout, store.KindTimeout, true},
		{"wrapped deadline", fmt.Errorf("query: %w", context.DeadlineExceeded), store.ErrTimeout, store.KindTimeout, true},
		{"bad connection", driver.ErrBadConn, store.ErrUnavailable, store.KindUnavailable, true},
		{"closed sql connection", sql.ErrConnDone, store.ErrUnavailable, store.KindUnavailable, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mapError(t.Context(), driverFamilyUnknown, tt.err)
			assertMappedError(t, got, tt.want, tt.kind, "", tt.transient, tt.err)
		})
	}
}

func TestMapError_ContextCanceledPassesThrough(t *testing.T) {
	for _, err := range []error{
		context.Canceled,
		fmt.Errorf("query: %w", context.Canceled),
	} {
		got := mapError(t.Context(), driverFamilyUnknown, err)
		if got != err {
			t.Fatalf("mapError(%v) changed cancellation identity: %v", err, got)
		}
		if errors.Is(got, store.ErrTimeout) {
			t.Fatal("context cancellation must not become a store timeout")
		}
	}
}

// mockSQLStateError simulates pgx and other drivers exposing SQLState.
type mockSQLStateError struct {
	state string
	msg   string
}

func (e *mockSQLStateError) Error() string    { return e.msg }
func (e *mockSQLStateError) SQLState() string { return e.state }

func TestMapError_PostgresSQLStateTable(t *testing.T) {
	tests := []struct {
		state     string
		want      error
		kind      store.Kind
		transient bool
	}{
		{"23505", store.ErrAlreadyExists, store.KindAlreadyExists, false},
		{"23502", store.ErrConstraint, store.KindConstraint, false},
		{"23503", store.ErrConstraint, store.KindConstraint, false},
		{"23514", store.ErrConstraint, store.KindConstraint, false},
		{"23P01", store.ErrConstraint, store.KindConstraint, false},
		{"40001", store.ErrSerialization, store.KindSerialization, true},
		{"40P01", store.ErrDeadlock, store.KindDeadlock, true},
		{"55P03", store.ErrContention, store.KindContention, true},
		{"25006", store.ErrReadOnly, store.KindReadOnly, false},
		{"08006", store.ErrUnavailable, store.KindUnavailable, true},
		{"57P01", store.ErrUnavailable, store.KindUnavailable, true},
		{"57P03", store.ErrUnavailable, store.KindUnavailable, true},
		{"53300", store.ErrUnavailable, store.KindUnavailable, true},
	}

	for _, tt := range tests {
		t.Run(tt.state, func(t *testing.T) {
			cause := &mockSQLStateError{state: tt.state, msg: "sqlstate " + tt.state}
			got := mapError(t.Context(), driverFamilyPostgres, cause)
			assertMappedError(t, got, tt.want, tt.kind, tt.state, tt.transient, cause)
		})
	}
}

func TestMapError_PostgresQueryCanceledUsesContext(t *testing.T) {
	t.Run("active ambiguous", func(t *testing.T) {
		cause := &mockSQLStateError{state: "57014", msg: "canceling statement due to user request"}
		if got := mapError(t.Context(), driverFamilyPostgres, cause); got != cause {
			t.Fatalf("ambiguous 57014 = %v, want exact passthrough", got)
		}
	})

	t.Run("active statement timeout", func(t *testing.T) {
		cause := &mockSQLStateError{state: "57014", msg: "canceling statement due to statement timeout"}
		got := mapError(t.Context(), driverFamilyPostgres, cause)
		assertMappedError(t, got, store.ErrTimeout, store.KindTimeout, "57014", true, cause)
	})

	t.Run("canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		cause := &mockSQLStateError{state: "57014", msg: "query canceled"}
		got := mapError(ctx, driverFamilyPostgres, cause)
		if !errors.Is(got, context.Canceled) || !errors.Is(got, cause) {
			t.Fatalf("canceled 57014 must preserve context and driver causes: %v", got)
		}
		if errors.Is(got, store.ErrTimeout) {
			t.Fatal("canceled 57014 must not become timeout")
		}
	})

	t.Run("deadline", func(t *testing.T) {
		ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
		defer cancel()
		cause := &mockSQLStateError{state: "57014", msg: "query canceled"}
		got := mapError(ctx, driverFamilyPostgres, cause)
		assertMappedError(t, got, store.ErrTimeout, store.KindTimeout, "57014", true, cause)
		if !errors.Is(got, context.DeadlineExceeded) {
			t.Fatal("deadline 57014 must preserve context deadline cause")
		}
	})

	t.Run("canceled context does not override constraint", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		cause := &mockSQLStateError{state: "23505", msg: "unique violation"}
		got := mapError(ctx, driverFamilyPostgres, cause)
		assertMappedError(t, got, store.ErrAlreadyExists, store.KindAlreadyExists, "23505", false, cause)
	})
}

// mockPgCodeError simulates lib/pq's Code() string method.
type mockPgCodeError struct {
	code string
	msg  string
}

func (e *mockPgCodeError) Error() string { return e.msg }
func (e *mockPgCodeError) Code() string  { return e.code }

func TestExtractSQLState_LibPQAndShapeValidation(t *testing.T) {
	cause := &mockPgCodeError{code: "23505", msg: "pq: duplicate key"}
	got := mapError(t.Context(), driverFamilyPostgres, cause)
	assertMappedError(t, got, store.ErrAlreadyExists, store.KindAlreadyExists, "23505", false, cause)

	for _, code := range []string{"ECONNRESET", "404", "23505x", "2350", "23 05"} {
		err := &mockPgCodeError{code: code, msg: "some failure"}
		if got := extractSQLState(err); got != "" {
			t.Errorf("extractSQLState(Code()=%q) = %q, want empty", code, got)
		}
	}
}

func TestMapError_MySQLNumberTable(t *testing.T) {
	tests := []struct {
		number    uint16
		want      error
		kind      store.Kind
		transient bool
	}{
		{1022, store.ErrAlreadyExists, store.KindAlreadyExists, false},
		{1062, store.ErrAlreadyExists, store.KindAlreadyExists, false},
		{1048, store.ErrConstraint, store.KindConstraint, false},
		{1216, store.ErrConstraint, store.KindConstraint, false},
		{1217, store.ErrConstraint, store.KindConstraint, false},
		{1451, store.ErrConstraint, store.KindConstraint, false},
		{1452, store.ErrConstraint, store.KindConstraint, false},
		{3819, store.ErrConstraint, store.KindConstraint, false},
		{1213, store.ErrDeadlock, store.KindDeadlock, true},
		{1205, store.ErrContention, store.KindContention, true},
		{3572, store.ErrContention, store.KindContention, true},
		{1792, store.ErrReadOnly, store.KindReadOnly, false},
		{1836, store.ErrReadOnly, store.KindReadOnly, false},
		{1040, store.ErrUnavailable, store.KindUnavailable, true},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprint(tt.number), func(t *testing.T) {
			cause := errors.New(fmt.Sprintf("Error %d (HY000): driver failure", tt.number))
			got := mapError(t.Context(), driverFamilyMySQL, cause)
			assertMappedError(t, got, tt.want, tt.kind, fmt.Sprint(tt.number), tt.transient, cause)
			if tt.number == 1205 && errors.Is(got, store.ErrTimeout) {
				t.Fatal("MySQL 1205 lock wait must not become request/statement timeout")
			}
		})
	}
}

func TestMapError_MySQLNumberBeatsBroadSQLState(t *testing.T) {
	tests := []struct {
		state string
		msg   string
		want  error
		kind  store.Kind
		code  string
	}{
		{"23000", "Error 1062 (23000): Duplicate entry", store.ErrAlreadyExists, store.KindAlreadyExists, "1062"},
		{"40001", "Error 1213 (40001): Deadlock found", store.ErrDeadlock, store.KindDeadlock, "1213"},
	}
	for _, tt := range tests {
		cause := &mockSQLStateError{state: tt.state, msg: tt.msg}
		got := mapError(t.Context(), driverFamilyMySQL, cause)
		assertMappedError(t, got, tt.want, tt.kind, tt.code, tt.kind == store.KindDeadlock, cause)
	}
}

func TestMapError_MySQL1290IsNotUnconditionallyReadOnly(t *testing.T) {
	cause := errors.New("Error 1290 (HY000): The MySQL server is running with the --secure-file-priv option")
	if got := mapError(t.Context(), driverFamilyMySQL, cause); got != cause {
		t.Fatalf("MySQL 1290 = %v, want exact passthrough", got)
	}
}

func TestParseMySQLErrNum_StrictEnvelope(t *testing.T) {
	tests := []struct {
		msg  string
		want uint16
	}{
		{"Error 1062: Duplicate entry", 1062},
		{"Error 1062 (23000): Duplicate entry", 1062},
		{"Error 1205 (HY000): Lock wait timeout exceeded", 1205},
		{"Error 1062 bananas", 0},
		{"Error 1062", 0},
		{"Error abc: bad number", 0},
		{"prefix Error 1062: Duplicate entry", 0},
	}
	for _, tt := range tests {
		if got := parseMySQLErrNum(tt.msg); got != tt.want {
			t.Errorf("parseMySQLErrNum(%q) = %d, want %d", tt.msg, got, tt.want)
		}
	}

	wrapped := fmt.Errorf("db: %w", errors.New("Error 1062 (23000): Duplicate entry"))
	if got := extractMySQLErrNum(wrapped); got != 1062 {
		t.Fatalf("extractMySQLErrNum(wrapped) = %d, want 1062", got)
	}
	joined := errors.Join(errors.New("outer"), wrapped)
	if got := extractMySQLErrNum(joined); got != 1062 {
		t.Fatalf("extractMySQLErrNum(joined) = %d, want 1062", got)
	}
}

type mockSQLiteError struct {
	code int
}

func (e *mockSQLiteError) Error() string { return fmt.Sprintf("sqlite code %d", e.code) }
func (e *mockSQLiteError) Code() int     { return e.code }

type mockMattnSQLiteShape struct {
	Code         int
	ExtendedCode int
}

func TestExtractSQLiteCodeFields_PrefersExtendedCode(t *testing.T) {
	tests := []struct {
		value mockMattnSQLiteShape
		want  int
		ok    bool
	}{
		{value: mockMattnSQLiteShape{Code: 19, ExtendedCode: 2067}, want: 2067, ok: true},
		{value: mockMattnSQLiteShape{Code: 19}, want: 19, ok: true},
		{value: mockMattnSQLiteShape{}, want: 0, ok: false},
	}
	for _, tt := range tests {
		got, ok := extractSQLiteCodeFields(reflect.ValueOf(tt.value))
		if got != tt.want || ok != tt.ok {
			t.Errorf("extractSQLiteCodeFields(%+v) = (%d, %v), want (%d, %v)", tt.value, got, ok, tt.want, tt.ok)
		}
	}
}

func TestMapError_SQLiteCodeTable(t *testing.T) {
	tests := []struct {
		code      int
		want      error
		kind      store.Kind
		transient bool
	}{
		{2067, store.ErrAlreadyExists, store.KindAlreadyExists, false},
		{1555, store.ErrAlreadyExists, store.KindAlreadyExists, false},
		{2579, store.ErrAlreadyExists, store.KindAlreadyExists, false},
		{19, store.ErrConstraint, store.KindConstraint, false},
		{275, store.ErrConstraint, store.KindConstraint, false},
		{787, store.ErrConstraint, store.KindConstraint, false},
		{1299, store.ErrConstraint, store.KindConstraint, false},
		{517, store.ErrSerialization, store.KindSerialization, true},
		{5, store.ErrContention, store.KindContention, true},
		{261, store.ErrContention, store.KindContention, true},
		{773, store.ErrContention, store.KindContention, true},
		{6, store.ErrContention, store.KindContention, true},
		{262, store.ErrContention, store.KindContention, true},
		{518, store.ErrContention, store.KindContention, true},
		{8, store.ErrReadOnly, store.KindReadOnly, false},
		{1032, store.ErrReadOnly, store.KindReadOnly, false},
		{10, store.ErrUnavailable, store.KindUnavailable, true},
		{14, store.ErrUnavailable, store.KindUnavailable, true},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprint(tt.code), func(t *testing.T) {
			cause := &mockSQLiteError{code: tt.code}
			got := mapError(t.Context(), driverFamilySQLite, cause)
			assertMappedError(t, got, tt.want, tt.kind, fmt.Sprint(tt.code), tt.transient, cause)
		})
	}
}

func TestMapError_SQLiteInterruptUsesContext(t *testing.T) {
	t.Run("active", func(t *testing.T) {
		cause := &mockSQLiteError{code: 9}
		if got := mapError(t.Context(), driverFamilySQLite, cause); got != cause {
			t.Fatalf("active SQLITE_INTERRUPT = %v, want passthrough", got)
		}
	})

	t.Run("canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		cause := &mockSQLiteError{code: 9}
		got := mapError(ctx, driverFamilySQLite, cause)
		if !errors.Is(got, context.Canceled) || !errors.Is(got, cause) {
			t.Fatalf("canceled SQLITE_INTERRUPT lost causes: %v", got)
		}
		if errors.Is(got, store.ErrTimeout) {
			t.Fatal("canceled SQLITE_INTERRUPT must not become timeout")
		}
	})

	t.Run("deadline", func(t *testing.T) {
		ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
		defer cancel()
		cause := &mockSQLiteError{code: 9}
		got := mapError(ctx, driverFamilySQLite, cause)
		assertMappedError(t, got, store.ErrTimeout, store.KindTimeout, "9", true, cause)
		if !errors.Is(got, context.DeadlineExceeded) {
			t.Fatal("deadline SQLITE_INTERRUPT must preserve context deadline cause")
		}
	})
}

func TestMapError_DomainMessagesPassThrough(t *testing.T) {
	for _, message := range []string{
		"duplicate key validation failed",
		"foreign key business rule rejected",
		"read-only domain state",
		"unique constraint in aggregate",
	} {
		cause := errors.New(message)
		if got := mapError(t.Context(), driverFamilyUnknown, cause); got != cause {
			t.Errorf("mapError(%q) = %v, want exact passthrough", message, got)
		}
	}
}

func TestMapError_UnknownAndAlreadyClassifiedPreserveIdentity(t *testing.T) {
	unknown := errors.New("some unknown error")
	if got := mapError(t.Context(), driverFamilyUnknown, unknown); got != unknown {
		t.Fatalf("unknown = %v, want exact identity", got)
	}

	classified := store.Wrap(store.ErrConstraint, errors.New("driver"))
	if got := mapError(t.Context(), driverFamilyPostgres, classified); got != classified {
		t.Fatalf("already classified = %v, want exact identity", got)
	}

	outer := &mockOuterFault{
		kind:  fault.KindNotFound,
		cause: &mockSQLStateError{state: "23505", msg: "unique violation"},
	}
	if got := mapError(t.Context(), driverFamilyPostgres, outer); got != outer {
		t.Fatalf("outer semantic fault = %v, want exact identity", got)
	}
}

func assertMappedError(
	t *testing.T,
	got error,
	wantSentinel error,
	wantKind store.Kind,
	wantCode string,
	wantTransient bool,
	wantCause error,
) {
	t.Helper()
	if !errors.Is(got, wantSentinel) {
		t.Fatalf("mapped error = %v, want sentinel %v", got, wantSentinel)
	}
	if !errors.Is(got, wantCause) {
		t.Fatalf("mapped error = %v, want original cause %v", got, wantCause)
	}
	kind, ok := store.KindOf(got)
	if !ok || kind != wantKind {
		t.Fatalf("KindOf(mapped) = (%q, %v), want (%q, true)", kind, ok, wantKind)
	}
	structured, ok := errors.AsType[*store.Error](got)
	if !ok {
		t.Fatalf("mapped error %T is not *store.Error", got)
	}
	if structured.Code != wantCode {
		t.Errorf("Code = %q, want %q", structured.Code, wantCode)
	}
	if structured.Transient != wantTransient {
		t.Errorf("Transient = %v, want %v", structured.Transient, wantTransient)
	}
	if gotTransient := store.IsTransient(got); gotTransient != wantTransient {
		t.Errorf("IsTransient = %v, want %v", gotTransient, wantTransient)
	}
}
