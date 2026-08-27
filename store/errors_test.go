package store_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/credo-go/credo/fault"
	"github.com/credo-go/credo/store"
)

type httpStatusError interface {
	error
	HTTPStatus() int
}

type outerFaultError struct {
	kind  fault.Kind
	cause error
}

func (e *outerFaultError) Error() string         { return "outer fault" }
func (e *outerFaultError) FaultKind() fault.Kind { return e.kind }
func (e *outerFaultError) Unwrap() error         { return e.cause }

func TestSentinelErrors_ErrorsIs(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{"ErrNotFound", store.ErrNotFound},
		{"ErrAlreadyExists", store.ErrAlreadyExists},
		{"ErrDuplicate", store.ErrDuplicate},
		{"ErrConstraint", store.ErrConstraint},
		{"ErrSerialization", store.ErrSerialization},
		{"ErrDeadlock", store.ErrDeadlock},
		{"ErrContention", store.ErrContention},
		{"ErrConflict", store.ErrConflict},
		{"ErrTimeout", store.ErrTimeout},
		{"ErrUnavailable", store.ErrUnavailable},
		{"ErrReadOnly", store.ErrReadOnly},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !errors.Is(tt.err, tt.err) {
				t.Errorf("errors.Is(%v, %v) = false, want true", tt.err, tt.err)
			}
		})
	}
}

func TestSentinelErrors_HTTPStatus(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		status int
	}{
		{"ErrNotFound", store.ErrNotFound, 404},
		{"ErrDuplicate", store.ErrDuplicate, 409},
		{"ErrConflict", store.ErrConflict, 409},
		{"ErrTimeout", store.ErrTimeout, 504},
		{"ErrReadOnly", store.ErrReadOnly, 503},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			se, ok := errors.AsType[httpStatusError](tt.err)
			if !ok {
				t.Fatalf("errors.As did not match HTTPStatus interface for %v", tt.err)
			}
			if got := se.HTTPStatus(); got != tt.status {
				t.Errorf("HTTPStatus() = %d, want %d", got, tt.status)
			}
		})
	}
}

func TestSentinelErrors_WrappedPreservesChain(t *testing.T) {
	wrapped := fmt.Errorf("repo: user get: %w", store.ErrNotFound)

	if !errors.Is(wrapped, store.ErrNotFound) {
		t.Error("errors.Is on wrapped error should match ErrNotFound")
	}

	se, ok := errors.AsType[httpStatusError](wrapped)
	if !ok {
		t.Fatal("errors.As should unwrap to find HTTPStatus on wrapped error")
	}
	if got := se.HTTPStatus(); got != 404 {
		t.Errorf("HTTPStatus() = %d, want 404", got)
	}
}

func TestSentinelErrors_ErrorMessage(t *testing.T) {
	tests := []struct {
		err  error
		want string
	}{
		{store.ErrNotFound, "store: record not found"},
		{store.ErrDuplicate, "store: duplicate record"},
		{store.ErrConflict, "store: conflict"},
		{store.ErrTimeout, "store: timeout"},
		{store.ErrReadOnly, "store: read-only"},
	}
	for _, tt := range tests {
		if got := tt.err.Error(); got != tt.want {
			t.Errorf("Error() = %q, want %q", got, tt.want)
		}
	}
}

func TestSentinelErrors_NotEqual(t *testing.T) {
	if errors.Is(store.ErrNotFound, store.ErrDuplicate) {
		t.Error("ErrNotFound should not match ErrDuplicate")
	}
	if errors.Is(store.ErrDuplicate, store.ErrConflict) {
		t.Error("ErrDuplicate should not match ErrConflict")
	}
}

func TestSentinelErrors_SemanticKinds(t *testing.T) {
	tests := []struct {
		err  error
		want store.Kind
	}{
		{store.ErrNotFound, store.KindNotFound},
		{store.ErrAlreadyExists, store.KindAlreadyExists},
		{store.ErrDuplicate, store.KindAlreadyExists},
		{store.ErrConstraint, store.KindConstraint},
		{store.ErrSerialization, store.KindSerialization},
		{store.ErrDeadlock, store.KindDeadlock},
		{store.ErrContention, store.KindContention},
		{store.ErrConflict, store.KindConflict},
		{store.ErrTimeout, store.KindTimeout},
		{store.ErrUnavailable, store.KindUnavailable},
		{store.ErrReadOnly, store.KindReadOnly},
	}

	for _, tt := range tests {
		got, ok := store.KindOf(fmt.Errorf("repo: %w", tt.err))
		if !ok {
			t.Fatalf("KindOf(%v) did not find a store kind", tt.err)
		}
		if got != tt.want {
			t.Errorf("KindOf(%v) = %q, want %q", tt.err, got, tt.want)
		}
	}
}

func TestSentinelErrors_LegacyAliases(t *testing.T) {
	if store.ErrAlreadyExists != store.ErrDuplicate { //nolint:errorlint // Exact alias identity is the contract.
		t.Fatal("ErrDuplicate must remain an alias of ErrAlreadyExists")
	}

	for _, err := range []error{
		store.ErrConstraint,
		store.ErrSerialization,
		store.ErrDeadlock,
		store.ErrContention,
	} {
		if !errors.Is(err, store.ErrConflict) {
			t.Errorf("errors.Is(%v, ErrConflict) = false, want legacy umbrella match", err)
		}
	}
	if errors.Is(store.ErrAlreadyExists, store.ErrConflict) {
		t.Fatal("AlreadyExists must not match legacy ErrConflict")
	}
}

func TestErrorsIs_DoesNotUnwrapTarget(t *testing.T) {
	wrappedTarget := fmt.Errorf("target wrapper: %w", store.ErrConstraint)
	if errors.Is(store.ErrConstraint, wrappedTarget) {
		t.Fatal("errors.Is must not unwrap or reverse-match the target error")
	}
}

