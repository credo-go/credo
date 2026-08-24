package credo

import (
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"

	"github.com/credo-go/credo/fault"
	internalfaultstatus "github.com/credo-go/credo/internal/faultstatus"
	internali18n "github.com/credo-go/credo/internal/i18n"
	"github.com/credo-go/credo/validation"
)

// MsgKey constants define i18n message keys for standard HTTP errors.
// These keys are used in locale files (e.g., locales/en/messages.json)
// and as lookup keys for [builtInMessages].
const (
	MsgKeyBadRequest          = "http.bad_request"
	MsgKeyUnauthorized        = "http.unauthorized"
	MsgKeyForbidden           = "http.forbidden"
	MsgKeyNotFound            = "http.not_found"
	MsgKeyMethodNotAllowed    = "http.method_not_allowed"
	MsgKeyConflict            = "http.conflict"
	MsgKeyUnprocessableEntity = "http.unprocessable_entity"
	MsgKeyUnsupportedMedia    = "http.unsupported_media_type"
	MsgKeyInternalError       = "http.internal_server_error"
	MsgKeyTooManyRequests     = "http.too_many_requests"
	MsgKeyServiceUnavailable  = "http.service_unavailable"
	MsgKeyGatewayTimeout      = "http.gateway_timeout"
	MsgKeyRequestTimeout      = "http.request_timeout"
	MsgKeyValidationFailed    = "http.validation_failed"
	MsgKeyBindFailed          = "http.bind_failed"
)

// builtInMessages maps MsgKey constants to default English messages.
// Used as fallback when i18n is not configured or the key is not found
// in locale files.
var builtInMessages = map[string]string{
	MsgKeyBadRequest:          "Bad Request",
	MsgKeyUnauthorized:        "Unauthorized",
	MsgKeyForbidden:           "Forbidden",
	MsgKeyNotFound:            "Not Found",
	MsgKeyMethodNotAllowed:    "Method Not Allowed",
	MsgKeyConflict:            "Conflict",
	MsgKeyUnprocessableEntity: "Unprocessable Entity",
	MsgKeyUnsupportedMedia:    "Unsupported Media Type",
	MsgKeyInternalError:       "Internal Server Error",
	MsgKeyTooManyRequests:     "Too Many Requests",
	MsgKeyServiceUnavailable:  "Service Unavailable",
	MsgKeyGatewayTimeout:      "Gateway Timeout",
	MsgKeyRequestTimeout:      "Request Timeout",
	MsgKeyValidationFailed:    "Validation Failed",
	MsgKeyBindFailed:          "Malformed Request",
}

// HTTPError represents an HTTP error carrying a status and a stable
// machine-readable code, with an optional presentation message key.
//
// Code is the primary wire identity; MessageKey only affects the
// human-readable title. When MessageKey is empty, the title resolves through
// the errors.<code> locale lookup, then http.StatusText, then "HTTP <status>".
// When MessageKey is set, the existing three-level chain applies:
//  1. i18n bundle lookup — if a translation exists for MessageKey, use it
//  2. builtInMessages lookup — if MessageKey matches a built-in key, use it
//  3. MessageKey itself — used as-is (works for literal messages)
type HTTPError struct {
	// Status is the HTTP status code.
	Status int `json:"status"`

	// Code is the stable machine-readable error code, rendered as the RFC
	// 7807 "code" extension member. [NewHTTPError] always materializes it:
	// either the explicit code argument or the frozen default for the status
	// (for example 404 → "not_found", unknown 499 → "http_499"). It must
	// satisfy the machine-code grammar ^[a-z0-9]+(_[a-z0-9]+)*$.
	Code string `json:"code,omitempty"`

	// MessageKey is an optional i18n message key or literal fallback message
	// used only for title presentation; it never contributes to Code. Attach
	// one with [HTTPError.WithMessageKey].
	MessageKey string `json:"message_key,omitempty"`

	// Details carries optional structured, client-safe detail rendered as the
	// RFC 7807 "details" extension member. It is encoded with the
	// application's JSON profile; never place secrets or internal state here.
	Details any `json:"-"`

	// Internal is the underlying error (not exposed to the client).
	Internal error `json:"-"`
}

