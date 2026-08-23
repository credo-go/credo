package credo

import (
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"strings"

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

// statusToKey maps HTTP status codes to their MsgKey constants.
// Used by the error handling pipeline to derive a message key from
// HTTPError and legacy/explicit errors that carry only a status code.
var statusToKey = map[int]string{
	http.StatusBadRequest:           MsgKeyBadRequest,
	http.StatusUnauthorized:         MsgKeyUnauthorized,
	http.StatusForbidden:            MsgKeyForbidden,
	http.StatusNotFound:             MsgKeyNotFound,
	http.StatusMethodNotAllowed:     MsgKeyMethodNotAllowed,
	http.StatusConflict:             MsgKeyConflict,
	http.StatusUnsupportedMediaType: MsgKeyUnsupportedMedia,
	http.StatusUnprocessableEntity:  MsgKeyUnprocessableEntity,
	http.StatusTooManyRequests:      MsgKeyTooManyRequests,
	http.StatusRequestTimeout:       MsgKeyRequestTimeout,
	http.StatusInternalServerError:  MsgKeyInternalError,
	http.StatusServiceUnavailable:   MsgKeyServiceUnavailable,
	http.StatusGatewayTimeout:       MsgKeyGatewayTimeout,
}

// HTTPError represents an HTTP error with a status code and a message key.
// The MessageKey field serves as both the i18n translation key and the
// fallback message when no translation is found.
//
// Resolution order for MessageKey (applied in the error handling pipeline):
//  1. i18n bundle lookup — if a translation exists for MessageKey, use it
//  2. builtInMessages lookup — if MessageKey matches a built-in key, use it
//  3. MessageKey itself — used as-is (works for literal messages)
type HTTPError struct {
	// Status is the HTTP status code.
	Status int `json:"status"`

	// Code is an optional machine-readable error code, rendered as the RFC
	// 7807 "code" extension member. When empty, the pipeline derives it from
	// MessageKey: the segment after the last dot ("user.email_exists" →
	// "email_exists"); a key without a dot is treated as a literal human
	// message and yields no code.
	Code string `json:"code,omitempty"`

	// MessageKey is the i18n message key or literal fallback message.
	MessageKey string `json:"message_key"`

	// Details carries optional structured, client-safe detail rendered as the
	// RFC 7807 "details" extension member. It is encoded with the
	// application's JSON profile; never place secrets or internal state here.
	Details any `json:"-"`

	// Internal is the underlying error (not exposed to the client).
	Internal error `json:"-"`
}

// NewHTTPError creates a new HTTPError with the given status code and
// optional message key. If no message key is provided, the corresponding
// MsgKey constant is used (falling back to http.StatusText for unknown codes).
func NewHTTPError(status int, messageKey ...string) *HTTPError {
	e := &HTTPError{Status: status}
	if len(messageKey) > 0 {
		e.MessageKey = messageKey[0]
	} else if key, ok := statusToKey[status]; ok {
		e.MessageKey = key
	} else {
		e.MessageKey = http.StatusText(status)
	}
	return e
}

// Error implements the error interface.
func (e *HTTPError) Error() string {
	s := fmt.Sprintf("status=%d, key=%s", e.Status, e.MessageKey)
	if e.Code != "" {
		s += ", code=" + e.Code
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

// WithCode returns a copy of the error with the machine-readable code set,
// overriding the default derivation from MessageKey.
func (e *HTTPError) WithCode(code string) *HTTPError {
	c := e.clone()
	c.Code = code
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
	// Populated from [HTTPError.Code] or derived from the message key.
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
			ctx.Response().Header().Set("Content-Type", "application/problem+json")
			ctx.Response().WriteHeader(http.StatusInternalServerError)
			jsonv2.MarshalWrite(ctx.Response(), //nolint:errcheck
				newKeyedProblem(ctx, http.StatusInternalServerError, MsgKeyInternalError),
				app.problemJSONOptions())
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
		if err := ctx.Response().JSON(pd.Status, body); err != nil {
			ctx.Logger().LogAttrs(ctx.Request().Context(), slog.LevelError,
				"credo: failed to write error response", slog.Any("error", err))
		}
		return
	}
	defaultRenderError(ctx, pd)
}

// classifyError converts an error into a message key and [ProblemDetails].
//
// Classification order:
//  1. validation.Errors → 422 Unprocessable Entity with field errors
//  2. *BindError → 400 Bad Request with a typed decode-reason errors entry
//  3. *HTTPError → status from Code, title resolved from MessageKey
//  4. fault.Provider → default root transport policy for the semantic kind
//  5. HTTPStatus() int interface → legacy or explicit transport status
//  6. Any other error → 500 Internal Server Error (message not leaked)
func (app *App) classifyError(err error, ctx *Context) (string, *ProblemDetails) {
	if ve, ok := errors.AsType[validation.Errors](err); ok {
		if app.i18nBundle != nil && ctx.locale != "" {
			ve = translateValidationErrors(app.i18nBundle, ctx.locale, ve)
		}
		return MsgKeyValidationFailed, &ProblemDetails{
			Type:   "https://credo.dev/errors/validation",
			Title:  resolveMessage(ctx, MsgKeyValidationFailed),
			Status: http.StatusUnprocessableEntity,
			Code:   deriveErrorCode(MsgKeyValidationFailed),
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
		pd := NewProblemDetails(he.Status, resolveMessage(ctx, he.MessageKey))
		pd.Code = he.Code
		if pd.Code == "" {
			pd.Code = deriveErrorCode(he.MessageKey)
		}
		pd.Details = he.Details
		return he.MessageKey, pd
	}

	if provider, ok := fault.ProviderOf(err); ok {
		status, known := internalfaultstatus.HTTP(provider.FaultKind())
		if !known {
			return MsgKeyInternalError, newKeyedProblem(ctx, http.StatusInternalServerError, MsgKeyInternalError)
		}
		key := statusToKey[status]
		if key == "" {
			key = http.StatusText(status)
		}
		return key, newKeyedProblem(ctx, status, key)
	}

	if se, ok := asHTTPStatus(err); ok {
		status := se.HTTPStatus()
		key := statusToKey[status]
		if key == "" {
			key = http.StatusText(status)
		}
		return key, newKeyedProblem(ctx, status, key)
	}

	return MsgKeyInternalError, newKeyedProblem(ctx, http.StatusInternalServerError, MsgKeyInternalError)
}

// newKeyedProblem builds a ProblemDetails whose title and machine-readable
// code both come from the same message key.
func newKeyedProblem(ctx *Context, status int, key string) *ProblemDetails {
	pd := NewProblemDetails(status, resolveMessage(ctx, key))
	pd.Code = deriveErrorCode(key)
	return pd
}

// deriveErrorCode derives the default machine-readable error code from a
// message key: the segment after the last dot ("user.email_exists" →
// "email_exists", "http.not_found" → "not_found"). A key without a dot is a
// literal human message rather than a code namespace, so no code is derived.
func deriveErrorCode(key string) string {
	if i := strings.LastIndexByte(key, '.'); i >= 0 {
		return key[i+1:]
	}
	return ""
}

// defaultRenderError writes an RFC 7807 Problem Details JSON response.
func defaultRenderError(ctx *Context, pd *ProblemDetails) {
	ctx.Response().Header().Set("Content-Type", "application/problem+json")
	ctx.Response().WriteHeader(pd.Status)
	if err := jsonv2.MarshalWrite(ctx.Response(), pd, ctx.app.problemJSONOptions()); err != nil {
		ctx.Logger().LogAttrs(ctx.Request().Context(), slog.LevelError,
			"credo: failed to write error response", slog.Any("error", err))
	}
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
		key := "v." + e.Code
		data := copyParams(e.Params, e.Field)

		// Inject translated field name if available.
		if e.Field != "" {
			if data == nil {
				data = make(map[string]any, 1)
			}
			data["field"] = bundle.FieldNameForLang(lang, e.Field)
		}

		if s, ok := bundle.TranslateForLang(lang, key, data); ok {
			result[i].Message = s
		}
	}
	return result
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
		data := copyParams(ve.Params, be.Field)
		if be.Field != "" {
			if data == nil {
				data = make(map[string]any, 1)
			}
			data["field"] = app.i18nBundle.FieldNameForLang(ctx.locale, be.Field)
		}
		if s, ok := app.i18nBundle.TranslateForLang(ctx.locale, "bind."+string(be.Reason), data); ok {
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
