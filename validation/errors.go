package validation

import (
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"strings"
)

// MessageKey is a catalog key: the exact key of a message in the
// application's translation catalog. It is never rendered as text — a key the
// catalog does not define falls back to the built-in message for the code —
// so literal client text goes elsewhere ([credo.HTTPError.WithMessage]), and
// client input goes into details. The root package aliases it as
// credo.MessageKey.
//
// The named type keeps a key a literal or a typed constant: a key computed as
// a string, such as one concatenated from a request value, does not compile
// without a conversion. Declare key constants with the type, so a scan of the
// sources finds them.
type MessageKey string

// Violation is one entry of an error response's violations list: a rule a
// field's value broke. It is named for what it carries rather than for the
// package, since the error pipeline reports a bind failure, a value the
// binder could not read, in the same type.
type Violation struct { //nolint:errname // Named for what it carries, the wire's "violations" (ADR-011).
	// Field is the field path, e.g. "name", "address.city", "items[0]".
	Field string `json:"field"`

	// Code is the rule identifier / i18n key, e.g. "required", "email", "min".
	Code string `json:"code"`

	// Message is the default English error message.
	Message string `json:"message"`

	// MessageKey is an optional catalog key for the message. It is used by
	// Credo's i18n error pipeline and is never serialized to clients.
	MessageKey MessageKey `json:"-"`

	// Params holds template variables for localized messages,
	// e.g. {"min": 2, "max": 100}.
	//
	// Params is serialized into the HTTP error response (deliberately —
	// clients use it to render localized messages). Custom rules must not
	// place internal or sensitive values here.
	Params map[string]any `json:"params,omitempty"`
}

// Error implements the error interface.
func (e *Violation) Error() string {
	if e.Field != "" {
		return e.Field + ": " + e.Message
	}
	return e.Message
}

// Violations is the list of violations a validation found, the error
// [ValidateStruct] returns. It implements the error interface and
// json.Marshaler for error-envelope integration.
type Violations []Violation //nolint:errname // Named for what it carries, the wire's "violations" (ADR-011).

// Error implements the error interface. Returns a semicolon-separated summary.
func (e Violations) Error() string {
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
func (e Violations) MarshalJSON() ([]byte, error) {
	// Marshal the underlying slice to avoid infinite recursion.
	return json.Marshal([]Violation(e))
}

// MarshalJSONTo implements the encoding/json/v2 marshaler interface,
// streaming the underlying slice without the intermediate []byte that
// MarshalJSON would otherwise force on v2 encoders.
func (e Violations) MarshalJSONTo(enc *jsontext.Encoder) error {
	return jsonv2.MarshalEncode(enc, []Violation(e))
}

// Unwrap returns the individual field errors, letting [errors.As] (and
// errors.Is) descend from a validation result — possibly wrapped further
// up the call chain — down to single *Violation values.
func (e Violations) Unwrap() []error {
	if len(e) == 0 {
		return nil
	}
	out := make([]error, len(e))
	for i := range e {
		out[i] = &e[i]
	}
	return out
}

// NewViolation returns a client-visible validation failure with the given rule
// code and message, for a custom rule or an inline [By] function. The field
// path is filled in by [Field], as for the built-in rules.
//
//	return validation.NewViolation("country_code", "must be a 2-letter code")
//
// A rule error that is neither a *Violation nor [Violations] is internal:
// validation stops and the error is returned unchanged, so the error
// pipeline classifies it like any handler error and its text never reaches
// the client.
func NewViolation(code, message string) *Violation {
	return &Violation{Code: code, Message: message}
}

// newRuleError creates a Violation for a built-in rule.
func newRuleError(code, message string, params map[string]any) *Violation {
	return &Violation{
		Code:    code,
		Message: message,
		Params:  params,
	}
}

// prefixErrors prepends prefix to the Field of each failure in err, an
// [Violations] or a single *Violation, and returns them as Violations. If the
// child field starts with "[", it concatenates without a dot separator (e.g.
// "items" + "[0]" → "items[0]"). Any other error is internal and is returned
// unchanged.
func prefixErrors(prefix string, err error) error {
	var result Violations
	if internal := collectErrors(&result, err, prefix); internal != nil {
		return internal
	}
	return result
}

// collectErrors appends the failures in err to dst, prefixing each Field with
// fieldPath. It never mutates err: each element of a Violations slice is copied
// before its Field is rewritten, so a rule that retains or shares the
// returned slice is unaffected.
//
// The type of err decides its meaning, found the way the error pipeline
// finds it, through the error's chain: [Violations] or a *Violation is a
// client-visible failure; any other error is internal, appends nothing and is
// returned for the caller to stop validation with.
func collectErrors(dst *Violations, err error, fieldPath string) error {
	if errs, ok := errors.AsType[Violations](err); ok {
		for _, ve := range errs {
			ve.Field = joinFieldPath(fieldPath, ve.Field)
			*dst = append(*dst, ve)
		}
		return nil
	}
	if e, ok := errors.AsType[*Violation](err); ok {
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