// NewHTTPError creates a new HTTPError with the given status and optional
// stable machine-readable code. With no code argument, the frozen default
// code for the status is used (404 → "not_found"; a status outside the
// frozen table yields "http_<status>"). MessageKey starts empty; attach a
// presentation key with [HTTPError.WithMessageKey].
//
// The status domain is the full valid range 100..999, not just the error
// classes: NewHTTPError(200) is accepted and defaults to code "ok".
// Restricting construction to 4xx/5xx semantics is the caller's concern.
//
// Misuse panics — this is a developer invariant violation, not a runtime
// condition: a status outside 100..999, more than one code argument, or a
// code that fails the machine-code grammar ^[a-z0-9]+(_[a-z0-9]+)*$ (which
// includes an explicitly empty code). During a request, built-in recovery
// converts the panic into a generic 500 without publishing the invalid value.
func NewHTTPError(status int, code ...string) *HTTPError {
	if !isValidHTTPStatus(status) {
		panic(fmt.Sprintf("credo: NewHTTPError: status %d is outside the valid HTTP status domain 100..999", status))
	}
	switch len(code) {
	case 0:
		return &HTTPError{Status: status, Code: defaultCodeForStatus(status)}
	case 1:
		if !isValidErrorCode(code[0]) {
			panic(fmt.Sprintf("credo: NewHTTPError: %q is not a valid machine code (want ^[a-z0-9]+(_[a-z0-9]+)*$); since v0.11.0 the second argument is the machine code — attach message keys or literal text with WithMessageKey", code[0]))
		}
		return &HTTPError{Status: status, Code: code[0]}
	default:
		panic("credo: NewHTTPError: at most one machine code argument is allowed")
	}
}

// Error implements the error interface. The diagnostic order is status,
// code, key, internal; empty segments are omitted.
func (e *HTTPError) Error() string {
	s := fmt.Sprintf("status=%d", e.Status)
	if e.Code != "" {
		s += ", code=" + e.Code
	}
	if e.MessageKey != "" {
		s += ", key=" + e.MessageKey
	}
	if e.Internal != nil {
		s += fmt.Sprintf(", internal=%v", e.Internal)
	}
	return s
}

// HTTPStatus returns the HTTP status code carried by the error.
func (e *HTTPError) HTTPStatus() int {
	return e.Status
}

// Unwrap returns the internal error, supporting errors.Is/As.
func (e *HTTPError) Unwrap() error {
	return e.Internal
}

// clone returns a shallow copy, backing the copy-on-write With* methods so
// shared sentinels are never mutated.
func (e *HTTPError) clone() *HTTPError {
	c := *e
	return &c
}

// WithInternal returns a copy of the error with the internal error set.
func (e *HTTPError) WithInternal(err error) *HTTPError {
	c := e.clone()
	c.Internal = err
	return c
}

// WithMessageKey returns a copy of the error with the presentation message
// key set. The key affects only the human-readable title (resolved through
// the i18n bundle → builtInMessages → literal chain); the machine-readable
// Code is untouched.
func (e *HTTPError) WithMessageKey(key string) *HTTPError {
	c := e.clone()
	c.MessageKey = key
	return c
}

// WithDetails returns a copy of the error with structured client-safe detail
// attached; it is rendered as the RFC 7807 "details" extension member.
func (e *HTTPError) WithDetails(v any) *HTTPError {
	c := e.clone()
	c.Details = v
	return c
}

// Sentinel errors for common HTTP error conditions.
//
// These are shared package-level instances, like [io.EOF]: compare with
// [errors.Is] and treat them as immutable. Mutating a sentinel's fields
// would silently change the behavior of every handler in the process.
// To attach context, derive a copy instead — [HTTPError.WithInternal]
// for a wrapped cause, or [NewHTTPError] for a different status or
// message key.
var (
	ErrNotFound             = NewHTTPError(http.StatusNotFound)
	ErrMethodNotAllowed     = NewHTTPError(http.StatusMethodNotAllowed)
	ErrBadRequest           = NewHTTPError(http.StatusBadRequest)
	ErrUnauthorized         = NewHTTPError(http.StatusUnauthorized)
	ErrForbidden            = NewHTTPError(http.StatusForbidden)
	ErrInternalServerError  = NewHTTPError(http.StatusInternalServerError)
	ErrConflict             = NewHTTPError(http.StatusConflict)
	ErrUnprocessableEntity  = NewHTTPError(http.StatusUnprocessableEntity)
	ErrUnsupportedMediaType = NewHTTPError(http.StatusUnsupportedMediaType)
)

