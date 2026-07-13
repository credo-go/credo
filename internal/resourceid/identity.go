// Package resourceid canonicalizes stable resource identity tokens shared by
// lifecycle-aware framework integrations.
package resourceid

import (
	"fmt"
	"reflect"
)

// Provider lets a semantic wrapper identify the physical resource whose
// lifecycle it represents. The returned token must be non-nil, comparable,
// reflexively equal, and stable for the resource lifetime; a pointer is the
// usual token.
type Provider interface {
	ResourceIdentity() any
}

// Identity is a comparable canonical resource key.
type Identity struct {
	dynamicType reflect.Type
	token       any
}

// Of returns the stable identity of value. Values implementing [Provider]
// supply an explicit token; other values identify as themselves.
func Of(value any) (identity Identity, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			identity = Identity{}
			err = fmt.Errorf("ResourceIdentity panicked: %v", recovered)
		}
	}()
	if isNil(value) {
		return Identity{}, fmt.Errorf("resource identity value must not be nil")
	}
	if provider, ok := value.(Provider); ok {
		value = provider.ResourceIdentity()
		if isNil(value) {
			return Identity{}, fmt.Errorf("ResourceIdentity must not return nil")
		}
	}

	reflected := reflect.ValueOf(value)
	if !reflected.Comparable() {
		return Identity{}, fmt.Errorf(
			"resource identity token type %s is not comparable; return a stable pointer or comparable token",
			reflected.Type(),
		)
	}
	if !reflected.Equal(reflected) {
		return Identity{}, fmt.Errorf(
			"resource identity token type %s is not reflexively equal",
			reflected.Type(),
		)
	}
	return Identity{dynamicType: reflected.Type(), token: value}, nil
}

// Valid reports whether identity was produced by [Of].
func (identity Identity) Valid() bool {
	return identity.dynamicType != nil
}

func isNil(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice,
		reflect.UnsafePointer:
		return reflected.IsNil()
	default:
		return false
	}
}
