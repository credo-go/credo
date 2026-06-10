package validation

import (
	"encoding/json"
	"errors"
	"strings"
)

// ValidationError represents a single field validation failure.
type ValidationError struct {
	// Field is the field path, e.g. "name", "address.city", "items[0]".
	Field string `json:"field"`

	// Code is the rule identifier / i18n key, e.g. "required", "email", "min".
	Code string `json:"code"`

	// Message is the default English error message.
	Message string `json:"message"`

	// Params holds template variables for localized messages,
	// e.g. {"min": 2, "max": 100}.
	//
	// Params is serialized into the HTTP error response (deliberately —
	// clients use it to render localized messages). Custom rules must not
	// place internal or sensitive values here.
	Params map[string]any `json:"params,omitempty"`
}

// Error implements the error interface.
func (e *ValidationError) Error() string {
	if e.Field != "" {
		return e.Field + ": " + e.Message
	}
	return e.Message
}

// Errors is a collection of validation errors. It implements the error
// interface and json.Marshaler for RFC 7807 integration.
type Errors []ValidationError

// Error implements the error interface. Returns a semicolon-separated summary.
func (e Errors) Error() string {
	if len(e) == 0 {
		return ""
	}
	msgs := make([]string, len(e))
	for i, ve := range e {
		msgs[i] = ve.Error()
	}
	return strings.Join(msgs, "; ")
}

// MarshalJSON implements json.Marshaler. Serializes as a JSON array.
func (e Errors) MarshalJSON() ([]byte, error) {
	// Marshal the underlying slice to avoid infinite recursion.
	return json.Marshal([]ValidationError(e))
}

// Unwrap returns the individual field errors, letting [errors.As] (and
// errors.Is) descend from a validation result — possibly wrapped further
// up the call chain — down to single *ValidationError values.
func (e Errors) Unwrap() []error {
	if len(e) == 0 {
		return nil
	}
	out := make([]error, len(e))
	for i := range e {
		out[i] = &e[i]
	}
	return out
}

// newRuleError creates a ValidationError for a built-in rule.
func newRuleError(code, message string, params map[string]any) *ValidationError {
	return &ValidationError{
		Code:    code,
		Message: message,
		Params:  params,
	}
}

// toValidationError converts any error to a *ValidationError.
// If the error is already a *ValidationError, returns it directly.
// Otherwise wraps it with code "invalid".
func toValidationError(err error) *ValidationError {
	if ve, ok := errors.AsType[*ValidationError](err); ok {
		return ve
	}
	return &ValidationError{
		Code:    "invalid",
		Message: err.Error(),
	}
}

// prefixErrors takes an error (which may be Errors or a single
// *ValidationError) and prepends the prefix to each error's Field.
// If the child field starts with "[", concatenates without a dot separator
// (e.g. "items" + "[0]" → "items[0]").
func prefixErrors(prefix string, err error) Errors {
	if errs, ok := errors.AsType[Errors](err); ok {
		result := make(Errors, len(errs))
		for i, ve := range errs {
			result[i] = ve
			result[i].Field = joinFieldPath(prefix, ve.Field)
		}
		return result
	}

	ve := toValidationError(err)
	ve.Field = joinFieldPath(prefix, ve.Field)
	return Errors{*ve}
}

// collectErrors normalizes err and appends the resulting ValidationError(s)
// to dst, prefixing each error's Field with fieldPath.
func collectErrors(dst *Errors, err error, fieldPath string) {
	if errs, ok := errors.AsType[Errors](err); ok {
		for i := range errs {
			errs[i].Field = joinFieldPath(fieldPath, errs[i].Field)
		}
		*dst = append(*dst, errs...)
		return
	}
	ve := toValidationError(err)
	ve.Field = joinFieldPath(fieldPath, ve.Field)
	*dst = append(*dst, *ve)
}

// joinFieldPath joins a parent and child field path.
// If child starts with "[", concatenates without dot.
// If child is empty, returns parent.
// Otherwise joins with ".".
func joinFieldPath(parent, child string) string {
	if child == "" {
		return parent
	}
	if parent == "" {
		return child
	}
	if strings.HasPrefix(child, "[") {
		return parent + child
	}
	return parent + "." + child
}