// ProblemDetails represents an RFC 7807 Problem Details response.
type ProblemDetails struct {
	// Type is a URI reference that identifies the problem type.
	// Defaults to "about:blank" per RFC 7807.
	Type string `json:"type"`

	// Title is a short, human-readable summary of the problem type.
	Title string `json:"title"`

	// Status is the HTTP status code.
	Status int `json:"status"`

	// Detail is a human-readable explanation specific to this occurrence.
	Detail string `json:"detail,omitempty"`

	// Instance is a URI reference that identifies the specific occurrence.
	Instance string `json:"instance,omitempty"`

	// Code is a machine-readable error code (RFC 7807 extension member).
	// Populated from [HTTPError.Code]; the classification default comes from
	// the frozen statusToCode table. Never derived from the message key.
	Code string `json:"code,omitempty"`

	// Details carries structured, client-safe detail about this occurrence
	// (RFC 7807 extension member). Populated from [HTTPError.Details].
	Details any `json:"details,omitempty"`

	// Errors holds field-level validation errors (if any).
	Errors []validation.ValidationError `json:"errors,omitempty"`
}

// NewProblemDetails creates a new ProblemDetails with the given status and title.
// Type defaults to "about:blank" per RFC 7807.
func NewProblemDetails(status int, title string) *ProblemDetails {
	return &ProblemDetails{
		Type:   "about:blank",
		Title:  title,
		Status: status,
	}
}

// builtinErrorHandler is a middleware that catches errors returned by the
// handler chain and writes the error response inline via [App.handleError].
// It sits between builtinAccessLog and the global middleware chain in
// compile(), ensuring that the access log's deferred read of
// [Response.Status], [Response.Size], and duration reflects the final
// committed response — including error responses.
func (app *App) builtinErrorHandler(next Handler) Handler {
	return func(ctx *Context) error {
		if err := next(ctx); err != nil {
			app.handleError(err, ctx)
		}
		app.warnEnvelopeBypass(ctx)
		return nil
	}
}

// handleError is the internal error handling pipeline. It performs:
//  1. Panic recovery (if ErrorRenderer panics, logs and sends 500)
//  2. Hijacked/committed guard (logs warning if the HTTP response is no longer writable)
//  3. Error classification via classifyError
//  4. Server error logging (5xx HTTPErrors with Internal, unhandled errors)
//  5. ErrorRenderer dispatch (renderer is called even for HEAD — can set headers)
//  6. Body write (HEAD → status only; renderer body → JSON; nil → default RFC 7807)
func (app *App) handleError(err error, ctx *Context) {
	defer app.recoverErrorRendererPanic(err, ctx)

	if ctx.Response().Hijacked() {
		ctx.Logger().LogAttrs(ctx.Request().Context(), slog.LevelWarn,
			"credo: error after response hijacked", slog.Any("error", err))
		return
	}
	if ctx.Response().Committed() {
		ctx.Logger().LogAttrs(ctx.Request().Context(), slog.LevelWarn,
			"credo: error after response committed", slog.Any("error", err))
		return
	}

	key, pd := app.classifyError(err, ctx)
	pd.Instance = ctx.Request().URL.Path

	app.logServerError(err, pd.Status, ctx)
	app.renderError(ctx, ErrorInfo{
		Err:        err,
		MessageKey: key,
		Problem:    pd,
	})
}

func (app *App) recoverErrorRendererPanic(err error, ctx *Context) {
	if r := recover(); r != nil {
		ctx.Logger().LogAttrs(ctx.Request().Context(), slog.LevelError,
			"credo: ErrorRenderer panic", slog.Any("panic", r), slog.Any("error", err))
		if !ctx.Response().Hijacked() && !ctx.Response().Committed() {
			_, pd := codedProblem(ctx, http.StatusInternalServerError, "", "")
			// A marshal failure inside panic recovery is deliberately
			// swallowed; there is no safer response left to attempt.
			writeProblemDetails(ctx, pd) //nolint:errcheck
		}
	}
}

func (app *App) logServerError(err error, status int, ctx *Context) {
	if status < 500 {
		return
	}

	message := "credo: server error"
	if _, isHTTPError := errors.AsType[*HTTPError](err); !isHTTPError {
		_, isFault := fault.ProviderOf(err)
		_, hasLegacyStatus := asHTTPStatus(err)
		if !isFault && !hasLegacyStatus {
			message = "credo: unhandled error"
		}
	}

	logErr := err
	if he, ok := errors.AsType[*HTTPError](err); ok && he.Internal != nil {
		logErr = he.Internal
	}
	ctx.Logger().LogAttrs(ctx.Request().Context(), slog.LevelError,
		message, slog.Int("status", status), slog.Any("error", logErr))
}

