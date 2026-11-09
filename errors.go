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

// MessageKey is a catalog key: the exact key of a message in the
// application's translation catalog, never rendered as text. It is
// [validation.MessageKey], whose violations carry a key too.
type MessageKey = validation.MessageKey

// Framework error codes carried on the wire in addition to the status-derived
// defaults (see errorcode.go). A code a status derives needs no constant:
// NewHTTPError(http.StatusNotFound) carries "not_found".
const (
	// CodeValidationFailed is the 422 code for validation errors.
	CodeValidationFailed = "validation_failed"

	// CodeBindFailed is the 400 code for a request body or parameter that
	// cannot be decoded into its target ([BindError]).
	CodeBindFailed = "bind_failed"

	// CodeUnsupportedContentEncoding is the 415 code for a request body whose
	// Content-Encoding the decompression feature ([App.UseDecompress]) has
	// not unwrapped. The encoding is in the details, under "encoding".
	CodeUnsupportedContentEncoding = "unsupported_content_encoding"

	// CodeInvalidBindTarget is the 500 code for a bind target the binder
	// cannot fill — nil, not a pointer, or with a field of a type it does not
	// bind — a developer error, reported without client-facing detail and
	// logged with the reason.
	CodeInvalidBindTarget = "invalid_bind_target"
)

// codeContentTypeRequired is the code for a request that must carry a
// Content-Type and does not: a QUERY request (400), or a body under
// ContractGuard's MetaAccept contract (415).
const codeContentTypeRequired = "content_type_required"

// codeInvalidPathEncoding is the 400 code for a request path whose parameter
// candidate is not valid percent-encoded UTF-8 (for example "%FF"). Malformed
// escapes such as "%zz" never reach the router: net/http rejects them first.
const codeInvalidPathEncoding = "invalid_path_encoding"

// builtInMessages maps error codes to safe default English messages. A code
// that is not here renders its status text.
var builtInMessages = map[string]string{
	"bad_request":            "Bad Request",
	"unauthorized":           "Unauthorized",
	"forbidden":              "Forbidden",
	"not_found":              "Not Found",
	"method_not_allowed":     "Method Not Allowed",
	"conflict":               "Conflict",
	"unprocessable_entity":   "Unprocessable Entity",
	"unsupported_media_type": "Unsupported Media Type",
	"internal_server_error":  "Internal Server Error",
	"too_many_requests":      "Too Many Requests",
	"service_unavailable":    "Service Unavailable",
	"gateway_timeout":        "Gateway Timeout",
	"request_timeout":        "Request Timeout",
	CodeValidationFailed:     "Validation Failed",
	CodeBindFailed:           "Malformed Request",
	codeContentTypeRequired:  "Content-Type is required.",
	codeInvalidPathEncoding:  "Request path contains an invalid percent-encoded value.",

	CodeUnsupportedContentEncoding: "Unsupported Content-Encoding.",

	// middleware.ContractGuard's rejections; their values are in the details.
	"content_type_not_accepted":        "Content-Type is not accepted.",
	"missing_required_header":          "A required header is missing.",
	"missing_required_query_parameter": "A required query parameter is missing.",
	"unsupported_api_version":          "Unsupported or missing API version.",
	"missing_required_scope":           "A required scope is missing.",
}

