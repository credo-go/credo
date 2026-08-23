// Originally derived from Echo (https://github.com/labstack/echo),
// Copyright (c) 2024 LabStack, MIT licensed. Substantially modified for Credo;
// see the NOTICES file for full attribution.

package credo

import (
	jsonv1 "encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/netip"
	"net/url"
	"reflect"

	internalproxy "github.com/credo-go/credo/internal/proxy"
	"github.com/credo-go/credo/validation"
)

// Request wraps *http.Request and provides request-side helpers:
// route parameters, query parameter shortcuts, and body/query binding.
type Request struct {
	*http.Request

	// app is a back-reference to the application, set by the context pool.
	// Used for debug-mode bind warnings. Nil for externally created Requests.
	app *App

	// paramKeys/paramValues hold URL parameters populated during dispatch
	// as parallel slices in insertion order. Typical routes carry at most a
	// handful of parameters, so a linear scan beats a map and the backing
	// arrays are retained across pool reuse — steady-state dispatch does not
	// allocate for parameters. For host-scoped routes they include both
	// path params and host params (collisions are rejected at registration).
	paramKeys   []string
	paramValues []string

	// paramsMap is the lazily materialized view served by RouteParams.
	// Rebuilt from the slices when paramsMapValid is false; backing storage
	// is retained across pool reuse.
	paramsMap      map[string]string
	paramsMapValid bool

	// cachedQuery is lazy-parsed from URL.Query() on first access.
	// Cleared on reset to avoid leaking across pooled reuse.
	cachedQuery url.Values

	// cachedScheme and cachedRealIP avoid repeated proxy-header parsing within
	// one request. Cleared on reset to avoid leaking across pooled reuse.
	cachedScheme    string
	cachedSchemeSet bool
	cachedRealIP    string
	cachedRealIPSet bool
}

// NewRequest creates a new Request wrapping the given *http.Request.
func NewRequest(r *http.Request) *Request {
	return &Request{Request: r}
}

// RouteParam returns the URL parameter value for name — a path parameter such
// as {id}, or, for host-scoped routes, a host parameter. It returns "" when
// the parameter is not present.
//
// RouteParam is the preferred accessor for single values; it never
// allocates:
//
//	id := ctx.Request().RouteParam("id")
//
// Unlike the map returned by [Request.RouteParams], the returned string is
// safe to retain after the request completes.
func (r *Request) RouteParam(name string) string {
	for i, k := range r.paramKeys {
		if k == name {
			return r.paramValues[i]
		}
	}
	return ""
}

// RouteParams returns all URL parameter key-value pairs.
// For host-scoped routes, host params and path params share the same namespace.
//
// The map is a read-only view, materialized lazily on first call: writes
// to it are not seen by [Request.RouteParam]. It is owned by the framework
// and recycled after the request completes — do not retain it or read it
// from another goroutine after the handler returns. For single values,
// prefer [Request.RouteParam].
func (r *Request) RouteParams() map[string]string {
	if !r.paramsMapValid {
		if r.paramsMap == nil {
			r.paramsMap = make(map[string]string, len(r.paramKeys))
		} else {
			clear(r.paramsMap)
		}
		for i, k := range r.paramKeys {
			r.paramsMap[k] = r.paramValues[i]
		}
		r.paramsMapValid = true
	}
	return r.paramsMap
}

// resetRouteParams clears the parameter set before a (re-)dispatch.
// Backing storage is retained.
func (r *Request) resetRouteParams() {
	r.paramKeys = r.paramKeys[:0]
	r.paramValues = r.paramValues[:0]
	r.paramsMapValid = false
}

// addRouteParam appends one parameter. Dispatch-internal; uniqueness is
// guaranteed by registration-time validation (path/host collisions panic).
func (r *Request) addRouteParam(key, value string) {
	r.paramKeys = append(r.paramKeys, key)
	r.paramValues = append(r.paramValues, value)
}

