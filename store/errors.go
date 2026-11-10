package store

import (
	"errors"
	"reflect"

	"github.com/credo-go/credo/fault"
)

// Error is a structured, transport-neutral data-access error.
//
// Code, Constraint, Resource, and Cause are internal diagnostic metadata. The
// default HTTP renderer classifies only Kind and never serializes these fields.
// Transient describes whether the condition may clear; it does not guarantee
// that replaying the operation or transaction is safe.
type Error struct {
	Kind       fault.Kind
	Op         string
	Resource   string
	Constraint string
	Code       string
	Transient  bool
	Cause      error
}

// Error returns the original cause message when available.
func (e *Error) Error() string {
	if e == nil {
		return "store: <nil>"
	}
	if e.Cause != nil {
		return e.Cause.Error()
	}
	return kindMessage(e.Kind)
}

// Unwrap returns the original driver or application cause.
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// FaultKind exposes the semantic category to transport policy packages.
func (e *Error) FaultKind() fault.Kind {
	if e == nil {
		return fault.KindUnknown
	}
	return e.Kind
}

func (e *Error) storeKind() fault.Kind {
	if e == nil {
		return fault.KindUnknown
	}
	return e.Kind
}

// Is matches the sentinel of the error's exact kind. Cause matching is handled
// by Unwrap.
func (e *Error) Is(target error) bool {
	if e == nil {
		return false
	}
	targetKind, ok := directStoreKind(target)
	return ok && e.Kind == targetKind
}

type kindError struct {
	kind    fault.Kind
	message string
}

func (e *kindError) Error() string         { return e.message }
func (e *kindError) FaultKind() fault.Kind { return e.kind }
func (e *kindError) storeKind() fault.Kind { return e.kind }
func (e *kindError) Is(target error) bool {
	targetKind, ok := directStoreKind(target)
	return ok && e.kind == targetKind
}

// Sentinel errors for data access operations.
var (
	// ErrNotFound indicates that the requested record does not exist.
	ErrNotFound error = newKindError(fault.KindNotFound, "store: record not found")

	// ErrAlreadyExists indicates a unique or primary-key violation.
	ErrAlreadyExists error = newKindError(fault.KindAlreadyExists, "store: duplicate record")

	// ErrConstraint indicates a persistent integrity constraint violation other
	// than a duplicate, such as foreign-key, not-null, or check failure.
	ErrConstraint error = newKindError(fault.KindConstraint, "store: constraint violation")

	// ErrSerialization indicates that transaction serialization failed.
	ErrSerialization error = newKindError(fault.KindSerialization, "store: serialization failure")

	// ErrDeadlock indicates that the database detected a deadlock.
	ErrDeadlock error = newKindError(fault.KindDeadlock, "store: deadlock")

	// ErrContention indicates a transient lock/busy conflict whose safe retry
	// scope is not implied by this sentinel.
	ErrContention error = newKindError(fault.KindContention, "store: contention")

	// ErrTimeout indicates that a database operation exceeded a verified
	// deadline or statement timeout.
	ErrTimeout error = newKindError(fault.KindTimeout, "store: timeout")

	// ErrUnavailable indicates that a database connection or service is
	// temporarily unavailable.
	ErrUnavailable error = newKindError(fault.KindUnavailable, "store: unavailable")

	// ErrReadOnly indicates that a write was attempted against a read-only
	// transaction or server.
	ErrReadOnly error = newKindError(fault.KindReadOnly, "store: read-only")
)

// KindOf finds the first recognized store kind in err's unwrap/join tree. The
// kinds are package fault's ([fault.KindNotFound] and the rest).
func KindOf(err error) (fault.Kind, bool) {
	provider, ok := firstStoreProvider(err)
	if !ok {
		return fault.KindUnknown, false
	}
	kind := provider.storeKind()
	return kind, true
}

// IsTransient reports whether the primary store classification describes a
// condition that may clear. It never means an operation, transaction callback,
// or externally visible side effect is safe to retry.
func IsTransient(err error) bool {
	provider, ok := firstStoreProvider(err)
	if !ok {
		return false
	}
	if structured, ok := provider.(*Error); ok {
		return structured.Transient
	}
	return defaultTransient(provider.storeKind())
}

// Wrap preserves cause while classifying it with a store sentinel. Nil causes
// return nil. Unsupported kinds and an already matching cause are returned
// unchanged for compatibility.
func Wrap(kind error, cause error) error {
	if cause == nil {
		return nil
	}
	if kind == nil || errors.Is(cause, kind) {
		return cause
	}
	semanticKind, ok := KindOf(kind)
	if !ok {
		return cause
	}
	return &Error{
		Kind:      semanticKind,
		Transient: defaultTransient(semanticKind),
		Cause:     cause,
	}
}

func newKindError(kind fault.Kind, message string) *kindError {
	return &kindError{kind: kind, message: message}
}

type storeKindProvider interface {
	error
	storeKind() fault.Kind
}

func directStoreKind(err error) (fault.Kind, bool) {
	// Wrap's compatibility no-op depends on the outer error itself already being
	// classified; a nested provider must not satisfy this check.
	provider, ok := err.(storeKindProvider) //nolint:errorlint
	if !ok || isNilStoreProvider(provider) || !isStoreKind(provider.storeKind()) {
		return fault.KindUnknown, false
	}
	return provider.storeKind(), true
}

func firstStoreProvider(err error) (storeKindProvider, bool) {
	if err == nil {
		return nil, false
	}
	// Inspect only this node so explicit recursion can skip typed nil and invalid
	// providers while preserving join order.
	if provider, ok := err.(storeKindProvider); ok && !isNilStoreProvider(provider) { //nolint:errorlint
		if isStoreKind(provider.storeKind()) {
			return provider, true
		}
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, child := range joined.Unwrap() {
			if provider, found := firstStoreProvider(child); found {
				return provider, true
			}
		}
		return nil, false
	}
	return firstStoreProvider(errors.Unwrap(err))
}

func isNilStoreProvider(provider storeKindProvider) bool {
	value := reflect.ValueOf(provider)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func isStoreKind(kind fault.Kind) bool {
	switch kind {
	case fault.KindNotFound,
		fault.KindAlreadyExists,
		fault.KindConstraint,
		fault.KindSerialization,
		fault.KindDeadlock,
		fault.KindContention,
		fault.KindConflict,
		fault.KindTimeout,
		fault.KindUnavailable,
		fault.KindReadOnly:
		return true
	default:
		return false
	}
}

func defaultTransient(kind fault.Kind) bool {
	switch kind {
	case fault.KindSerialization, fault.KindDeadlock, fault.KindContention, fault.KindTimeout, fault.KindUnavailable:
		return true
	default:
		return false
	}
}

func kindMessage(kind fault.Kind) string {
	switch kind {
	case fault.KindNotFound:
		return "store: record not found"
	case fault.KindAlreadyExists:
		return "store: duplicate record"
	case fault.KindConstraint:
		return "store: constraint violation"
	case fault.KindSerialization:
		return "store: serialization failure"
	case fault.KindDeadlock:
		return "store: deadlock"
	case fault.KindContention:
		return "store: contention"
	case fault.KindConflict:
		return "store: conflict"
	case fault.KindTimeout:
		return "store: timeout"
	case fault.KindUnavailable:
		return "store: unavailable"
	case fault.KindReadOnly:
		return "store: read-only"
	default:
		return "store: unknown error"
	}
}