// HTTPError represents an HTTP error carrying a status and a stable
// machine-readable code, with an optional message: a catalog key or literal
// text.
//
// Code is the primary wire identity; the message only affects the
// human-readable text. The message resolves in this order:
//  1. Message, set with [HTTPError.WithMessage] — literal text, rendered as is
//  2. MessageKey, set with [HTTPError.WithMessageKey] — looked up in the
//     catalog; without one, the configured scoped resolver or the bare Code
//     is the key
//  3. the built-in message for the Code, then the status text
//
// A key the catalog does not define is never rendered as text: it falls back
// to step 3, and in debug mode the pipeline warns with the key, the code and
// the route.
type HTTPError struct {
	// Status is the HTTP status code.
	Status int `json:"status"`

	// Code is the stable machine-readable error code, rendered as the RFC
	// 7807 "code" extension member. [NewHTTPError] always materializes it:
	// either the explicit code argument or the frozen default for the status
	// (for example 404 → "not_found", unknown 499 → "http_499"). It must
	// satisfy the machine-code grammar ^[a-z0-9]+(_[a-z0-9]+)*$.
	Code string `json:"code,omitempty"`

	// MessageKey is an optional catalog key for the message; it never
	// contributes to Code. Attach one with [HTTPError.WithMessageKey].
	MessageKey MessageKey `json:"message_key,omitempty"`

	// Message is optional literal client text, rendered as is and never
	// looked up; it wins over MessageKey. Attach it with
	// [HTTPError.WithMessage]. Client input belongs in Details, not here.
	Message string `json:"message,omitempty"`

	// Details carries optional structured, client-safe detail rendered as the
	// default error response's "details" member. It is encoded with the
	// application's JSON profile; never place secrets or internal state here.
	Details any `json:"-"`

	// Internal is the underlying error. It is never exposed to the client;
	// the error pipeline logs it, at Error for a 5xx status and at Debug below.
	Internal error `json:"-"`
}

// NewHTTPError creates a new HTTPError with the given status and optional
// stable machine-readable code. With no code argument, the frozen default
// code for the status is used (404 → "not_found"; a status outside the
// frozen table yields "http_<status>"). The message starts empty; attach a
// catalog key with [HTTPError.WithMessageKey] or literal text with
// [HTTPError.WithMessage].
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
			panic(fmt.Sprintf("credo: NewHTTPError: %q is not a valid machine code (want ^[a-z0-9]+(_[a-z0-9]+)*$); since v0.11.0 the second argument is the machine code — attach a catalog key with WithMessageKey or literal text with WithMessage", code[0]))
		}
		return &HTTPError{Status: status, Code: code[0]}
	default:
		panic("credo: NewHTTPError: at most one machine code argument is allowed")
	}
}

// Error implements the error interface. The diagnostic order is status,
// code, key, message, internal; empty segments are omitted.
func (e *HTTPError) Error() string {
	s := fmt.Sprintf("status=%d", e.Status)
	if e.Code != "" {
		s += ", code=" + e.Code
	}
	if e.MessageKey != "" {
		s += ", key=" + string(e.MessageKey)
	}
	if e.Message != "" {
		s += fmt.Sprintf(", message=%q", e.Message)
	}
	if e.Internal != nil {
		s += fmt.Sprintf(", internal=%v", e.Internal)
	}
	return s
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

// WithMessageKey returns a copy of the error with the catalog key of its
// message set. The key affects only the human-readable message, looked up in
// the catalog; a key the catalog does not define renders the built-in message
// for the Code, never the key. The machine-readable Code is untouched.
func (e *HTTPError) WithMessageKey(key MessageKey) *HTTPError {
	c := e.clone()
	c.MessageKey = key
	return c
}

// WithMessage returns a copy of the error with literal client text as its
// message, rendered as is and never looked up. It wins over a message key.
// The text is the application's own; client input belongs in
// [HTTPError.WithDetails].
func (e *HTTPError) WithMessage(text string) *HTTPError {
	c := e.clone()
	c.Message = text
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

// ErrorResponse is Credo's default JSON error envelope. The top level carries
// only the success discriminator; everything about the error itself lives in
// the nested [ErrorBody] object, mirroring how a success envelope pairs
// "success" with a data payload.
type ErrorResponse struct {
	Success bool      `json:"success"`
	Error   ErrorBody `json:"error"`
}

// ErrorBody is the error object nested inside [ErrorResponse].
type ErrorBody struct {
	// Code is the stable machine-readable error identity.
	Code string `json:"code"`

	// Message is the resolved human-readable message.
	Message string `json:"message"`

	// Details carries optional structured, client-safe detail.
	Details any `json:"details,omitempty"`

	// Violations lists rule violations behind the error: field-scoped
	// validation failures and document-scoped bind (body-contract) failures.
	// A bind entry's Field may be empty when the violation concerns the whole
	// body (for example a JSON syntax error).
	Violations []validation.ValidationError `json:"violations,omitempty"`
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

	// Violations holds validation or binding rule violations (if any) as an
	// extension member, matching the default envelope's vocabulary.
	Violations []validation.ValidationError `json:"violations,omitempty"`
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
// application/problem+json; Code, Details, and Violations are extension
// members.
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
			Type:       problemType,
			Title:      title,
			Status:     info.Status,
			Detail:     detail,
			Instance:   ctx.Request().URL.Path,
			Code:       info.Code,
			Details:    info.Details,
			Violations: info.Violations,
		}
	}
}