// PathValue returns the route parameter for name, falling back to the
// embedded request's [http.Request.PathValue]. Prefer [Request.RouteParam]
// in new code.
//
// This shadow exists for stdlib muscle memory: Credo's dispatcher does not
// populate the embedded *http.Request's path values (doing so would cost an
// allocation per request for data [Request.RouteParam] already serves), so
// without it ctx.Request().PathValue("id") would silently return "". The
// raw embedded request — as seen by stdlib handlers via [App.Mount] or
// middleware via [WrapStdMiddleware] — still carries no path values.
func (r *Request) PathValue(name string) string {
	for i, k := range r.paramKeys {
		if k == name {
			return r.paramValues[i]
		}
	}
	return r.Request.PathValue(name)
}

// QueryParam returns a query string parameter value by name.
// Returns "" if not present.
func (r *Request) QueryParam(name string) string {
	return r.query().Get(name)
}

// Scheme reports the scheme the original client used: "http" or "https".
//
// If the request arrived over TLS directly, Scheme returns "https". Otherwise,
// forwarded scheme headers are considered only when the immediate peer
// RemoteAddr is configured as a trusted proxy on the App. Untrusted peers cannot
// influence the result.
//
// Only "http" and "https" are returned. Invalid forwarded header values fall
// back to the underlying transport.
func (r *Request) Scheme() string {
	if r == nil {
		return "http"
	}
	if r.cachedSchemeSet {
		return r.cachedScheme
	}

	var trustedProxies []netip.Prefix
	if r.app != nil {
		trustedProxies = r.app.trustedProxies
	}
	r.cachedScheme = internalproxy.Scheme(r.Request, trustedProxies)
	r.cachedSchemeSet = true
	return r.cachedScheme
}

// RealIP returns the address of the original client.
//
// When the immediate peer RemoteAddr is trusted, RealIP walks the proxy
// chain — the RFC 7239 Forwarded header's for= parameters first, then
// X-Forwarded-For — from right to left, skipping trusted proxy hops and
// returning the first untrusted address. If neither yields a usable value,
// X-Real-IP is used. If the peer is untrusted, all forwarded headers are
// ignored.
//
// The returned value is an IP address only for parseable addresses. If
// RemoteAddr itself is unparseable, RealIP falls back to RemoteAddr verbatim.
func (r *Request) RealIP() string {
	if r == nil {
		return ""
	}
	if r.cachedRealIPSet {
		return r.cachedRealIP
	}

	var trustedProxies []netip.Prefix
	if r.app != nil {
		trustedProxies = r.app.trustedProxies
	}
	r.cachedRealIP = internalproxy.RealIP(r.Request, trustedProxies)
	r.cachedRealIPSet = true
	return r.cachedRealIP
}

// query returns the parsed query values, caching the result for reuse
// within the same request.
func (r *Request) query() url.Values {
	if r.cachedQuery == nil {
		r.cachedQuery = r.URL.Query()
	}
	return r.cachedQuery
}

