package credo

import "sync"

// pool is a type-safe wrapper around sync.Pool using Go generics.
type pool[T any] struct {
	p sync.Pool
}

// newPool creates a new pool with the given constructor function.
func newPool[T any](fn func() T) *pool[T] {
	return &pool[T]{p: sync.Pool{New: func() any { return fn() }}}
}

func (p *pool[T]) get() T  { return p.p.Get().(T) }
func (p *pool[T]) put(x T) { p.p.Put(x) }
