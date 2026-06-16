package credo

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"

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
}

// statusToKey maps HTTP status codes to their MsgKey constants.
// Used by the error handling pipeline to derive a message key from
// errors that only carry a status code (e.g., store errors).
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
	// Code is the HTTP status code.
	Code int `json:"code"`

	// MessageKey is the i18n message key or literal fallback message.
	MessageKey string `json:"message_key"`

	// Internal is the underlying error (not exposed to the client).
	Internal error `json:"-"`
}

// NewHTTPError creates a new HTTPError with the given status code and
// optional message key. If no message key is provided, the corresponding
// MsgKey constant is used (falling back to http.StatusText for unknown codes).
func NewHTTPError(code int, messageKey ...string) *HTTPError {
	e := &HTTPError{Code: code}
	if len(messageKey) > 0 {
		e.MessageKey = messageKey[0]
	} else if key, ok := statusToKey[code]; ok {
		e.MessageKey = key
	} else {
		e.MessageKey = http.StatusText(code)
	}
	return e
}

// Error implements the error interface.
func (e *HTTPError) Error() string {
	if e.Internal != nil {
		return fmt.Sprintf("code=%d, key=%s, internal=%v", e.Code, e.MessageKey, e.Internal)
	}
	return fmt.Sprintf("code=%d, key=%s", e.Code, e.MessageKey)
}

// HTTPStatus returns the HTTP status code carried by the error.
func (e *HTTPError) HTTPStatus() int {
	return e.Code
}

// Unwrap returns the internal error, supporting errors.Is/As.
func (e *HTTPError) Unwrap() error {
	return e.Internal
}

// WithInternal returns a copy of the error with the internal error set.
func (e *HTTPError) WithInternal(err error) *HTTPError {
	return &HTTPError{
		Code:       e.Code,
		MessageKey: e.MessageKey,
		Internal:   err,
	}
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
//  2. Committed guard (logs warning if response already committed)
//  3. Error classification via classifyError
//  4. Server error logging (5xx HTTPErrors with Internal, unhandled errors)
//  5. ErrorRenderer dispatch (renderer is called even for HEAD — can set headers)
//  6. HEAD/fallback guard (if renderer didn't commit: HEAD → NoContent, else → default JSON)
func (app *App) handleError(err error, ctx *Context) {
	defer app.recoverErrorRendererPanic(err, ctx)

	if ctx.Response().Committed() {
		ctx.Logger().LogAttrs(ctx.Request().Context(), slog.LevelWarn,
			"credo: error after response committed", slog.Any("error", err))
		return
	}

	key, pd := app.classifyError(err, ctx)
	pd.Instance = ctx.Request().URL.Path

	app.logServerError(err, ctx)
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
		if !ctx.Response().Committed() {
			ctx.Response().Header().Set("Content-Type", "application/problem+json")
			ctx.Response().WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(ctx.Response()).Encode(NewProblemDetails( //nolint:errcheck
				http.StatusInternalServerError, resolveMessage(ctx, MsgKeyInternalError)))
		}
	}
}

func (app *App) logServerError(err error, ctx *Context) {
	if he, ok := errors.AsType[*HTTPError](err); ok {
		if he.Code >= 500 {
			// Log all 5xx faults, even sentinel errors with no Internal cause —
			// a silent server error is worse than a slightly thin log line.
			logErr := error(he)
			if he.Internal != nil {
				logErr = he.Internal
			}
			ctx.Logger().LogAttrs(ctx.Request().Context(), slog.LevelError,
				"credo: server error", slog.Int("status", he.Code), slog.Any("error", logErr))
		}
		return
	}
	if _, ok := errors.AsType[validation.Errors](err); ok {
		return
	}
	if se, ok := asHTTPStatus(err); ok {
		// Errors carrying an HTTP status (e.g. store errors). Only 5xx are
		// server-side faults worth logging; 4xx are client errors.
		if status := se.HTTPStatus(); status >= 500 {
			ctx.Logger().LogAttrs(ctx.Request().Context(), slog.LevelError,
				"credo: server error", slog.Int("status", status), slog.Any("error", err))
		}
		return
	}
	// Catch-all: unhandled error.
	ctx.Logger().LogAttrs(ctx.Request().Context(), slog.LevelError,
		"credo: unhandled error", slog.Any("error", err))
}

func (app *App) renderError(ctx *Context, info ErrorInfo) {
	pd := info.Problem
	isHEAD := ctx.Request().Method == http.MethodHead

	// Dispatch to ErrorRenderer or default.
	if app.errorRenderer != nil {
		app.errorRenderer(ctx, info)
		if !ctx.Response().Committed() {
			if isHEAD {
				_ = ctx.Response().NoContent(pd.Status)
			} else {
				ctx.Logger().LogAttrs(ctx.Request().Context(), slog.LevelWarn,
					"credo: ErrorRenderer did not write response, falling back to default")
				defaultRenderError(ctx, pd)
			}
		}
		return
	}

	// No custom renderer — use default.
	if isHEAD {
		_ = ctx.Response().NoContent(pd.Status)
		return
	}
	defaultRenderError(ctx, pd)
}

// classifyError converts an error into a message key and [ProblemDetails].
//
// Classification order:
//  1. validation.Errors → 422 Unprocessable Entity with field errors
//  2. *HTTPError → status from Code, title resolved from MessageKey
//  3. HTTPStatus() int interface → status from HTTPStatus() (e.g., store errors)
//  4. Any other error → 500 Internal Server Error (message not leaked)
func (app *App) classifyError(err error, ctx *Context) (string, *ProblemDetails) {
	if ve, ok := errors.AsType[validation.Errors](err); ok {
		if app.i18nBundle != nil && ctx.locale != "" {
			ve = translateValidationErrors(app.i18nBundle, ctx.locale, ve)
		}
		return MsgKeyValidationFailed, &ProblemDetails{
			Type:   "https://credo.dev/errors/validation",
			Title:  resolveMessage(ctx, MsgKeyValidationFailed),
			Status: http.StatusUnprocessableEntity,
			Errors: []validation.ValidationError(ve),
		}
	}

	if he, ok := errors.AsType[*HTTPError](err); ok {
		return he.MessageKey, NewProblemDetails(he.Code, resolveMessage(ctx, he.MessageKey))
	}

	if se, ok := asHTTPStatus(err); ok {
		status := se.HTTPStatus()
		key := statusToKey[status]
		if key == "" {
			key = http.StatusText(status)
		}
		return key, NewProblemDetails(status, resolveMessage(ctx, key))
	}

	return MsgKeyInternalError, NewProblemDetails(
		http.StatusInternalServerError,
		resolveMessage(ctx, MsgKeyInternalError),
	)
}

// defaultRenderError writes an RFC 7807 Problem Details JSON response.
func defaultRenderError(ctx *Context, pd *ProblemDetails) {
	ctx.Response().Header().Set("Content-Type", "application/problem+json")
	ctx.Response().WriteHeader(pd.Status)
	if err := json.NewEncoder(ctx.Response()).Encode(pd); err != nil {
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
// to import the package that defines the error (e.g., store/).
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