// BindBody decodes the request body into target based on the Content-Type header.
// Supported content types:
//   - application/json (default when Content-Type is absent)
//   - application/xml, text/xml
//   - application/x-www-form-urlencoded (uses "form" struct tags)
//   - multipart/form-data (uses "form" struct tags, including file fields)
//
// JSON bodies are decoded with encoding/json/v2 under strict semantics:
// exactly one JSON value is accepted — any content after it (a second
// document, or trailing garbage) fails the bind with
// [BindReasonTrailingData] (trailing whitespace is allowed) — and object
// members must be unique: a repeated member (including a case-variant
// repeat) fails with [BindReasonDuplicateField]. Member names keep v1's
// case-insensitive matching against struct fields.
//
// If target implements [validation.Validatable], Validate() is called
// automatically after successful decoding ("parse, don't validate").
//
// Decode failures return a [*BindError] carrying a typed reason (400 Bad
// Request via the error pipeline). Exceeding the body-size limit returns
// 413 Request Entity Too Large; an unrecognized content type returns
// 415 Unsupported Media Type.
func (r *Request) BindBody(target any) error {
	// Developer-error guard, checked before any decoding so it cannot be
	// misreported as a client payload problem.
	if rv := reflect.ValueOf(target); rv.Kind() != reflect.Pointer || rv.IsNil() {
		return NewHTTPError(http.StatusBadRequest, "bind target must be a non-nil pointer")
	}

	if r.Body == nil {
		return &BindError{Reason: BindReasonEmptyBody}
	}

	ct := r.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/json"
	}

	mediaType, _, err := mime.ParseMediaType(ct)
	if err != nil {
		// If parsing fails, use the raw Content-Type as-is
		mediaType = ct
	}

	switch mediaType {
	case "application/json":
		dec := jsontext.NewDecoder(r.Body)
		// FormatDurationAsNano: json/v2 has no default time.Duration
		// representation and Go 1.27 ships without the `format:` tag, so
		// without this option any Duration field would fail every bind.
		// Durations decode as integer nanoseconds, as under encoding/json v1.
		opts := []jsonv2.Options{
			jsonv2.MatchCaseInsensitiveNames(true),
			jsonv1.FormatDurationAsNano(true),
		}
		if r.app != nil && r.app.strictBodies {
			opts = append(opts, jsonv2.RejectUnknownMembers(true))
		}
		if err := jsonv2.UnmarshalDecode(dec, target, opts...); err != nil {
			return jsonBindError(err)
		}
		// Enforce a single JSON value per body: after the first value only
		// whitespace may remain. ReadToken returns io.EOF for a clean end; a
		// second value or trailing garbage yields a token or a syntax error.
		if _, terr := dec.ReadToken(); !errors.Is(terr, io.EOF) {
			if he, ok := maxBytesHTTPError(terr); ok {
				return he
			}
			return &BindError{Reason: BindReasonTrailingData, Internal: terr}
		}

	case "application/xml", "text/xml":
		if err := xml.NewDecoder(r.Body).Decode(target); err != nil {
			return xmlBindError(err)
		}

	case "application/x-www-form-urlencoded":
		if err := r.ParseForm(); err != nil {
			return formBindError(err)
		}
		if err := decodeValues(target, r.PostForm, "form"); err != nil {
			return err
		}

	case "multipart/form-data":
		if err := r.ParseMultipartForm(defaultMultipartMaxMemory); err != nil {
			return formBindError(err)
		}
		if err := decodeValues(target, url.Values(r.MultipartForm.Value), "form"); err != nil {
			return err
		}
		if err := bindMultipartFiles(target, r.MultipartForm.File); err != nil {
			return err
		}

	default:
		return NewHTTPError(http.StatusUnsupportedMediaType,
			"unsupported content type: "+mediaType)
	}

	return r.validateBoundTarget("BindBody", target)
}

// maxBytesHTTPError detects a body-size-limit overrun (http.MaxBytesReader)
// anywhere in a decode error chain and maps it to 413 Request Entity Too
// Large. Size overruns keep their dedicated status instead of becoming a
// 400-class BindError.
func maxBytesHTTPError(err error) (*HTTPError, bool) {
	if mbe, ok := errors.AsType[*http.MaxBytesError](err); ok {
		return NewHTTPError(http.StatusRequestEntityTooLarge, "request body too large").
			WithInternal(mbe), true
	}
	return nil, false
}