func (app *App) renderError(ctx *Context, info ErrorInfo) {
	var body any
	if app.errorRenderer != nil {
		body = app.errorRenderer(ctx, info)
		if ctx.Response().Hijacked() || ctx.Response().Committed() {
			// The renderer took full control and wrote the response itself;
			// the returned body, if any, is irrelevant by contract.
			return
		}
	}

	// The renderer may have mutated info.Problem (typically Status), so the
	// pointer is read only after it returns.
	pd := info.Problem
	if ctx.Request().Method == http.MethodHead {
		// Renderer-set headers are preserved; a HEAD response carries no body.
		_ = ctx.Response().NoContent(pd.Status)
		return
	}
	if body != nil {
		// Framework-internal write: the error pipeline is not a success-
		// envelope bypass.
		ctx.Response().exemptJSON = true
		defer func() { ctx.Response().exemptJSON = false }()
		if err := ctx.Response().JSON(pd.Status, body); err != nil {
			ctx.Logger().LogAttrs(ctx.Request().Context(), slog.LevelError,
				"credo: failed to write error response", slog.Any("error", err))
		}
		return
	}
	defaultRenderError(ctx, pd)
}

// classifyError converts an error into an effective message key and
// [ProblemDetails].
//
// Classification order:
//  1. validation.Errors → 422 Unprocessable Entity with field errors
//  2. *BindError → 400 Bad Request with a typed decode-reason errors entry
//  3. *HTTPError → status/code from the error, title from MessageKey or the
//     errors.<code> chain; invalid stored fields fail closed to a generic 500
//  4. fault.Provider → default root transport policy for the semantic kind
//  5. HTTPStatus() int interface → legacy or explicit transport status;
//     out-of-domain statuses fail closed to a generic 500
//  6. Any other error → 500 Internal Server Error (message not leaked)
//
// Every branch resolves its effective code and title through [codedProblem],
// so the default pipeline always emits a non-empty machine code.
func (app *App) classifyError(err error, ctx *Context) (string, *ProblemDetails) {
	if ve, ok := errors.AsType[validation.Errors](err); ok {
		if app.i18nBundle != nil && ctx.locale != "" {
			ve = translateValidationErrors(app.i18nBundle, ctx.locale, ve)
		}
		return MsgKeyValidationFailed, &ProblemDetails{
			Type:   "https://credo.dev/errors/validation",
			Title:  resolveMessage(ctx, MsgKeyValidationFailed),
			Status: http.StatusUnprocessableEntity,
			Code:   "validation_failed",
			Errors: []validation.ValidationError(ve),
		}
	}

	if be, ok := errors.AsType[*BindError](err); ok {
		return MsgKeyBindFailed, &ProblemDetails{
			Type:   "https://credo.dev/errors/binding",
			Title:  resolveMessage(ctx, MsgKeyBindFailed),
			Status: http.StatusBadRequest,
			Code:   string(be.Reason),
			Errors: []validation.ValidationError{app.bindProblemError(ctx, be)},
		}
	}

	if he, ok := errors.AsType[*HTTPError](err); ok {
		// Fail closed on invalid directly constructed values: rebuild a
		// generic internal-server problem and publish none of the invalid
		// value's client-facing fields.
		if !isValidHTTPStatus(he.Status) || (he.Code != "" && !isValidErrorCode(he.Code)) {
			return codedProblem(ctx, http.StatusInternalServerError, "", "")
		}
		key, pd := codedProblem(ctx, he.Status, he.Code, he.MessageKey)
		pd.Details = he.Details
		return key, pd
	}

	if provider, ok := fault.ProviderOf(err); ok {
		status, known := internalfaultstatus.HTTP(provider.FaultKind())
		if !known {
			return codedProblem(ctx, http.StatusInternalServerError, "", "")
		}
		return codedProblem(ctx, status, "", "")
	}

	if se, ok := asHTTPStatus(err); ok {
		status := se.HTTPStatus()
		if !isValidHTTPStatus(status) {
			return codedProblem(ctx, http.StatusInternalServerError, "", "")
		}
		return codedProblem(ctx, status, "", "")
	}

	return codedProblem(ctx, http.StatusInternalServerError, "", "")
}

// codedProblem is the single source of the effective-code and effective-title
// calculation for the HTTPError, fault, legacy-status, and generic-500
// classification branches. The effective code is the explicit code when
// non-empty, otherwise the frozen default for the status. With an explicit
// message key the title resolves through the existing three-level chain and
// the key is returned as the effective key; without one the effective key is
// "errors.<code>" and the title resolves as locale bundle lookup for that key,
// then http.StatusText, then "HTTP <status>".
func codedProblem(ctx *Context, status int, explicitCode, explicitKey string) (string, *ProblemDetails) {
	code := explicitCode
	if code == "" {
		code = defaultCodeForStatus(status)
	}

	if explicitKey != "" {
		pd := NewProblemDetails(status, resolveMessage(ctx, explicitKey))
		pd.Code = code
		return explicitKey, pd
	}

	key := "errors." + code
	title := ""
	if ctx.app != nil && ctx.app.i18nBundle != nil && ctx.locale != "" {
		if s, ok := ctx.app.i18nBundle.TranslateForLang(ctx.locale, key, nil); ok {
			title = s
		}
	}
	if title == "" {
		title = http.StatusText(status)
	}
	if title == "" {
		title = fmt.Sprintf("HTTP %d", status)
	}
	pd := NewProblemDetails(status, title)
	pd.Code = code
	return key, pd
}

