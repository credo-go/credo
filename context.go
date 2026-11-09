// Originally derived from Echo (https://github.com/labstack/echo),
// Copyright (c) 2024 LabStack, MIT licensed. Substantially modified for Credo;
// see the NOTICES file for full attribution.

package credo

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/credo-go/credo/internal/radix"
)

// Context is the request-scoped struct that holds the Request, Response,
// matched Route, logger, and the request's values.
//
// Each request gets its own Context, one allocation that holds its Request,
// its Response and the first route parameters. A *Context kept past its
// request reads that request's state and no other. It is not safe for
// concurrent use: its context head moves as scopes return. Its response
// cannot be written after the handler returns, as net/http forbids for every
// ResponseWriter.
//
// Context does not implement context.Context: its head moves as scopes open
// and return, so a *Context held as a context.Context would not be one fixed
// context. Call [Context.Context] to obtain the request's context.Context, a
// fixed node, for context-taking APIs.
//
// Context is Credo's HTTP request context. Lifecycle hooks such as
// [App.OnStart] and [App.OnStop] receive a standard context.Context,
// not *Context.
type Context struct {
	app      *App
	request  Request
	response Response
	route    *Route
	logger   *slog.Logger
	// mountRouteCtx is the pooled RouteContext a mount's handler is given,
	// set by dispatch for the mount's chain only (see dispatch).
	mountRouteCtx *radix.RouteContext
	// pendingLogAttrID defers the request ID tier's logger enrichment: the
	// enriched logger (base.With("request_id", …)) is only built when
	// Logger() is first called, so requests whose handlers never log skip
	// the With allocations entirely. Framework emitters read it directly
	// and attach the ID as an explicit attribute instead of materializing.
	pendingLogAttrID string
	// locale and localeState hold the lazily resolved request locale: the
	// detector runs on the first Locale/translation access and the result is
	// memoized for the request (see resolveLocale).
	locale      string
	localeState uint8
	// head is the request's current context.Context, values holds the record
	// of Set calls and scope the innermost open scope (see contexthead.go).
	// headGen changes whenever the head does; installed is the embedded
	// request the Context last installed and installedGen the headGen it
	// carries, so Std knows without comparing contexts whether the embedded
	// request is in step.
	head         context.Context
	scope        headScope
	values       []valueEntry
	valuesBuf    [2]valueEntry // the first entries of values
	seq          uint64
	pending      bool // values set after pendingStart await reapplication
	pendingStart uint64
	headGen      uint64
	installed    *http.Request
	installedGen uint64
	originalPath string // set when the Context is built, never modified after
	// exec is the request executor's per-request bookkeeping.
	exec execState
}

// Locale resolution states.
const (
	localeUnresolved uint8 = iota
	localeResolving
	localeResolved
)

// NewContext creates a Context for w and r without an App. It is for unit
// tests of the hooks that take a *Context — a locale detector, a Skipper, an
// ErrorRenderer, the middleware around an auth.Authenticator — and of a
// [Request] or a [Response], taken with [Context.Request] and
// [Context.Response].
//
// A Context built without an App has what the App supplies to a served
// request missing: no route ([Context.Route] is nil and there are no route
// parameters), no request ID unless one is set, no translation (the
// locale is empty and [Context.T] returns its key), no trusted proxies
// ([Request.RealIP] and [Request.Scheme] read the peer alone), the package
// default logger, the framework's default JSON profile and multipart memory
// limit, no strict body decoding, and no success renderer ([Context.Render]
// writes plain JSON). Nothing finalizes it: a returned error is not rendered,
// and no observer or access record sees the request.
func NewContext(w http.ResponseWriter, r *http.Request) *Context {
	return newContext(nil, w, r)
}

