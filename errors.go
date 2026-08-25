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

// MsgKey constants define Credo's bare default message keys for standard HTTP
// errors. Applications may map them into their own namespaces with
// [I18nConfig.ResolveMessageKey].
const (
	MsgKeyBadRequest          = "bad_request"
	MsgKeyUnauthorized        = "unauthorized"
	MsgKeyForbidden           = "forbidden"
	MsgKeyNotFound            = "not_found"
	MsgKeyMethodNotAllowed    = "method_not_allowed"
	MsgKeyConflict            = "conflict"
	MsgKeyUnprocessableEntity = "unprocessable_entity"
	MsgKeyUnsupportedMedia    = "unsupported_media_type"
	MsgKeyInternalError       = "internal_server_error"
	MsgKeyTooManyRequests     = "too_many_requests"
	MsgKeyServiceUnavailable  = "service_unavailable"
	MsgKeyGatewayTimeout      = "gateway_timeout"
	MsgKeyRequestTimeout      = "request_timeout"
	MsgKeyValidationFailed    = "validation_failed"
	MsgKeyBindFailed          = "bind_failed"
)

// builtInMessages maps framework error codes to safe default English messages.
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
// human-readable message. When MessageKey is empty, the message resolves
// through the configured scoped resolver or bare Code lookup, then the safe
// built-in/status fallback.
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
	// default error response's "details" member. It is encoded with the
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
// attached; it is rendered as the default response's "details" member.
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

// ErrorResponse is Credo's default JSON error envelope.
type ErrorResponse struct {
	Success bool                         `json:"success"`
	Code    string                       `json:"code"`
	Message string                       `json:"message"`
	Details any                          `json:"details,omitempty"`
	Errors  []validation.ValidationError `json:"errors,omitempty"`
}

// ProblemDetails represents an RFC 9457 Problem Details response. It is used
// by the opt-in [RFC9457ErrorRenderer]; Credo's default envelope is
// [ErrorResponse].
type ProblemDetails struct {
	// Type is a URI reference that identifies the problem type.
	// Defaults to "about:blank" per RFC 9457.
	Type string `json:"type"`

	// Title is a short, human-readable summary of the problem type.
	Title string `json:"title"`

	// Status is the HTTP status code.
	Status int `json:"status"`

	// Detail is a human-readable explanation specific to this occurrence.
	Detail string `json:"detail,omitempty"`

	// Instance is a URI reference that identifies the specific occurrence.
	Instance string `json:"instance,omitempty"`

	// Code is a machine-readable error-code extension member.
	// Populated from [HTTPError.Code]; the classification default comes from
	// the frozen statusToCode table. Never derived from the message key.
	Code string `json:"code,omitempty"`

	// Details carries structured, client-safe detail about this occurrence as
	// an extension member. Populated from [HTTPError.Details].
	Details any `json:"details,omitempty"`

	// Errors holds field-level validation errors (if any).
	Errors []validation.ValidationError `json:"errors,omitempty"`
}

// NewProblemDetails creates a new ProblemDetails with the given status and title.
// Type defaults to "about:blank" per RFC 9457.
func NewProblemDetails(status int, title string) *ProblemDetails {
	return &ProblemDetails{
		Type:   "about:blank",
		Title:  title,
		Status: status,
	}
}

// RFC9457Config configures [RFC9457ErrorRenderer].
type RFC9457Config struct {
	// ResolveType optionally returns the problem type URI for a normalized error.
	// Empty means "about:blank". The callback is request-scoped and must not
	// retain info.
	ResolveType func(info *ErrorInfo) string
}

