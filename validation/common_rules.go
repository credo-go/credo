package validation

import "errors"

// By creates a [Rule] from an inline function. The type parameter T is
// inferred from the function signature.
//
//	validation.Field(&c.Code, validation.By(func(code string) error {
//	    if len(code) != 2 {
//	        return errors.New("must be a 2-letter code")
//	    }
//	    return nil
//	}))
func By[T any](fn func(T) error) Rule[T] {
	return &byRule[T]{fn: fn}
}

type byRule[T any] struct {
	fn func(T) error
}

func (r *byRule[T]) Validate(value T) error {
	err := r.fn(value)
	if err == nil {
		return nil
	}
	// Preserve multiple errors returned as Errors.
	if errs, ok := errors.AsType[Errors](err); ok {
		return errs
	}
	return toValidationError(err)
}

// In creates a [Rule] that checks the value is one of the allowed values.
// The allowed values are not included in the error message for security.
func In[T comparable](values ...T) Rule[T] {
	return &inRule[T]{values: values}
}

type inRule[T comparable] struct {
	values []T
}

func (r *inRule[T]) Validate(value T) error {
	for _, v := range r.values {
		if value == v {
			return nil
		}
	}
	return newRuleError("in", "must be one of the allowed values", nil)
}

// NotNil creates a [Rule] that checks the pointer is not nil.
func NotNil[T any]() Rule[*T] {
	return &notNilRule[T]{}
}

type notNilRule[T any] struct{}

func (r *notNilRule[T]) Validate(value *T) error {
	if value == nil {
		return newRuleError("not_nil", "is required", nil)
	}
	return nil
}
