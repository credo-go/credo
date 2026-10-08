package validation

import (
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
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

	// MessageKey is an optional exact presentation key. It is used by Credo's
	// i18n error pipeline and is never serialized to clients.
	MessageKey string `json:"-"`

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
// interface and json.Marshaler for error-envelope integration.
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

// MarshalJSONTo implements the encoding/json/v2 marshaler interface,
// streaming the underlying slice without the intermediate []byte that
// MarshalJSON would otherwise force on v2 encoders.
func (e Errors) MarshalJSONTo(enc *jsontext.Encoder) error {
	return jsonv2.MarshalEncode(enc, []ValidationError(e))
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

// NewError returns a client-visible validation failure with the given rule
// code and message, for a custom rule or an inline [By] function. The field
// path is filled in by [Field], as for the built-in rules.
//
//	return validation.NewError("country_code", "must be a 2-letter code")
//
// A rule error that is neither a *ValidationError nor [Errors] is internal:
// validation stops and the error is returned unchanged, so the error
// pipeline classifies it like any handler error and its text never reaches
// the client.
func NewError(code, message string) *ValidationError {
	return &ValidationError{Code: code, Message: message}
}

// newRuleError creates a ValidationError for a built-in rule.
func newRuleError(code, message string, params map[string]any) *ValidationError {
	return &ValidationError{
		Code:    code,
		Message: message,
		Params:  params,
	}
}

// prefixErrors prepends prefix to the Field of each failure in err, an
// [Errors] or a single *ValidationError, and returns them as Errors. If the
// child field starts with "[", it concatenates without a dot separator (e.g.
// "items" + "[0]" → "items[0]"). Any other error is internal and is returned
// unchanged.
func prefixErrors(prefix string, err error) error {
	var result Errors
	if internal := collectErrors(&result, err, prefix); internal != nil {
		return internal
	}
	return result
}

// collectErrors appends the failures in err to dst, prefixing each Field with
// fieldPath. It never mutates err: each element of an Errors slice is copied
// before its Field is rewritten, so a rule that retains or shares the
// returned slice is unaffected.
//
// The type of err decides its meaning, found the way the error pipeline
// finds it, through the error's chain: [Errors] or a *ValidationError is a
// client-visible failure; any other error is internal, appends nothing and is
// returned for the caller to stop validation with.
func collectErrors(dst *Errors, err error, fieldPath string) error {
	if errs, ok := errors.AsType[Errors](err); ok {
		for _, ve := range errs {
			ve.Field = joinFieldPath(fieldPath, ve.Field)
			*dst = append(*dst, ve)
		}
		return nil
	}
	if e, ok := errors.AsType[*ValidationError](err); ok {
		ve := *e
		ve.Field = joinFieldPath(fieldPath, ve.Field)
		*dst = append(*dst, ve)
		return nil
	}
	return err
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