// handleError is the internal error handling pipeline. It performs:
//  1. Panic recovery (if ErrorRenderer panics, logs and sends 500)
//  2. Hijacked/committed guard (logs warning if the HTTP response is no longer writable)
//  3. Error classification via classifyError
//  4. Logging (5xx at Error; the internal cause of a client error at Debug)
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
	ctx.exec.code = info.Code
	app.logError(err, info.Status, ctx)
	app.renderError(ctx, info)
}

func (app *App) recoverErrorPipelinePanic(err error, ctx *Context) {
	if r := recover(); r != nil {
		ctx.Logger().LogAttrs(ctx.Request().Context(), slog.LevelError,
			"credo: error pipeline panic", slog.Any("panic", r), slog.Any("error", err))
		if !ctx.Response().Hijacked() && !ctx.Response().Committed() {
			// A marshal failure inside panic recovery is deliberately
			// swallowed; there is no safer response left to attempt.
			info := safeInternalErrorInfo()
			ctx.exec.code = info.Code
			writeDefaultError(ctx, info) //nolint:errcheck
		}
	}
}

// logError writes what the response keeps from the client: a server error at
// Error, the internal cause of a client error at Debug.
func (app *App) logError(err error, status int, ctx *Context) {
	if status < 500 {
		logClientErrorCause(err, status, ctx)
		return
	}

	message := "credo: server error"
	if _, isHTTPError := errors.AsType[*HTTPError](err); !isHTTPError {
		if _, isFault := fault.ProviderOf(err); !isFault {
			message = "credo: unhandled error"
		}
	}

	logErr := err
	if cause := internalCause(err); cause != nil {
		logErr = cause
	}
	ctx.Logger().LogAttrs(ctx.Request().Context(), slog.LevelError,
		message, slog.Int("status", status), slog.Any("error", logErr))
}

// logClientErrorCause writes the internal cause of an error below 500 at
// Debug. A client error earns no line at the default level, but the cause
// attached with [HTTPError.WithInternal] or [fault.Error.WithInternal], or
// the decoder error behind a [BindError], is what explains the response to
// whoever goes looking. An error that carries no such cause — a fault
// provider other than a fault.Error — writes nothing.
func logClientErrorCause(err error, status int, ctx *Context) {
	cause := internalCause(err)
	if cause == nil {
		if be, isBindError := errors.AsType[*BindError](err); isBindError {
			cause = be.Internal
		}
	}
	if cause == nil {
		return
	}
	ctx.Logger().LogAttrs(ctx.Request().Context(), slog.LevelDebug,
		"credo: client error", slog.Int("status", status), slog.Any("error", cause))
}