// defaultRenderError writes an RFC 7807 Problem Details JSON response.
func defaultRenderError(ctx *Context, pd *ProblemDetails) {
	if err := writeProblemDetails(ctx, pd); err != nil {
		ctx.Logger().LogAttrs(ctx.Request().Context(), slog.LevelError,
			"credo: failed to write error response", slog.Any("error", err))
	}
}

// writeProblemDetails commits a Problem Details response and returns the
// marshal error, letting callers choose their own failure policy.
func writeProblemDetails(ctx *Context, pd *ProblemDetails) error {
	ctx.Response().Header().Set("Content-Type", "application/problem+json")
	ctx.Response().WriteHeader(pd.Status)
	return jsonv2.MarshalWrite(ctx.Response(), pd, ctx.app.problemJSONOptions())
}

// resolveMessage resolves a message key to a human-readable string using
// a 3-level fallback: i18n bundle → builtInMessages → key itself.
func resolveMessage(ctx *Context, key string) string {
	// 1. i18n bundle
	if ctx.app != nil && ctx.app.i18nBundle != nil && ctx.locale != "" {
		if s, ok := ctx.app.i18nBundle.TranslateForLang(ctx.locale, key, nil); ok {
			return s
		}
	}
	// 2. built-in fallback
	if msg, ok := builtInMessages[key]; ok {
		return msg
	}
	// 3. key itself
	return key
}

// httpStatusProvider is implemented by errors that carry an HTTP status code.
// This interface is detected via errors.As without requiring the error handler
// to import the package that defines the error.
type httpStatusProvider interface {
	error
	HTTPStatus() int
}

// asHTTPStatus extracts an httpStatusProvider from err's chain.
func asHTTPStatus(err error) (httpStatusProvider, bool) {
	return errors.AsType[httpStatusProvider](err)
}

// translateValidationErrors translates each validation error using the bundle.
func translateValidationErrors(bundle *internali18n.Bundle, lang string, ve validation.Errors) validation.Errors {
	result := make(validation.Errors, len(ve))
	for i, e := range ve {
		result[i] = e // copy

		// Lookup key: "v." + code
		if s, ok := translateFieldMessage(bundle, lang, "v."+e.Code, e.Params, e.Field); ok {
			result[i].Message = s
		}
	}
	return result
}

// translateFieldMessage resolves a field-scoped message through the bundle:
// it copies params, injects the translated field name when field is non-empty,
// and looks up key for lang. The boolean reports whether a translation exists.
// Shared by the validation and bind errors[] flows so their field-name and
// params handling cannot drift.
func translateFieldMessage(bundle *internali18n.Bundle, lang, key string, params map[string]any, field string) (string, bool) {
	data := copyParams(params, field)
	if field != "" {
		if data == nil {
			data = make(map[string]any, 1)
		}
		data["field"] = bundle.FieldNameForLang(lang, field)
	}
	return bundle.TranslateForLang(lang, key, data)
}

// bindProblemError converts a [BindError] into the single errors[] entry of
// the RFC 7807 response. The entry mirrors the validation error shape:
// Code carries the machine-readable reason, Message the localized (or
// default English) text, and Params the client-safe template variables.
// Translation follows the validation pipeline: lookup key "bind.<reason>",
// field names resolved via the bundle's field translations.
func (app *App) bindProblemError(ctx *Context, be *BindError) validation.ValidationError {
	ve := validation.ValidationError{
		Field:   be.Field,
		Code:    string(be.Reason),
		Message: be.message(),
		Params:  be.params(),
	}

	if app.i18nBundle != nil && ctx.locale != "" {
		if s, ok := translateFieldMessage(app.i18nBundle, ctx.locale, "bind."+string(be.Reason), ve.Params, be.Field); ok {
			ve.Message = s
		}
	}

	return ve
}

// copyParams creates a shallow copy of the params map, allocating space for
// an optional field entry.
func copyParams(src map[string]any, field string) map[string]any {
	if src == nil && field == "" {
		return nil
	}
	dst := make(map[string]any, len(src)+1)
	maps.Copy(dst, src)
	return dst
}