// RFC9457ErrorRenderer returns an ErrorRenderer that projects Credo's
// normalized error model into RFC 9457 Problem Details. The response uses
// application/problem+json; Code, Details, and Errors are extension members.
func RFC9457ErrorRenderer(cfgs ...RFC9457Config) ErrorRenderer {
	if len(cfgs) > 1 {
		panic("credo: RFC9457ErrorRenderer accepts at most one config")
	}
	var cfg RFC9457Config
	if len(cfgs) == 1 {
		cfg = cfgs[0]
	}
	return func(ctx *Context, info *ErrorInfo) any {
		problemType := "about:blank"
		if cfg.ResolveType != nil {
			if resolved := cfg.ResolveType(info); resolved != "" {
				problemType = resolved
			}
		}
		title := http.StatusText(info.Status)
		if title == "" {
			title = fmt.Sprintf("HTTP %d", info.Status)
		}
		detail := info.Message
		if detail == title {
			detail = ""
		}
		ctx.Response().Header().Set("Content-Type", "application/problem+json")
		return &ProblemDetails{
			Type:     problemType,
			Title:    title,
			Status:   info.Status,
			Detail:   detail,
			Instance: ctx.Request().URL.Path,
			Code:     info.Code,
			Details:  info.Details,
			Errors:   info.Errors,
		}
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
//  6. Body write (HEAD → status only; renderer body → JSON; nil → default envelope)
func (app *App) handleError(err error, ctx *Context) {
	defer app.recoverErrorPipelinePanic(err, ctx)

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

	info := app.classifyError(err, ctx)
	info.Err = err
	app.logServerError(err, info.Status, ctx)
	app.renderError(ctx, info)
}

func (app *App) recoverErrorPipelinePanic(err error, ctx *Context) {
	if r := recover(); r != nil {
		ctx.Logger().LogAttrs(ctx.Request().Context(), slog.LevelError,
			"credo: error pipeline panic", slog.Any("panic", r), slog.Any("error", err))
		if !ctx.Response().Hijacked() && !ctx.Response().Committed() {
			// A marshal failure inside panic recovery is deliberately
			// swallowed; there is no safer response left to attempt.
			writeDefaultError(ctx, safeInternalErrorInfo()) //nolint:errcheck
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

func (app *App) renderError(ctx *Context, info *ErrorInfo) {
	var body any
	if app.errorRenderer != nil {
		body = app.errorRenderer(ctx, info)
		if ctx.Response().Hijacked() || ctx.Response().Committed() {
			// The renderer took full control and wrote the response itself;
			// the returned body, if any, is irrelevant by contract.
			return
		}
	}

	if !isValidHTTPStatus(info.Status) {
		ctx.Logger().LogAttrs(ctx.Request().Context(), slog.LevelError,
			"credo: ErrorRenderer returned an invalid status",
			slog.Int("status", info.Status), slog.Any("error", info.Err))
		info = safeInternalErrorInfo()
		body = nil
	}
	if ctx.Request().Method == http.MethodHead {
		// Renderer-set headers are preserved; a HEAD response carries no body.
		_ = ctx.Response().NoContent(info.Status)
		return
	}
	if bodilessStatus(info.Status) {
		_ = ctx.Response().NoContent(info.Status)
		return
	}
	if body != nil {
		// Framework-internal write: the error pipeline is not a success-
		// envelope bypass.
		ctx.Response().exemptJSON = true
		defer func() { ctx.Response().exemptJSON = false }()
		if err := writeRenderedError(ctx, info.Status, body); err != nil {
			ctx.Logger().LogAttrs(ctx.Request().Context(), slog.LevelError,
				"credo: failed to write error response", slog.Any("error", err))
		}
		return
	}
	defaultRenderError(ctx, info)
}

func writeRenderedError(ctx *Context, status int, body any) error {
	if ctx.Response().Header().Get("Content-Type") == "" {
		ctx.Response().Header().Set("Content-Type", "application/json; charset=utf-8")
	}
	ctx.Response().WriteHeader(status)
	return jsonv2.MarshalWrite(ctx.Response(), body, ctx.app.jsonOptions())
}

// classifyError converts an error into normalized [ErrorInfo].
//
// Classification order:
//  1. validation.Errors → 422 Unprocessable Entity with field errors
//  2. *BindError → 400 Bad Request with a typed decode-reason errors entry
//  3. *HTTPError → status/code from the error and message from exact-key
//     resolution; invalid stored fields fail closed to a generic 500
//  4. fault.Provider → default root transport policy for the semantic kind
//  5. HTTPStatus() int interface → legacy or explicit transport status;
//     out-of-domain statuses fail closed to a generic 500
//  6. Any other error → 500 Internal Server Error (message not leaked)
//
// Every branch resolves its effective code and message through [codedErrorInfo],
// so the default pipeline always emits a non-empty machine code.
func (app *App) classifyError(err error, ctx *Context) *ErrorInfo {
	if ve, ok := errors.AsType[validation.Errors](err); ok {
		info := app.codedErrorInfo(ctx, http.StatusUnprocessableEntity, "validation_failed", "")
		info.Errors = []validation.ValidationError(app.translateValidationErrors(ctx, ve))
		return info
	}

	if be, ok := errors.AsType[*BindError](err); ok {
		info := app.codedErrorInfo(ctx, http.StatusBadRequest, "bind_failed", "")
		info.Errors = []validation.ValidationError{app.bindProblemError(ctx, be)}
		return info
	}

	if he, ok := errors.AsType[*HTTPError](err); ok {
		// Fail closed on invalid directly constructed values: rebuild a
		// generic internal-server problem and publish none of the invalid
		// value's client-facing fields.
		if !isValidHTTPStatus(he.Status) || (he.Code != "" && !isValidErrorCode(he.Code)) {
			return app.codedErrorInfo(ctx, http.StatusInternalServerError, "", "")
		}
		info := app.codedErrorInfo(ctx, he.Status, he.Code, he.MessageKey)
		info.Details = he.Details
		return info
	}

	if provider, ok := fault.ProviderOf(err); ok {
		status, known := internalfaultstatus.HTTP(provider.FaultKind())
		if !known {
			return app.codedErrorInfo(ctx, http.StatusInternalServerError, "", "")
		}
		return app.codedErrorInfo(ctx, status, "", "")
	}

	if se, ok := asHTTPStatus(err); ok {
		status := se.HTTPStatus()
		if !isValidHTTPStatus(status) {
			return app.codedErrorInfo(ctx, http.StatusInternalServerError, "", "")
		}
		return app.codedErrorInfo(ctx, status, "", "")
	}

	return app.codedErrorInfo(ctx, http.StatusInternalServerError, "", "")
}

// codedErrorInfo is the single source of the effective-code and message
// calculation for the HTTPError, fault, legacy-status, and generic-500
// classification branches.
func (app *App) codedErrorInfo(ctx *Context, status int, explicitCode, explicitKey string) *ErrorInfo {
	code := explicitCode
	if code == "" {
		code = defaultCodeForStatus(status)
	}
	key, message := app.resolveErrorMessage(ctx, status, code, explicitKey)
	return &ErrorInfo{Status: status, Code: code, MessageKey: key, Message: message}
}

func safeInternalErrorInfo() *ErrorInfo {
	return &ErrorInfo{
		Status:     http.StatusInternalServerError,
		Code:       "internal_server_error",
		MessageKey: "internal_server_error",
		Message:    "Internal Server Error",
	}
}

// defaultRenderError writes Credo's default JSON error envelope.
func defaultRenderError(ctx *Context, info *ErrorInfo) {
	if err := writeDefaultError(ctx, info); err != nil {
		ctx.Logger().LogAttrs(ctx.Request().Context(), slog.LevelError,
			"credo: failed to write error response", slog.Any("error", err))
	}
}

func writeDefaultError(ctx *Context, info *ErrorInfo) error {
	ctx.Response().Header().Set("Content-Type", "application/json; charset=utf-8")
	ctx.Response().WriteHeader(info.Status)
	return jsonv2.MarshalWrite(ctx.Response(), ErrorResponse{
		Success: false,
		Code:    info.Code,
		Message: info.Message,
		Details: info.Details,
		Errors:  info.Errors,
	}, ctx.app.errorJSONOptions())
}

func (app *App) resolveErrorMessage(ctx *Context, status int, code, explicitKey string) (string, string) {
	key, explicit := app.effectiveMessageKey(MessageScopeError, code, explicitKey)
	if app.i18nBundle != nil && ctx.locale != "" {
		if message, ok := app.i18nBundle.TranslateForLang(ctx.locale, key, nil); ok {
			return key, message
		}
	}
	if explicit {
		if message, ok := builtInMessages[key]; ok {
			return key, message
		}
		return key, key
	}
	if message, ok := builtInMessages[code]; ok {
		return key, message
	}
	if message := http.StatusText(status); message != "" {
		return key, message
	}
	return key, fmt.Sprintf("HTTP %d", status)
}

func (app *App) effectiveMessageKey(scope MessageScope, code, explicitKey string) (string, bool) {
	if explicitKey != "" {
		return explicitKey, true
	}
	if app != nil && app.messageKeyResolver != nil {
		key := app.messageKeyResolver(MessageRef{Scope: scope, Code: code})
		if key == "" {
			panic(fmt.Sprintf("credo: MessageKeyResolver returned an empty key for scope %d code %q", scope, code))
		}
		return key, false
	}
	return code, false
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

// translateValidationErrors resolves each validation error's exact key and
// translates it when a bundle is active.
func (app *App) translateValidationErrors(ctx *Context, ve validation.Errors) validation.Errors {
	result := make(validation.Errors, len(ve))
	for i, e := range ve {
		result[i] = e // copy
		key, _ := app.effectiveMessageKey(MessageScopeValidation, e.Code, e.MessageKey)
		result[i].MessageKey = key
		if app.i18nBundle != nil && ctx.locale != "" {
			if s, ok := translateFieldMessage(app.i18nBundle, ctx.locale, key, e.Params, e.Field); ok {
				result[i].Message = s
			}
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
// the default error response. The entry mirrors the validation error shape:
// Code carries the machine-readable reason, Message the localized (or
// default English) text, and Params the client-safe template variables.
// Translation follows the validation pipeline with a scoped exact key; field
// names are resolved via the bundle's field translations.
func (app *App) bindProblemError(ctx *Context, be *BindError) validation.ValidationError {
	key, _ := app.effectiveMessageKey(MessageScopeBind, string(be.Reason), "")
	ve := validation.ValidationError{
		Field:      be.Field,
		Code:       string(be.Reason),
		Message:    be.message(),
		MessageKey: key,
		Params:     be.params(),
	}

	if app.i18nBundle != nil && ctx.locale != "" {
		if s, ok := translateFieldMessage(app.i18nBundle, ctx.locale, key, ve.Params, be.Field); ok {
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
