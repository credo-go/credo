package credo

import (
	"fmt"
	"strings"
)

// Key is a typed key: it names a request value, set with [Context.Set] and
// read with [Context.Get], or a route meta value, set with [Route.SetMeta] and
// read with [Route.GetMeta]. The type parameter binds the value's type to the
// key, so a value of another type does not compile:
//
//	var Tenant = credo.NewKey[*TenantInfo]("tenant")
//
//	c.Set(Tenant, t)       // t must be a *TenantInfo
//	t, ok := c.Get(Tenant) // t is a *TenantInfo
//
// A key's identity is the value [NewKey] returns, not its name: two NewKey
// calls with one name are two keys, so the keys of two packages never collide.
// The name is for diagnostics only. (A [MessageKey] is the opposite: two
// message keys with one text are one catalog key.)
//
// A request value lives in the request's context.Context: Key is the context
// key, so code that holds only a context.Context reads a value with the
// standard Value and the key, ctx.Value(Tenant).(*TenantInfo).
//
// Declare keys with NewKey, as package-level variables. The zero Key, declared
// without NewKey, is not a key: every method that takes one panics with it,
// since all zero keys of one type would be one key.
type Key[T any] struct {
	id *keyID
}

// keyID is a key's identity. Meta tables index by it; a request value's
// context key is the Key value itself, which compares by it.
type keyID struct {
	name string
}

// NewKey returns a new key for values of type T. name is for diagnostics: it
// is what the key's String method returns, and it does not make two keys one.
func NewKey[T any](name string) Key[T] {
	return Key[T]{id: &keyID{name: name}}
}

// String returns the key's name, as given to [NewKey].
func (k Key[T]) String() string {
	if k.id == nil {
		return "<zero credo.Key>"
	}
	return k.id.name
}

// mustID returns the key's identity and panics, naming op, for a zero key.
func (k Key[T]) mustID(op string) *keyID {
	if k.id == nil {
		zeroKeyPanic[T](op)
	}
	return k.id
}

// zeroKeyPanic is kept out of mustID so that the accessors stay small enough
// to inline.
//
//go:noinline
func zeroKeyPanic[T any](op string) {
	typ := strings.TrimPrefix(fmt.Sprintf("%T", (*T)(nil)), "*")
	panic(fmt.Sprintf("credo: %s: the key is a zero credo.Key[%s]; declare it with credo.NewKey", op, typ))
}
