// Originally derived from Echo (https://github.com/labstack/echo),
// Copyright (c) 2024 LabStack, MIT licensed. Substantially modified for Credo;
// see the NOTICES file for full attribution.

package credo

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
)

// Context is the request-scoped struct that holds the Request, Response,
// matched Route, logger, and a key-value store.
//
// Context is pooled via sync.Pool for zero-allocation request handling.
// Access fields only through methods (not direct field access) to ensure
// pool safety. Because it is pooled, Context deliberately does not implement
// context.Context; call [Context.Context] to obtain the request's
// context.Context for context-taking APIs.
type Context struct {
	app              *App
	request          *Request
	response         *Response
	route            *Route
	logger           *slog.Logger
	locale           string
	extra            map[string]any
	originalPath     string // set in reset(), never modified after
	rewriteTarget    string // set by Rewrite(), consumed by dispatch loop
	rewriteRequested bool   // set by Rewrite(), read by dispatch loop
	rewriteCount     int    // loop detection counter
}

const maxRewrites = 10

// errRewrite is an internal sentinel used by the leaf handler wrapper.
// It is caught by the wrapper and never escapes to user middleware.
var errRewrite = errors.New("credo: internal rewrite")

// NewContext creates a new Context wrapping the given ResponseWriter and Request.
// This is primarily useful for testing error handlers and middleware.
func NewContext(w http.ResponseWriter, r *http.Request) *Context {
	return &Context{
		request:  NewRequest(r),
		response: NewResponse(w),
	}
}

// Request returns the Request for this context.
func (c *Context) Request() *Request {
	return c.request
}

// Response returns the Response for this context.
func (c *Context) Response() *Response {
	return c.response
}

// Route returns the matched Route (for accessing Meta, Name, BuildURI).
// Returns nil when no route matched (e.g., inside custom 404/405 handlers).
// Use [Context.HasRoute] to guard against nil before calling Route methods.
func (c *Context) Route() *Route {
	return c.route
}

// HasRoute reports whether a route matched for this request.
// Returns false inside custom 404/405 status handlers where no route exists.
// Use this to guard [Context.Route] calls in middleware that may run on
// unmatched-request paths:
//
//	if ctx.HasRoute() {
//	    val, _ := ctx.Route().LookupMeta("permission")
//	}
func (c *Context) HasRoute() bool {
	return c.route != nil
}

// Logger returns the request-scoped logger. Falls back through the chain:
// request logger → app logger → nop logger.
func (c *Context) Logger() *slog.Logger {
	if c.logger != nil {
		return c.logger
	}
	if c.app != nil {
		return c.app.Logger()
	}
	return defaultLogger
}

// SetLogger replaces the request-scoped logger for the remainder of this
// request. It is the wholesale-replacement API: reach for it when the
// logger itself must change (different handler, level, or destination).
// To add request-bound attributes, prefer [Context.AddLogAttrs].
//
// When replacing, derive the new logger from [Context.Logger] so existing
// enrichment is preserved:
//
//	ctx.SetLogger(ctx.Logger().With("tenant_id", tenantID))
//
// A logger built from scratch silently drops attributes added earlier —
// including the request_id added by the request ID tier — from all
// subsequent log output (handler logs and the access log). The framework
// cannot detect this; if you must start from a fresh logger, re-attach the
// ID via [Context.RequestID].
//
// The logger is cleared automatically when the request completes.
func (c *Context) SetLogger(l *slog.Logger) {
	c.logger = l
}

// AddLogAttrs adds attributes to the request-scoped logger for the
// remainder of this request. The new logger is derived from
// [Context.Logger], so enrichment added earlier — such as the request_id
// attribute — is preserved by construction. args are key-value pairs as
// accepted by [slog.Logger.With]:
//
//	ctx.AddLogAttrs("tenant_id", tenantID)
//
// Prefer this over [Context.SetLogger] when the goal is to add attributes
// rather than replace the logger. Calling it with no arguments is a no-op.
func (c *Context) AddLogAttrs(args ...any) {
	if len(args) == 0 {
		return
	}
	c.logger = c.Logger().With(args...)
}

// HasRequestLogger reports whether a request-scoped logger has been set for
// this request — by the built-in request ID tier, middleware.RequestID,
// [Context.SetLogger], or [Context.AddLogAttrs]. It does not inspect the
// logger's attributes.
//
// The framework's log emitters (access log, panic recovery) use it as a
// convention-based signal: under the derivation contract documented on
// [Context.SetLogger], a request-scoped logger is assumed to already carry
// request_id, so the emitters skip adding the attribute explicitly.
func (c *Context) HasRequestLogger() bool {
	return c.logger != nil
}

// RequestID returns the current request ID.
// It returns an empty string when request ID middleware is not active.
func (c *Context) RequestID() string {
	if id, ok := c.Get(requestIDKey).(string); ok {
		return id
	}
	return ""
}

