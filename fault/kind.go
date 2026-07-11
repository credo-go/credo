package fault

import (
	"errors"
	"reflect"
)

// Kind identifies the semantic category of an error independently of any
// transport status code.
type Kind string

// Semantic error kinds understood by Credo's default transport policies.
const (
	KindUnknown       Kind = ""
	KindNotFound      Kind = "not_found"
	KindAlreadyExists Kind = "already_exists"
	KindConstraint    Kind = "constraint"
	KindSerialization Kind = "serialization"
	KindDeadlock      Kind = "deadlock"
	KindContention    Kind = "contention"
	KindConflict      Kind = "conflict"
	KindTimeout       Kind = "timeout"
	KindUnavailable   Kind = "unavailable"
	KindReadOnly      Kind = "read_only"
)

// Provider is implemented by errors that expose a transport-neutral semantic
// kind. Implementations must keep sensitive driver metadata out of FaultKind.
type Provider interface {
	error
	FaultKind() Kind
}

// ProviderOf finds the first non-nil Provider in err's unwrap/join tree. A
// provider returning KindUnknown is still returned so transport policy can
// fail closed instead of falling through to an unrelated legacy interface.
func ProviderOf(err error) (Provider, bool) {
	if err == nil {
		return nil, false
	}
	if provider, ok := err.(Provider); ok && !isNilProvider(provider) {
		return provider, true
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, child := range joined.Unwrap() {
			if provider, found := ProviderOf(child); found {
				return provider, true
			}
		}
		return nil, false
	}
	return ProviderOf(errors.Unwrap(err))
}

// KindOf finds the first semantic Kind in err's unwrap/join tree.
func KindOf(err error) (Kind, bool) {
	if err == nil {
		return KindUnknown, false
	}
	if provider, ok := err.(Provider); ok && !isNilProvider(provider) {
		if kind := provider.FaultKind(); kind != KindUnknown {
			return kind, true
		}
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, child := range joined.Unwrap() {
			if kind, found := KindOf(child); found {
				return kind, true
			}
		}
		return KindUnknown, false
	}
	return KindOf(errors.Unwrap(err))
}

func isNilProvider(provider Provider) bool {
	value := reflect.ValueOf(provider)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}