// internalCause is the internal cause of the error that classifies err: an
// HTTPError's, or else a fault.Error's. Nil when that error carries none.
func internalCause(err error) error {
	if he, ok := errors.AsType[*HTTPError](err); ok {
		return he.Internal
	}
	if fe, ok := errors.AsType[*fault.Error](err); ok {
		return fe.Internal
	}
	return nil
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
		ctx.exec.code = info.Code
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
//  1. *HTTPError → status/code from the error and message from exact-key
//     resolution; invalid stored fields fail closed to a generic 500. An
//     explicit status wins over a validation or bind error wrapped inside it.
//  2. *fault.Error → status from its kind, its code and details; an
//     undeclared kind or an invalid code fails closed to a generic 500. Like
//     an HTTPError it wins over a validation or bind error it wraps.
//  3. validation.Errors or a single *validation.ValidationError → 422
//     Unprocessable Entity with field violations
//  4. *BindError → 400 Bad Request with a typed decode-reason violations entry
//  5. any other fault.Provider → the status for its kind, with the kind as
//     the code
//  6. Any other error → 500 Internal Server Error (message not leaked)
//
// These are the only status channels: an error that merely has an
// HTTPStatus() int method is "any other error".
//
// Every branch resolves its effective code and message through [codedErrorInfo],
// so the default pipeline always emits a non-empty machine code.
func (app *App) classifyError(err error, ctx *Context) *ErrorInfo {
	if he, ok := errors.AsType[*HTTPError](err); ok {
		// Fail closed on invalid directly constructed values: rebuild a
		// generic internal-server problem and publish none of the invalid
		// value's client-facing fields.
		if !isValidHTTPStatus(he.Status) || (he.Code != "" && !isValidErrorCode(he.Code)) {
			return app.codedErrorInfo(ctx, http.StatusInternalServerError, "", "")
		}
		var info *ErrorInfo
		if he.Message != "" {
			info = &ErrorInfo{Status: he.Status, Code: effectiveCode(he.Status, he.Code), Message: he.Message}
		} else {
			info = app.codedErrorInfo(ctx, he.Status, he.Code, he.MessageKey)
		}
		info.Details = he.Details
		return info
	}

	if fe, ok := errors.AsType[*fault.Error](err); ok {
		status, known := internalfaultstatus.HTTP(fe.Kind)
		if !known || !isValidErrorCode(fe.Code) {
			return app.codedErrorInfo(ctx, http.StatusInternalServerError, "", "")
		}
		info := app.codedErrorInfo(ctx, status, fe.Code, "")
		info.Details = fe.Details
		return info
	}

	if ve, ok := errors.AsType[validation.Errors](err); ok {
		return app.validationErrorInfo(ctx, ve)
	}
	if ve, ok := errors.AsType[*validation.ValidationError](err); ok {
		return app.validationErrorInfo(ctx, validation.Errors{*ve})
	}

	if be, ok := errors.AsType[*BindError](err); ok {
		info := app.codedErrorInfo(ctx, http.StatusBadRequest, CodeBindFailed, "")
		info.Violations = []validation.ValidationError{app.bindProblemError(ctx, be)}
		return info
	}

	if provider, ok := fault.ProviderOf(err); ok {
		kind := provider.FaultKind()
		status, known := internalfaultstatus.HTTP(kind)
		if !known {
			return app.codedErrorInfo(ctx, http.StatusInternalServerError, "", "")
		}
		// The kind is the code, so a duplicate stays distinguishable from a
		// retryable serialization or deadlock failure under the same 409.
		return app.codedErrorInfo(ctx, status, string(kind), "")
	}

	return app.codedErrorInfo(ctx, http.StatusInternalServerError, "", "")
}

// validationErrorInfo is the 422 validation_failed classification.
func (app *App) validationErrorInfo(ctx *Context, ve validation.Errors) *ErrorInfo {
	info := app.codedErrorInfo(ctx, http.StatusUnprocessableEntity, CodeValidationFailed, "")
	info.Violations = []validation.ValidationError(app.translateValidationErrors(ctx, ve))
	return info
}

// codedErrorInfo is the single source of the effective-code and message
// calculation for the HTTPError, fault and generic-500 classification
// branches.
func (app *App) codedErrorInfo(ctx *Context, status int, explicitCode string, explicitKey MessageKey) *ErrorInfo {
	code := effectiveCode(status, explicitCode)
	key, message := app.resolveErrorMessage(ctx, status, code, explicitKey)
	return &ErrorInfo{Status: status, Code: code, MessageKey: key, Message: message}
}

// effectiveCode is the explicit code, or the status's default without one.
func effectiveCode(status int, explicitCode string) string {
	if explicitCode != "" {
		return explicitCode
	}
	return defaultCodeForStatus(status)
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
		Error: ErrorBody{
			Code:       info.Code,
			Message:    info.Message,
			Details:    info.Details,
			Violations: info.Violations,
		},
	}, ctx.app.errorJSONOptions())
}

