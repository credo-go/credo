package validation

// By creates a [Rule] from an inline function. The type parameter T is
// inferred from the function signature.
//
// The function reports a client-visible failure with a *ValidationError —
// [NewError] builds one — or [Errors]. Any other error is internal: validation
// stops and the error leaves Validate unchanged, so a lookup that could not
// reach its database is a server failure, not invalid input.
//
//	validation.Field(&c.Code, validation.By(func(code string) error {
//	    if len(code) != 2 {
//	        return validation.NewError("country_code", "must be a 2-letter code")
//	    }
//	    return nil
//	}))
func By[T any](fn func(T) error) Rule[T] {
	return funcRule[T](fn)
}

// In creates a [Rule] that checks the value is one of the allowed values.
// The allowed values are not included in the error message for security.
func In[T comparable](values ...T) Rule[T] {
	return funcRule[T](func(value T) error {
		for _, v := range values {
			if value == v {
				return nil
			}
		}
		return newRuleError("in", "must be one of the allowed values", nil)
	})
}

// NotNil creates a [Rule] that checks the pointer is not nil.
func NotNil[T any]() Rule[*T] {
	return funcRule[*T](func(value *T) error {
		if value == nil {
			return newRuleError("not_nil", "is required", nil)
		}
		return nil
	})
}
