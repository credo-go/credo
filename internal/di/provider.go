// Adapted from github.com/samber/do (MIT License).

package di

import (
	"fmt"
	"reflect"
)

// provider is one registered type's construction strategy. The container
// holds exactly one provider per type; the strategy decides how the singleton
// is built and which dependencies the graph validator and cycle detector see.
// Replace protection is not part of the provider: it lives in
// Container.protected, keyed by type.
type provider interface {
	// deps returns the constructor parameter types the container must satisfy
	// (validated at Seal, walked for cycles). Pre-built values and opaque
	// factories have none.
	deps() []reflect.Type

	// build produces the instance. stack is the resolution stack that
	// parameter resolution extends for cycle detection.
	build(c *Container, stack []reflect.Type) (any, error)
}

// constructorProvider calls a reflected constructor (Provide) with parameters
// resolved from the container.
type constructorProvider struct {
	constructor  reflect.Value  // the constructor function
	paramTypes   []reflect.Type // cached constructor parameter types
	resultType   reflect.Type   // the T being registered
	returnsError bool           // true if constructor returns (T, error)
}

func (p *constructorProvider) deps() []reflect.Type { return p.paramTypes }

func (p *constructorProvider) build(c *Container, stack []reflect.Type) (any, error) {
	serviceName := deriveServiceName(p.resultType)

	params := make([]reflect.Value, len(p.paramTypes))
	for i, pt := range p.paramTypes {
		param, err := c.resolveParamValue(pt, serviceName, stack)
		if err != nil {
			return nil, fmt.Errorf("di: constructing %s: param %d (%s): %w", p.resultType, i, pt, err)
		}
		params[i] = param
	}

	results := p.constructor.Call(params)
	instance := results[0].Interface()
	if p.returnsError {
		if errVal := results[1].Interface(); errVal != nil {
			return nil, fmt.Errorf("di: constructing %s: %w", p.resultType, errVal.(error))
		}
	}
	return instance, nil
}

// valueProvider returns a pre-built value (ProvideValue, ProvideProtectedValue,
// Replace). It has no dependencies and is always valid during Seal.
type valueProvider struct {
	value any
}

func (valueProvider) deps() []reflect.Type { return nil }

func (p valueProvider) build(*Container, []reflect.Type) (any, error) { return p.value, nil }

// factoryProvider runs a compile-time-checked factory (ProvideFactory). It is
// opaque to the container: no parameter injection, and any Resolve calls
// inside fn start a fresh cycle-detection stack.
type factoryProvider struct {
	resultType reflect.Type
	fn         func() (any, error)
}

func (factoryProvider) deps() []reflect.Type { return nil }

func (p factoryProvider) build(*Container, []reflect.Type) (any, error) {
	instance, err := p.fn()
	if err != nil {
		return nil, fmt.Errorf("di: constructing %s: %w", p.resultType, err)
	}
	return instance, nil
}