// newContext builds the Context of one request in a single allocation: the
// Request and the Response are embedded by value, and the first route
// parameters and values land in fixed arrays inside it. app is nil for a
// Context built with NewContext.
func newContext(app *App, w http.ResponseWriter, r *http.Request) *Context {
	c := &Context{app: app}
	c.request.Request = r
	c.request.owner = c
	c.request.app = app
	c.request.paramKeys = c.request.paramKeyBuf[:0]
	c.request.paramValues = c.request.paramValueBuf[:0]
	c.response.ResponseWriter = w
	c.response.app = app
	c.values = c.valuesBuf[:0]
	c.initHead(r)
	c.originalPath = r.URL.EscapedPath()
	return c
}

// Request returns the Request for this context.
func (c *Context) Request() *Request {
	return &c.request
}

// Response returns the Response for this context.
func (c *Context) Response() *Response {
	return &c.response
}

// Route returns the matched Route (for accessing Meta, Name, BuildURI).
// Returns nil when no route matched (e.g., inside custom 404/405 handlers).
// Use [Context.HasRoute] to guard against nil before calling Route methods.
func (c *Context) Route() *Route {
	return c.route
}

// HasRoute reports whether a route matched for this request; a request a
// [App.Mount] matches has the mount's route. It is false before the match —
// in [App.PreRouting] middleware — and inside custom 404/405 status
// handlers. A middleware that enforces route meta fails closed without a
// route, since a guard that passes such a request lets every request
// through:
//
//	if !c.HasRoute() {
//	    return errors.New("authz: no route; register the middleware with app.Middleware, a group or a route")
//	}
//	scopes, _ := c.Route().GetMeta(RequiredScopes)
func (c *Context) HasRoute() bool {
	return c.route != nil
}

// Logger returns the request-scoped logger. Falls back through the chain:
// request logger → app logger → nop logger.
func (c *Context) Logger() *slog.Logger {
	if c.logger == nil && c.pendingLogAttrID != "" {
		// Deferred request ID enrichment (see pendingLogAttrID): pay the
		// With cost only when the logger is actually used.
		c.logger = c.baseLogger().With("request_id", c.pendingLogAttrID)
	}
	if c.logger != nil {
		return c.logger
	}
	return c.baseLogger()
}

