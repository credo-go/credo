package store

import (
	"errors"
	"reflect"

	"github.com/credo-go/credo/fault"
)

// Kind is the transport-neutral semantic category of a store error.
type Kind = fault.Kind

// Store error kinds. They are aliases of the shared transport-neutral kinds
// so Credo's HTTP policy and future gRPC policy can classify them without
// importing store and creating a package cycle.
const (
	KindUnknown       Kind = fault.KindUnknown
	KindNotFound      Kind = fault.KindNotFound
	KindAlreadyExists Kind = fault.KindAlreadyExists
	KindConstraint    Kind = fault.KindConstraint
	KindSerialization Kind = fault.KindSerialization
	KindDeadlock      Kind = fault.KindDeadlock
	KindContention    Kind = fault.KindContention
	KindConflict      Kind = fault.KindConflict
	KindTimeout       Kind = fault.KindTimeout
	KindUnavailable   Kind = fault.KindUnavailable
	KindReadOnly      Kind = fault.KindReadOnly
)

// Error is a structured, transport-neutral data-access error.
//
// Code, Constraint, Resource, and Cause are internal diagnostic metadata. The
// default HTTP renderer classifies only Kind and never serializes these fields.
// Transient describes whether the condition may clear; it does not guarantee
// that replaying the operation or transaction is safe.
type Error struct {
	Kind       Kind
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

func (e *Error) storeKind() Kind {
	if e == nil {
		return KindUnknown
	}
	return e.Kind
}

// Is matches the exact semantic sentinel and the deprecated ErrConflict
// umbrella where applicable. Cause matching is handled by Unwrap.
func (e *Error) Is(target error) bool {
	if e == nil {
		return false
	}
	targetKind, ok := directStoreKind(target)
	return ok && kindsMatch(e.Kind, targetKind)
}

// HTTPStatus returns the legacy default HTTP mapping for this error.
//
// Deprecated: inspect [KindOf] and apply transport policy at the application
// boundary. Credo's root error pipeline now uses FaultKind before this bridge.
func (e *Error) HTTPStatus() int {
	if e == nil {
		return 500
	}
	return legacyHTTPStatus(e.Kind)
}

type kindError struct {
	kind    Kind
	message string
}

func (e *kindError) Error() string         { return e.message }
func (e *kindError) FaultKind() fault.Kind { return e.kind }
func (e *kindError) storeKind() Kind       { return e.kind }
func (e *kindError) Is(target error) bool {
	targetKind, ok := directStoreKind(target)
	return ok && kindsMatch(e.kind, targetKind)
}

// HTTPStatus returns the sentinel's legacy default HTTP mapping.
//
// Deprecated: inspect [KindOf] and apply transport policy at the application
// boundary. Credo's root error pipeline now uses FaultKind before this bridge.
func (e *kindError) HTTPStatus() int { return legacyHTTPStatus(e.kind) }

// Sentinel errors for data access operations.
var (
	// ErrNotFound indicates that the requested record does not exist.
	ErrNotFound error = newKindError(KindNotFound, "store: record not found")

	// ErrAlreadyExists indicates a unique or primary-key violation.
	ErrAlreadyExists error = newKindError(KindAlreadyExists, "store: duplicate record")

	// ErrDuplicate is the compatibility name for [ErrAlreadyExists].
	//
	// Deprecated: use ErrAlreadyExists or inspect [KindAlreadyExists].
	ErrDuplicate = ErrAlreadyExists

	// ErrConstraint indicates a persistent integrity constraint violation other
	// than a duplicate, such as foreign-key, not-null, or check failure.
	ErrConstraint error = newKindError(KindConstraint, "store: constraint violation")

	// ErrSerialization indicates that transaction serialization failed.
	ErrSerialization error = newKindError(KindSerialization, "store: serialization failure")

	// ErrDeadlock indicates that the database detected a deadlock.
	ErrDeadlock error = newKindError(KindDeadlock, "store: deadlock")

	// ErrContention indicates a transient lock/busy conflict whose safe retry
	// scope is not implied by this sentinel.
	ErrContention error = newKindError(KindContention, "store: contention")

	// ErrConflict is the legacy umbrella for constraint and concurrency errors.
	// New classifiers return a more specific sentinel whose errors.Is method
	// still matches ErrConflict.
	//
	// Deprecated: inspect ErrConstraint, ErrSerialization, ErrDeadlock, or
	// ErrContention instead.
	ErrConflict error = newKindError(KindConflict, "store: conflict")

	// ErrTimeout indicates that a database operation exceeded a verified
	// deadline or statement timeout.
	ErrTimeout error = newKindError(KindTimeout, "store: timeout")

	// ErrUnavailable indicates that a database connection or service is
	// temporarily unavailable.
	ErrUnavailable error = newKindError(KindUnavailable, "store: unavailable")

	// ErrReadOnly indicates that a write was attempted against a read-only
	// transaction or server.
	ErrReadOnly error = newKindError(KindReadOnly, "store: read-only")
)

// KindOf finds the first recognized store Kind in err's unwrap/join tree.
func KindOf(err error) (Kind, bool) {
	provider, ok := firstStoreProvider(err)
	if !ok {
		return KindUnknown, false
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

func newKindError(kind Kind, message string) *kindError {
	return &kindError{kind: kind, message: message}
}

type storeKindProvider interface {
	error
	storeKind() Kind
}

func directStoreKind(err error) (Kind, bool) {
	// Wrap's compatibility no-op depends on the outer error itself already being
	// classified; a nested provider must not satisfy this check.
	provider, ok := err.(storeKindProvider) //nolint:errorlint
	if !ok || isNilStoreProvider(provider) || !isStoreKind(provider.storeKind()) {
		return KindUnknown, false
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

func isStoreKind(kind Kind) bool {
	switch kind {
	case KindNotFound,
		KindAlreadyExists,
		KindConstraint,
		KindSerialization,
		KindDeadlock,
		KindContention,
		KindConflict,
		KindTimeout,
		KindUnavailable,
		KindReadOnly:
		return true
	default:
		return false
	}
}

func kindsMatch(kind Kind, target Kind) bool {
	if kind == target {
		return true
	}
	return target == KindConflict && isLegacyConflictKind(kind)
}

func isLegacyConflictKind(kind Kind) bool {
	switch kind {
	case KindConstraint, KindSerialization, KindDeadlock, KindContention:
		return true
	default:
		return false
	}
}

func defaultTransient(kind Kind) bool {
	switch kind {
	case KindSerialization, KindDeadlock, KindContention, KindTimeout, KindUnavailable:
		return true
	default:
		return false
	}
}

func kindMessage(kind Kind) string {
	switch kind {
	case KindNotFound:
		return "store: record not found"
	case KindAlreadyExists:
		return "store: duplicate record"
	case KindConstraint:
		return "store: constraint violation"
	case KindSerialization:
		return "store: serialization failure"
	case KindDeadlock:
		return "store: deadlock"
	case KindContention:
		return "store: contention"
	case KindConflict:
		return "store: conflict"
	case KindTimeout:
		return "store: timeout"
	case KindUnavailable:
		return "store: unavailable"
	case KindReadOnly:
		return "store: read-only"
	default:
		return "store: unknown error"
	}
}

func legacyHTTPStatus(kind Kind) int {
	switch kind {
	case KindNotFound:
		return 404
	case KindAlreadyExists,
		KindConstraint,
		KindSerialization,
		KindDeadlock,
		KindContention,
		KindConflict:
		return 409
	case KindTimeout:
		return 504
	case KindUnavailable, KindReadOnly:
		return 503
	default:
		return 500
	}
}