// jsonBindError maps an encoding/json/v2 decode failure to a typed
// [BindError] (or 413 for body-size overruns):
//
//   - io.EOF (empty or whitespace-only body) → empty_body
//   - jsontext.ErrDuplicateName → duplicate_field with the member's path
//   - jsonv2.ErrUnknownName (strict bodies only) → unknown_field with the
//     member's path
//   - other *jsontext.SyntacticError → syntax with the byte offset
//   - *jsonv2.SemanticError with a nil inner error (pure JSON-kind vs Go-type
//     clash) → type_mismatch with the expected JSON type term
//   - *jsonv2.SemanticError with an inner error (right JSON kind, failed
//     semantic conversion: time parse, TextUnmarshaler, overflow) →
//     invalid_value
//
// Unknown error shapes fall back to the syntax reason; the original error
// is always preserved as Internal for logging.
func jsonBindError(err error) error {
	if he, ok := maxBytesHTTPError(err); ok {
		return he
	}
	if errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return &BindError{Reason: BindReasonEmptyBody, Internal: err}
	}
	if errors.Is(err, jsontext.ErrDuplicateName) {
		be := &BindError{Reason: BindReasonDuplicateField, Internal: err}
		if se, ok := errors.AsType[*jsontext.SyntacticError](err); ok {
			be.Field = jsonPointerToFieldPath(string(se.JSONPointer))
			be.Offset = se.ByteOffset
		}
		return be
	}
	if se, ok := errors.AsType[*jsontext.SyntacticError](err); ok {
		return &BindError{Reason: BindReasonSyntax, Offset: se.ByteOffset, Internal: err}
	}
	// ErrUnknownName arrives wrapped in a *SemanticError with Err set, so it
	// must be recognised before the generic invalid_value branch below.
	if errors.Is(err, jsonv2.ErrUnknownName) {
		be := &BindError{Reason: BindReasonUnknownField, Internal: err}
		if sme, ok := errors.AsType[*jsonv2.SemanticError](err); ok {
			be.Field = jsonPointerToFieldPath(string(sme.JSONPointer))
			be.Offset = sme.ByteOffset
		}
		return be
	}
	if sme, ok := errors.AsType[*jsonv2.SemanticError](err); ok {
		field := jsonPointerToFieldPath(string(sme.JSONPointer))
		if sme.Err != nil {
			return &BindError{
				Reason:   BindReasonInvalidValue,
				Field:    field,
				Offset:   sme.ByteOffset,
				Internal: err,
			}
		}
		return &BindError{
			Reason:   BindReasonTypeMismatch,
			Field:    field,
			Expected: jsonTypeName(sme.GoType),
			Offset:   sme.ByteOffset,
			Internal: err,
		}
	}
	return &BindError{Reason: BindReasonSyntax, Internal: err}
}

// xmlBindError maps an encoding/xml decode failure to a typed [BindError]
// (or 413 for body-size overruns).
func xmlBindError(err error) error {
	if he, ok := maxBytesHTTPError(err); ok {
		return he
	}
	if errors.Is(err, io.EOF) {
		return &BindError{Reason: BindReasonEmptyBody, Internal: err}
	}
	return &BindError{Reason: BindReasonSyntax, Internal: err}
}

// formBindError maps a form/multipart parse failure to a typed [BindError]
// (or 413 for body-size overruns).
func formBindError(err error) error {
	if he, ok := maxBytesHTTPError(err); ok {
		return he
	}
	return &BindError{Reason: BindReasonSyntax, Internal: err}
}

// BindQuery decodes URL query parameters into target using `query:"name"` struct tags.
// If target implements [validation.Validatable], Validate() is called automatically
// after successful decoding ("parse, don't validate").
//
// In debug mode, a warning is logged when the target does not implement Validatable.
func (r *Request) BindQuery(target any) error {
	if err := decodeValues(target, r.query(), "query"); err != nil {
		return err
	}
	return r.validateBoundTarget("BindQuery", target)
}

func (r *Request) validateBoundTarget(op string, target any) error {
	if v, ok := target.(validation.Validatable); ok {
		return v.Validate()
	}
	if r.app.IsDebug() {
		r.app.logger.Warn(op+": target does not implement Validatable, skipping validation",
			"type", fmt.Sprintf("%T", target))
	}
	return nil
}

// reset prepares the Request for pool reuse.
func (r *Request) reset(hr *http.Request) {
	r.Request = hr
	r.resetRouteParams() // retains backing storage for reuse
	r.cachedQuery = nil  // drop reference so next request parses its own URL
	r.cachedScheme = ""
	r.cachedSchemeSet = false
	r.cachedRealIP = ""
	r.cachedRealIPSet = false
}