// baseLogger returns the app logger (or the package default) without any
// request-scoped enrichment.
func (c *Context) baseLogger() *slog.Logger {
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
//	c.SetLogger(c.Logger().With("tenant_id", tenantID))
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
//	c.AddLogAttrs("tenant_id", tenantID)
//
// Prefer this over [Context.SetLogger] when the goal is to add attributes
// rather than replace the logger. Calling it with no arguments is a no-op.
func (c *Context) AddLogAttrs(args ...any) {
	if len(args) == 0 {
		return
	}
	c.logger = c.Logger().With(args...)
}

// HasRequestLogger reports whether [Context.Logger] returns (or, for the
// request ID tier's deferred enrichment, will return on first use) a
// request-scoped logger — one set by the request ID feature
// ([App.UseRequestID]), [Context.SetLogger], or [Context.AddLogAttrs]. It
// does not inspect the logger's attributes.
//
// The framework's log emitters (access log, panic recovery) use it as a
// convention-based signal: under the derivation contract documented on
// [Context.SetLogger], a request-scoped logger is assumed to already carry
// request_id, so the emitters skip adding the attribute explicitly.
func (c *Context) HasRequestLogger() bool {
	return c.logger != nil || c.pendingLogAttrID != ""
}

// RequestID returns the current request ID.
// It returns an empty string when request ID middleware is not active.
func (c *Context) RequestID() string {
	id, _ := c.Get(requestIDKey)
	return id
}

// Locale returns the request's locale (e.g., "en", "tr"). It is resolved on
// first use — by Locale itself, [Context.T], [Context.TPlural] or the error
// pipeline's message translation — through [I18nConfig.Detect] and memoized
// for the request: later header changes, authentication or rewrites do not
// change it. An empty or unresolvable detection selects the configured
// default language. Returns an empty string, without invoking the detector,
// when i18n is not configured.
func (c *Context) Locale() string {
	return c.resolveLocale()
}

// translatable reports whether the error pipeline may translate for this
// request: an i18n bundle is installed. Resolution itself is lazy, so the
// first error translation may be what triggers detection.
func (c *Context) translatable() bool {
	return c.app != nil && c.app.i18n != nil
}

// resolveLocale returns the memoized request locale, running the detector on
// first use. Detection is marked in progress before the callback runs:
// calling Locale or a translation from inside the detector is programming
// misuse and panics. When the detector panics (or re-enters), the default
// language is cached before the panic propagates, so error rendering never
// triggers a second detection for the request.
func (c *Context) resolveLocale() string {
	if c.localeState == localeResolved {
		return c.locale
	}
	if c.app == nil || c.app.i18n == nil {
		return ""
	}
	if c.localeState == localeResolving {
		panic("credo: recursive locale detection: Locale or a translation was called from inside I18nConfig.Detect")
	}
	f := c.app.i18n
	c.localeState = localeResolving
	c.locale = f.defaultLang
	defer func() { c.localeState = localeResolved }()
	if lang := f.detect(c); lang != "" {
		c.locale = f.bundle.MatchLangString(lang)
	}
	return c.locale
}

// T translates a message key using the detected locale. If i18n is not
// configured or the key is not found, the key itself is returned.
// Optional data map provides template variables for the message.
//
// T always renders the message's Other plural form; for count-based plural
// selection use [Context.TPlural].
func (c *Context) T(key string, data ...map[string]any) string {
	if c.app == nil || c.app.i18n == nil {
		return key
	}
	var d map[string]any
	if len(data) > 0 {
		d = data[0]
	}
	if s, ok := c.app.i18n.bundle.TranslateForLang(c.resolveLocale(), key, d); ok {
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
//	c.TPlural("items", 1) // "1 item"
//	c.TPlural("items", 5) // "5 items"
//
// If i18n is not configured or the key is not found, the key itself is
// returned. When count cannot be interpreted as a number, the Other form
// is rendered.
func (c *Context) TPlural(key string, count any, data ...map[string]any) string {
	if c.app == nil || c.app.i18n == nil {
		return key
	}
	var d map[string]any
	if len(data) > 0 {
		d = data[0]
	}
	if s, ok := c.app.i18n.bundle.TranslatePluralForLang(c.resolveLocale(), key, count, d); ok {
		return s
	}
	return key
}

// --- Request context access ---

// Context returns the request's current [context.Context], its head: the
// request's own context, the values set with [Context.Set] as nodes over it,
// and the deadline or span of the scope it is read in ([Context.Within], a
// stdlib middleware wrapped with [WrapStdMiddleware]). [Request.Context]
// returns the same context. Use it for APIs that take a context.Context —
// database queries, downstream requests, or transaction propagation through a
// data-store API:
//
//	err := db.InTx(c.Context(), func(txCtx context.Context) error {
//		return service.Update(txCtx)
//	})
//
// The returned context is made of immutable nodes, so it may be handed to a
// goroutine: later Set calls do not change it. It is canceled when the request
// completes. For background work that must outlive the request, detach it
// with [context.WithoutCancel]:
//
//	bg := context.WithoutCancel(c.Context())
//	go process(bg)
//
// Context deliberately does NOT implement context.Context itself: its head
// moves as scopes open and return, so a *Context held as a context.Context
// would not be one fixed context, and a *Context is not safe for concurrent
// use. Context hands back the head as it stands instead.
func (c *Context) Context() context.Context {
	if c.head == nil {
		return c.request.Request.Context()
	}
	c.noteForeignRequest()
	c.materialize()
	return c.head
}

// OriginalPath returns the request path as received from the client in its
// wire form (percent-encoding preserved), before any rewriting by
// middleware.Rewrite.
// Useful for access logging, analytics, and debugging.
func (c *Context) OriginalPath() string {
	return c.originalPath
}
