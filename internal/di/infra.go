package di

import (
	"context"
	"reflect"
)

// FrameworkProvider produces a value for constructor parameters of Type that
// have no registration of their own: the framework injects them. The root
// package registers credo.Infra this way so the container can produce it
// without importing the root package (avoiding an import cycle). Factory
// receives the name of the unit being built — its binding's type as the
// lifecycle prints it, such as "*users.Service", or a managed component's
// name — and returns the value to pass.
type FrameworkProvider struct {
	Type    reflect.Type
	Factory func(unitName string) any
}

// SetFrameworkProvider registers (or replaces) the framework provider for
// p.Type. Must be called before any Resolve or Seal (typically in credo.New);
// the resolve path reads the provider map without locking.
func (c *Container) SetFrameworkProvider(p FrameworkProvider) {
	if p.Type == nil || p.Factory == nil {
		panic("di: SetFrameworkProvider: Type and Factory must not be nil")
	}
	c.frameworkProviders[p.Type] = p
}

// frameworkProvider returns the provider for t when the framework produces it.
func (c *Container) frameworkProvider(t reflect.Type) (FrameworkProvider, bool) {
	p, ok := c.frameworkProviders[t]
	return p, ok
}

// isFrameworkType reports whether t is produced by a framework provider
// rather than satisfied by a registration.
func (c *Container) isFrameworkType(t reflect.Type) bool {
	_, ok := c.frameworkProviders[t]
	return ok
}

// SetCallContext installs the function that derives the context of every
// teardown call the container makes on a component — Shutdown or Close —
// from the drain context and the unit's name. The root puts the component's
// scoped Infra in it. Must be called before any Shutdown; nil leaves the
// context as it is.
func (c *Container) SetCallContext(fn func(ctx context.Context, unitName string) context.Context) {
	c.callContext = fn
}

// callCtx returns the context of a call on u.
func (c *Container) callCtx(ctx context.Context, u *Unit) context.Context {
	if c.callContext == nil {
		return ctx
	}
	return c.callContext(ctx, u.Name())
}