// Locale returns the detected locale string for this request (e.g., "en", "tr").
// Returns an empty string if i18n is not configured.
func (c *Context) Locale() string {
	return c.locale
}

// T translates a message key using the detected locale. If i18n is not
// configured or the key is not found, the key itself is returned.
// Optional data map provides template variables for the message.
//
// T always renders the message's Other plural form; for count-based plural
// selection use [Context.TPlural].
func (c *Context) T(key string, data ...map[string]any) string {
	if c.app == nil || c.app.i18nBundle == nil {
		return key
	}
	var d map[string]any
	if len(data) > 0 {
		d = data[0]
	}
	if s, ok := c.app.i18nBundle.TranslateForLang(c.locale, key, d); ok {
		return s
	}
	return key
}

// TPlural translates a message key using the detected locale, rendering the
// CLDR cardinal plural form selected for count. count may be any integer
// kind, an integral float, or a decimal string ("1.5") when visible fraction
// digits matter. The count value is exposed to the template as {{.count}}.
//
//	// messages.json: "items": {"one": "{{.count}} item", "other": "{{.count}} items"}
//	ctx.TPlural("items", 1) // "1 item"
//	ctx.TPlural("items", 5) // "5 items"
//
// If i18n is not configured or the key is not found, the key itself is
// returned. When count cannot be interpreted as a number, the Other form
// is rendered.
func (c *Context) TPlural(key string, count any, data ...map[string]any) string {
	if c.app == nil || c.app.i18nBundle == nil {
		return key
	}
	var d map[string]any
	if len(data) > 0 {
		d = data[0]
	}
	if s, ok := c.app.i18nBundle.TranslatePluralForLang(c.locale, key, count, d); ok {
		return s
	}
	return key
}

// --- Request-scoped store ---

// Set stores a key-value pair in the request-scoped store.
func (c *Context) Set(key string, val any) {
	if c.extra == nil {
		c.extra = make(map[string]any)
	}
	c.extra[key] = val
}

// Get retrieves a value from the request-scoped store.
func (c *Context) Get(key string) any {
	if c.extra == nil {
		return nil
	}
	return c.extra[key]
}

// --- Request context access ---

// Context returns the underlying request's [context.Context]. Use it
// for APIs that take a context.Context — database queries, downstream
// requests, or [github.com/credo-go/credo/auth.GetUser]:
//
//	user, ok := auth.GetUser[*User](ctx.Context())
//
// The returned context is canceled when the request completes. For background
// work that must outlive the request, detach it with [context.WithoutCancel]:
//
//	bg := context.WithoutCancel(ctx.Context())
//	go process(bg)
//
// Context deliberately does NOT implement context.Context itself: it is pooled
// (sync.Pool) and reused across requests, so retaining a *Context as a
// long-lived context.Context would observe a later request's state.
// Context hands back the real, non-pooled request context instead.
func (c *Context) Context() context.Context {
	return c.request.Context()
}

// --- Pool reuse ---

// reset prepares the Context for pool reuse.
func (c *Context) reset(w http.ResponseWriter, r *http.Request) {
	c.request.reset(r)
	c.response.Reset(w)
	c.route = nil
	c.logger = nil
	c.locale = ""
	clear(c.extra)
	if r.URL.RawPath != "" {
		c.originalPath = r.URL.RawPath
	} else {
		c.originalPath = r.URL.Path
	}
	c.rewriteTarget = ""
	c.rewriteRequested = false
	c.rewriteCount = 0
}

// OriginalPath returns the request path as received from the client,
// before any rewriting (middleware.Rewrite or ctx.Rewrite).
// Useful for access logging, analytics, and debugging.
func (c *Context) OriginalPath() string {
	return c.originalPath
}

// Rewrite triggers an internal re-dispatch to the given path.
// The client is unaware of the rewrite (no HTTP redirect is sent).
// The original request path is preserved in OriginalPath().
// The matched host scope does not change.
//
// Rewrite must be the last call in a handler — the return value
// must be returned directly:
//
//	return ctx.Rewrite("/new-path")
//
// A maximum of 10 rewrites per request is enforced to prevent loops.
// If exceeded, an error is returned and the request fails with 500.
func (c *Context) Rewrite(path string) error {
	if c.Response().Committed() {
		return errors.New("credo: cannot rewrite after response is committed")
	}
	if path == "" || path[0] != '/' {
		return fmt.Errorf("credo: rewrite target must start with '/': %q", path)
	}
	c.rewriteTarget = path
	c.rewriteRequested = true
	return errRewrite
}

// IsRewriting reports whether this context has a pending internal rewrite.
// This can be useful in middleware to avoid side-effects (e.g., response
// writing) when the handler signaled a re-dispatch.
func (c *Context) IsRewriting() bool {
	return c.rewriteRequested
}