func TestIsTransient_DoesNotImplyRetrySafety(t *testing.T) {
	tests := []struct {
		err  error
		want bool
	}{
		{store.ErrAlreadyExists, false},
		{store.ErrConstraint, false},
		{store.ErrSerialization, true},
		{store.ErrDeadlock, true},
		{store.ErrContention, true},
		{store.ErrTimeout, true},
		{store.ErrUnavailable, true},
		{store.ErrReadOnly, false},
	}
	for _, tt := range tests {
		if got := store.IsTransient(fmt.Errorf("wrapped: %w", tt.err)); got != tt.want {
			t.Errorf("IsTransient(%v) = %v, want %v", tt.err, got, tt.want)
		}
	}
}

func TestError_PreservesStructuredMetadataAndCause(t *testing.T) {
	cause := errors.New("driver detail")
	err := &store.Error{
		Kind:       store.KindConstraint,
		Op:         "insert",
		Resource:   "users",
		Constraint: "users_org_id_fkey",
		Code:       "23503",
		Transient:  false,
		Cause:      cause,
	}

	if !errors.Is(err, store.ErrConstraint) || !errors.Is(err, store.ErrConflict) {
		t.Fatal("structured constraint must match exact and legacy sentinels")
	}
	if !errors.Is(err, cause) {
		t.Fatal("structured error must unwrap to the original cause")
	}
	if got, ok := store.KindOf(err); !ok || got != store.KindConstraint {
		t.Fatalf("KindOf(structured) = (%q, %v), want (%q, true)", got, ok, store.KindConstraint)
	}
	if err.Error() != cause.Error() {
		t.Fatalf("Error() = %q, want cause message %q", err.Error(), cause.Error())
	}
}

func TestKindOf_SkipsOuterNonStoreFault(t *testing.T) {
	err := &outerFaultError{kind: fault.KindUnavailable, cause: store.ErrConstraint}

	if got, ok := store.KindOf(err); !ok || got != store.KindConstraint {
		t.Fatalf("store.KindOf = (%q, %v), want inner store constraint", got, ok)
	}
	if got, ok := fault.KindOf(err); !ok || got != fault.KindUnavailable {
		t.Fatalf("fault.KindOf = (%q, %v), want outer transport fault", got, ok)
	}
}

func TestIsTransient_TypedNilStructuredError(t *testing.T) {
	var structured *store.Error
	err := errors.Join(structured, store.ErrDeadlock)
	if got, ok := store.KindOf(err); !ok || got != store.KindDeadlock {
		t.Fatalf("KindOf(typed nil + deadlock) = (%q, %v), want deadlock", got, ok)
	}
	if !store.IsTransient(err) {
		t.Fatal("typed-nil provider must be skipped in favor of deadlock")
	}
}

func TestKindOfAndIsTransient_UseSamePrimaryStoreError(t *testing.T) {
	serialization := &store.Error{
		Kind:      store.KindSerialization,
		Transient: true,
		Cause:     errors.New("serialization"),
	}
	err := errors.Join(store.ErrConstraint, serialization)
	if got, ok := store.KindOf(err); !ok || got != store.KindConstraint {
		t.Fatalf("KindOf = (%q, %v), want first constraint", got, ok)
	}
	if store.IsTransient(err) {
		t.Fatal("IsTransient must follow the same first constraint classification")
	}

	reversed := errors.Join(serialization, store.ErrConstraint)
	if got, ok := store.KindOf(reversed); !ok || got != store.KindSerialization {
		t.Fatalf("KindOf(reversed) = (%q, %v), want first serialization", got, ok)
	}
	if !store.IsTransient(reversed) {
		t.Fatal("IsTransient(reversed) must follow the first serialization classification")
	}
}

func TestWrap_PreservesCauseAndStatus(t *testing.T) {
	original := errors.New("duplicate key value violates unique constraint users_email_key")
	wrapped := store.Wrap(store.ErrDuplicate, original)

	if !errors.Is(wrapped, store.ErrDuplicate) {
		t.Fatal("wrapped error should match ErrDuplicate")
	}
	if !errors.Is(wrapped, original) {
		t.Fatal("wrapped error should preserve original cause")
	}
	if wrapped.Error() != original.Error() {
		t.Fatalf("wrapped error message = %q, want %q", wrapped.Error(), original.Error())
	}

	se, ok := errors.AsType[httpStatusError](wrapped)
	if !ok {
		t.Fatal("wrapped error should expose HTTPStatus")
	}
	if got := se.HTTPStatus(); got != 409 {
		t.Fatalf("HTTPStatus() = %d, want 409", got)
	}
}

func TestWrap_CompatibilityNoOps(t *testing.T) {
	cause := errors.New("cause")
	unsupported := errors.New("unsupported kind")

	if got := store.Wrap(store.ErrConstraint, nil); got != nil {
		t.Fatalf("Wrap(kind, nil) = %v, want nil", got)
	}
	if got := store.Wrap(nil, cause); got != cause { //nolint:errorlint // Wrap must return the exact cause.
		t.Fatalf("Wrap(nil, cause) = %v, want exact cause", got)
	}
	if got := store.Wrap(unsupported, cause); got != cause { //nolint:errorlint // Wrap must return the exact cause.
		t.Fatalf("Wrap(unsupported, cause) = %v, want exact cause", got)
	}
	already := store.Wrap(store.ErrConstraint, cause)
	if got := store.Wrap(store.ErrConstraint, already); got != already { //nolint:errorlint // No-op preserves identity.
		t.Fatalf("Wrap(already classified) = %v, want exact classified error", got)
	}
}