// resolveErrorMessage looks the effective key up in the catalog, then falls
// back to the built-in message for the code and the status text. A key is
// never rendered as text.
func (app *App) resolveErrorMessage(ctx *Context, status int, code string, explicitKey MessageKey) (MessageKey, string) {
	key, explicit := app.effectiveMessageKey(MessageScopeError, code, explicitKey)
	if ctx.translatable() {
		if message, ok := app.i18n.bundle.TranslateForLang(ctx.resolveLocale(), string(key), nil); ok {
			return key, message
		}
	}
	if explicit {
		app.noteCatalogMiss(ctx, key, code)
	}
	if message, ok := builtInMessages[code]; ok {
		return key, message
	}
	if message := http.StatusText(status); message != "" {
		return key, message
	}
	return key, fmt.Sprintf("HTTP %d", status)
}

// noteCatalogMiss warns in debug mode that an explicit key is not in the
// catalog, so the code's built-in message was rendered in its place.
func (app *App) noteCatalogMiss(ctx *Context, key MessageKey, code string) {
	if app == nil || !app.debug {
		return
	}
	ctx.Logger().LogAttrs(ctx.Request().Context(), slog.LevelWarn,
		"credo: message key is not in the catalog; the built-in message for the code is rendered",
		slog.String("key", string(key)), slog.String("code", code), slog.String("route", routeLabel(ctx.route)))
}

// routeLabel names a route in a diagnostic: its method and pattern, its
// pattern alone for a mount, empty without a route.
func routeLabel(r *Route) string {
	if r == nil {
		return ""
	}
	if r.method != "" {
		return r.method + " " + r.pattern
	}
	return r.pattern
}

func (app *App) effectiveMessageKey(scope MessageScope, code string, explicitKey MessageKey) (MessageKey, bool) {
	if explicitKey != "" {
		return explicitKey, true
	}
	if app != nil && app.i18n != nil && app.i18n.resolver != nil {
		key := app.i18n.resolver(MessageRef{Scope: scope, Code: code})
		if key == "" {
			panic(fmt.Sprintf("credo: MessageKeyResolver returned an empty key for scope %d code %q", scope, code))
		}
		return MessageKey(key), false
	}
	return MessageKey(code), false
}

// translateValidationErrors resolves each validation error's exact key and
// translates it when a bundle is active.
func (app *App) translateValidationErrors(ctx *Context, ve validation.Errors) validation.Errors {
	result := make(validation.Errors, len(ve))
	for i, e := range ve {
		result[i] = e // copy
		key, _ := app.effectiveMessageKey(MessageScopeValidation, e.Code, e.MessageKey)
		result[i].MessageKey = key
		if ctx.translatable() {
			if s, ok := translateFieldMessage(app.i18n.bundle, ctx.resolveLocale(), string(key), e.Params, e.Field); ok {
				result[i].Message = s
			}
		}
	}
	return result
}

// translateFieldMessage resolves a field-scoped message through the bundle:
// it copies params, injects the translated field name when field is non-empty,
// and looks up key for lang. The boolean reports whether a translation exists.
// Shared by the validation and bind violations[] flows so their field-name
// and params handling cannot drift.
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

// bindProblemError converts a [BindError] into the single violations[] entry
// of the default error response. The entry mirrors the validation error shape:
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

	if ctx.translatable() {
		if s, ok := translateFieldMessage(app.i18n.bundle, ctx.resolveLocale(), string(key), ve.Params, be.Field); ok {
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
