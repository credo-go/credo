package credo

import (
	"net/http"
	"reflect"
	"strconv"
	"strings"
)

// BindErrorReason identifies why [Request.BindBody] or [Request.BindQuery]
// failed to decode the request payload into the target struct.
//
// Reason values are stable machine-readable identifiers: each appears as the
// Code of the violations[] entry in the default response and as the scoped
// i18n lookup reason.
type BindErrorReason string

const (
	// BindReasonSyntax reports a malformed payload: invalid JSON, XML, or
	// form encoding, including a truncated body.
	BindReasonSyntax BindErrorReason = "syntax"

	// BindReasonTypeMismatch reports a value whose type does not match the
	// target field — a JSON string where a number is expected, or a
	// non-numeric form/query value for an integer field.
	BindReasonTypeMismatch BindErrorReason = "type_mismatch"

	// BindReasonInvalidValue reports a value of the right shape that failed
	// semantic conversion: rejected by the target field's
	// [encoding.TextUnmarshaler] or a semantic unmarshal step (an
	// unparseable time.Time, netip.Addr, custom type, or numeric overflow).
	BindReasonInvalidValue BindErrorReason = "invalid_value"

	// BindReasonEmptyBody reports a request without a body where one is
	// required.
	BindReasonEmptyBody BindErrorReason = "empty_body"

	// BindReasonTrailingData reports extra content after the first JSON
	// value in the request body. BindBody accepts exactly one JSON value.
	BindReasonTrailingData BindErrorReason = "trailing_data"

	// BindReasonDuplicateField reports a JSON object member that appears
	// more than once (including case-variant repeats of the same field).
	BindReasonDuplicateField BindErrorReason = "duplicate_field"

	// BindReasonUnknownField reports a JSON object member that does not
	// correspond to any field of the target. Only produced under strict
	// bodies ([WithStrictBodies] or server.strict_bodies); by default
	// unknown members are ignored.
	BindReasonUnknownField BindErrorReason = "unknown_field"
)

// BindError is the typed decode error returned by [Request.BindBody] and
// [Request.BindQuery]. The error pipeline classifies it as 400 Bad Request
// with a default error body whose single violations[] entry mirrors the
// validation output shape: Field, Code (the Reason), Message, and Params.
//
// Handlers normally return it unchanged. To branch on it, use
// errors.AsType[*credo.BindError](err).
//
// A request body exceeding the configured size limit is not a BindError:
// it keeps its dedicated 413 Request Entity Too Large classification.
type BindError struct {
	// Reason is the machine-readable failure category.
	Reason BindErrorReason

	// Field is the affected field path (e.g. "age", "user.address.city",
	// or a form/query parameter name). Empty for body-level failures
	// (syntax, empty_body, trailing_data).
	Field string

	// Expected is the human-readable expected type for type_mismatch
	// ("integer", "number", "string", "boolean", "array", "object").
	// Empty for other reasons.
	Expected string

	// Offset is the byte offset in the JSON body where the failure was
	// detected. Zero when unknown or not applicable (XML, form, query).
	Offset int64

	// Internal is the underlying decoder error. It is logged but never
	// exposed to the client.
	Internal error
}

// Error implements the error interface.
func (e *BindError) Error() string {
	msg := "bind: " + string(e.Reason)
	if e.Field != "" {
		msg += ": field " + strconv.Quote(e.Field)
	}
	if e.Expected != "" {
		msg += ": expected " + e.Expected
	}
	if e.Offset > 0 {
		msg += " (offset " + strconv.FormatInt(e.Offset, 10) + ")"
	}
	if e.Internal != nil {
		msg += ": " + e.Internal.Error()
	}
	return msg
}

// Unwrap returns the underlying decoder error, supporting errors.Is/As.
func (e *BindError) Unwrap() error {
	return e.Internal
}

// HTTPStatus returns 400 Bad Request. The error pipeline classifies
// BindError directly; this method keeps the status visible to custom
// renderers and middleware that only understand the HTTPStatus interface.
func (e *BindError) HTTPStatus() int {
	return http.StatusBadRequest
}

// message returns the default English client-facing message for the
// violations[] entry. Field-scoped reasons use the validation-message style
// ("must be of type integer"); body-level reasons name their subject.
func (e *BindError) message() string {
	switch e.Reason {
	case BindReasonSyntax:
		return "request body is malformed"
	case BindReasonTypeMismatch:
		if e.Expected != "" {
			return "must be of type " + e.Expected
		}
		return "has an invalid type"
	case BindReasonInvalidValue:
		return "is not a valid value"
	case BindReasonEmptyBody:
		return "request body is empty"
	case BindReasonTrailingData:
		return "request body must contain a single JSON value"
	case BindReasonDuplicateField:
		return "must appear only once"
	case BindReasonUnknownField:
		return "is not a known field"
	}
	return "request could not be decoded"
}

// params returns the template variables serialized into the default response's
// violations[] entry (and passed to i18n translation). Nil when empty.
func (e *BindError) params() map[string]any {
	var p map[string]any
	if e.Expected != "" {
		p = map[string]any{"expected": e.Expected}
	}
	if e.Offset > 0 {
		if p == nil {
			p = make(map[string]any, 1)
		}
		p["offset"] = e.Offset
	}
	return p
}

// jsonPointerToFieldPath converts an RFC 6901 JSON Pointer (as reported by
// encoding/json/v2 errors, e.g. "/user/age", "/items/1/n") to the dot-path
// field convention used by validation errors ("user.age", "items[1].n").
// Purely numeric segments are rendered as array indexes.
func jsonPointerToFieldPath(pointer string) string {
	if pointer == "" {
		return ""
	}
	var b strings.Builder
	for seg := range strings.SplitSeq(strings.TrimPrefix(pointer, "/"), "/") {
		seg = strings.ReplaceAll(seg, "~1", "/")
		seg = strings.ReplaceAll(seg, "~0", "~")
		if isAllDigits(seg) {
			b.WriteString("[" + seg + "]")
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('.')
		}
		b.WriteString(seg)
	}
	return b.String()
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// jsonTypeName maps the Go target type of a JSON semantic decode error to a
// client-facing JSON type term. Go type names are deliberately not exposed.
func jsonTypeName(t reflect.Type) string {
	if t == nil {
		return ""
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "boolean"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "integer"
	case reflect.Float32, reflect.Float64:
		return "number"
	case reflect.Slice, reflect.Array:
		return "array"
	case reflect.Map, reflect.Struct:
		return "object"
	default:
		return "value"
	}
}
