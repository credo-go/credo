// Originally derived from Chi (https://github.com/go-chi/chi), Copyright (c)
// 2015-present Peter Kieltyka, Google Inc., MIT licensed. Substantially
// modified for Credo; see the NOTICES file for full attribution.

package credo

import (
	"fmt"
	"log/slog"
	"net/netip"
	"reflect"
	"sync"
	"sync/atomic"

	"github.com/credo-go/credo/config"
	"github.com/credo-go/credo/internal/di"
	internali18n "github.com/credo-go/credo/internal/i18n"
	internalproxy "github.com/credo-go/credo/internal/proxy"
)

// App is the main Credo application. It holds the router, middleware stack,
// context pool, error handler, and route registry.
type App struct {
	// container is the DI container for service registration and resolution.
	container *di.Container

	// logger is the application-level logger, set via WithLogger.
	// Used by framework internals (error handler, server lifecycle, i18n setup).
	// Service-scoped loggers (Infra.Logger) are derived from this.
	logger *slog.Logger

	// mux is the underlying radix-tree router.
	mux *mux

	// errorRenderer formats error responses, set via SetErrorRenderer.
	// nil = default RFC 7807 JSON renderer.
	errorRenderer ErrorRenderer

	// pool reuses Context instances.
	ctxPool *pool[*Context]

	// root is the root route group: empty prefix, default mux, no host
	// scope. App's route-registration surface (HTTP verbs, Group, Static,
	// File, SetMeta) delegates to it so registration logic — path joining,
	// HEAD-twin wiring — lives in Group alone.
	root *Group

	// globalMW holds global middleware (applied to all requests, including 404/405).
	globalMW []Middleware

	// compiledHandler is the global MW chain ending in dispatch.
	compiledHandler Handler

	// handlerOnce ensures compile is called exactly once.
	handlerOnce sync.Once

	// frozen is set after compile(); prevents late route/middleware additions.
	// Separate from state because ServeHTTP triggers compile (frozen) without
	// entering the Running state — the user may manage their own *http.Server.
	frozen atomic.Bool

	// lifecycle owns the server-session state machine, the bound server and app
	// context, the start/shutdown hooks, and the graceful-drain sequence. The
	// public Run/Shutdown/State/Addr/OnStart/OnShutdown methods delegate to it.
	lifecycle *lifecycleManager

	// rawConfig holds the RawConfig passed via WithRawConfig option.
	rawConfig RawConfig

	// serverCfg holds the server configuration (host, port, timeouts).
	serverCfg serverConfig

	// i18nBundle holds the loaded i18n message bundle (nil if i18n inactive).
	i18nBundle *internali18n.Bundle

	// healthEngine holds the health check engine (nil if UseHealth not called).
	healthEngine *healthEngine

	// healthExposeErrors includes check error strings in readiness responses.
	// Set from HealthConfig.ExposeErrors; default false (errors are logged,
	// not exposed to probe callers).
	healthExposeErrors bool

	// hosts holds registered host patterns for domain-based routing.
	// Empty when no host-specific routing is configured.
	hosts []*hostEntry

	// staticHosts maps exact host names to host entries for O(1) dispatch.
	// Param and regex host patterns remain in hosts and use specificity order.
	staticHosts map[string]*hostEntry

	// namedRoutes maps route names to Route pointers.
	namedRoutes map[string]*Route

	// statusHandlers holds app-level custom handlers for 404/405/5xx
	// responses, set via StatusHandler.
	statusHandlers map[int]Handler

	// redirectTrailingSlash controls automatic trailing-slash redirects.
	// Resolved once from config/option in New(); default true.
	redirectTrailingSlash bool

	// disableRecover disables the built-in panic recovery wrapper.
	// Set via WithoutRecover option.
	disableRecover bool

	// disableRequestID disables the built-in request ID middleware.
	// Set via WithoutRequestID option.
	disableRequestID bool

	// disableAccessLog disables the built-in access logger middleware.
	// Set via WithoutAccessLog option.
	disableAccessLog bool

	// debug enables development-mode warnings.
	// Set via WithDebug option or server.debug config key.
	debug bool

	// trustedProxies contains parsed CIDR ranges allowed to influence forwarded headers.
	trustedProxies []netip.Prefix
}

// New creates a new App with the given options. When no RawConfig is provided,
// New auto-loads configuration with config.Load and registers the resulting
// RawConfig in DI. Passing WithRawConfig bypasses auto-load and registers the
// given RawConfig instead.
//
// New returns an error if configuration loading fails or if server settings
// contain invalid values (negative timeouts, invalid port).
//
// Usage:
//
//	// Zero-config (all defaults):
//	app, err := credo.New()
//
//	// With listen address:
//	app, err := credo.New(credo.WithAddr("127.0.0.1", 8080))
//
//	// With explicit RawConfig (server settings read from "server" key):
//	app, err := credo.New(credo.WithRawConfig(rawCfg))
func New(opts ...Option) (*App, error) {
	o := appOptions{}
	for _, opt := range opts {
		opt(&o)
	}

	// Auto-load: if no RawConfig provided, load with defaults.
	if o.rawConfig == nil {
		rc, err := config.Load()
		if err != nil {
			return nil, fmt.Errorf("credo: auto-load config: %w", err)
		}
		o.rawConfig = rc
	}

	// If a "server" section exists, decode it.
	// Missing key is fine (use defaults), but a decode error is surfaced.
	if o.rawConfig.Exists("server") {
		if err := o.rawConfig.Unmarshal("server", &o.serverCfg); err != nil {
			return nil, fmt.Errorf("credo: invalid server config: %w", err)
		}
	}
	if o.trustedProxiesSet {
		o.serverCfg.TrustedProxies = append([]string(nil), o.trustedProxies...)
	}
	applyServerDefaults(&o.serverCfg)

	if err := validateServerConfig(&o.serverCfg); err != nil {
		return nil, err
	}
	trustedProxies, err := internalproxy.ParsePrefixes(o.serverCfg.TrustedProxies)
	if err != nil {
		return nil, fmt.Errorf("credo: invalid trusted proxies: %w", err)
	}

	c := di.New()

	// Configure Infra auto-injection (Model 1).
	baseInfra := newInfra(o.logger)
	c.SetInfraProvider(&di.InfraProvider{
		InfraType: reflect.TypeFor[Infra](),
		Factory: func(serviceName string) any {
			return Infra{Logger: baseInfra.Logger.With("service", serviceName)}
		},
	})

	// RawConfig is always available (auto-loaded or explicit).
	di.MustProvideValue[RawConfig](c, o.rawConfig)

	app := &App{
		container:             c,
		logger:                baseInfra.Logger,
		mux:                   newMux(),
		staticHosts:           make(map[string]*hostEntry),
		namedRoutes:           make(map[string]*Route),
		rawConfig:             o.rawConfig,
		serverCfg:             o.serverCfg,
		redirectTrailingSlash: o.serverCfg.RedirectTrailingSlash == nil || *o.serverCfg.RedirectTrailingSlash,
		disableRecover:        o.disableRecover,
		disableRequestID:      o.disableRequestID,
		disableAccessLog:      o.disableAccessLog,
		debug:                 o.debug || o.serverCfg.Debug,
		trustedProxies:        trustedProxies,
	}
	app.root = &Group{app: app}
	app.lifecycle = &lifecycleManager{app: app}
	app.ctxPool = newPool(func() *Context {
		return &Context{
			app: app,
			request: &Request{
				app: app,
				// Most routes carry 1-4 parameters; pre-size so steady-state
				// dispatch never grows the backing arrays.
				paramKeys:   make([]string, 0, 4),
				paramValues: make([]string, 0, 4),
			},
			response: &Response{},
		}
	})
	return app, nil
}

// --- HTTP Method Shortcuts ---

// GET registers a GET route. A matching HEAD handler is registered
// automatically; subsequent calls to [Route.Middleware] and [Route.SetMeta]
// on the returned route apply to HEAD as well, so HEAD requests can never
// silently bypass auth, rate limiting, or meta-driven middleware.
//
// Panics if h is nil, the pattern is invalid or already registered, or if
// called after compile (route registration is developer configuration — see
// the package-level "Panics and Errors" section).
func (app *App) GET(pattern string, h Handler) *Route {
	return app.root.GET(pattern, h)
}

// POST registers a POST route.
// Panics under the same conditions as [App.GET].
func (app *App) POST(pattern string, h Handler) *Route {
	return app.root.POST(pattern, h)
}

// PUT registers a PUT route.
// Panics under the same conditions as [App.GET].
func (app *App) PUT(pattern string, h Handler) *Route {
	return app.root.PUT(pattern, h)
}

// DELETE registers a DELETE route.
// Panics under the same conditions as [App.GET].
func (app *App) DELETE(pattern string, h Handler) *Route {
	return app.root.DELETE(pattern, h)
}

// PATCH registers a PATCH route.
// Panics under the same conditions as [App.GET].
func (app *App) PATCH(pattern string, h Handler) *Route {
	return app.root.PATCH(pattern, h)
}

// HEAD registers an explicit HEAD route (overrides auto-generated one).
// Panics under the same conditions as [App.GET].
func (app *App) HEAD(pattern string, h Handler) *Route {
	return app.root.HEAD(pattern, h)
}

// OPTIONS registers an OPTIONS route.
// Panics under the same conditions as [App.GET].
func (app *App) OPTIONS(pattern string, h Handler) *Route {
	return app.root.OPTIONS(pattern, h)
}

// --- Middleware ---

// GlobalMiddleware appends middleware that runs on every request,
// including 404 and 405 responses. Must be called before the server starts;
// panics if called after compile.
func (app *App) GlobalMiddleware(middlewares ...Middleware) {
	app.checkFrozen("GlobalMiddleware")
	app.globalMW = append(app.globalMW, middlewares...)
}

// --- Groups ---

// Group creates a new route group with the given prefix.
func (app *App) Group(prefix string) *Group {
	return app.root.Group(prefix)
}

// --- Host Routing ---

// Host creates a route group scoped to the given host pattern.
// Pattern supports exact labels ("api.example.com"), named parameters
// ("{tenant}.example.com", "{org:[a-z]+}.platform.io"), and a leftmost
// anonymous wildcard label ("*.example.com").
// Routes registered on the returned Group only match requests whose Host
// header matches the pattern. Unmatched hosts fall back to the default mux.
// Returns *Group for API consistency with [App.Group].
//
// Host panics if the pattern is a duplicate, overlaps an existing pattern with
// identical match semantics, contains an invalid wildcard, an invalid regex
// constraint, or a port. Registering a route on the returned Group panics if
// a route parameter name collides with a host parameter name.
// Must be called before the server starts; panics if called after compile.
func (app *App) Host(pattern string) *Group {
	app.checkFrozen("host registration")

	normalized := normalizeHostPattern(pattern)
	if app.hasHostPattern(normalized) {
		panic(fmt.Sprintf("credo: duplicate host pattern %q", normalized))
	}

	segments, paramKeys := parseHostPattern(normalized)
	semantic := hostPatternSemanticKey(segments)
	if existing := app.hostSemanticConflict(semantic); existing != "" {
		panic(fmt.Sprintf("credo: host patterns %q and %q have identical match semantics; choose one", existing, normalized))
	}
	m := newMux()
	entry := &hostEntry{
		pattern:   normalized,
		segments:  segments,
		paramKeys: paramKeys,
		semantic:  semantic,
		mux:       m,
	}
	app.hosts = append(app.hosts, entry)
	if isStaticHostEntry(entry) {
		app.staticHosts[normalized] = entry
	}

	return &Group{
		app:         app,
		parent:      app.root, // inherit app-level meta
		mux:         m,
		hostPattern: normalized,
	}
}

// --- Status Handlers ---

// StatusHandler sets a custom handler for the given HTTP status code
// at the root level.
// Must be called before the server starts; panics if called after compile.
func (app *App) StatusHandler(code int, h Handler) {
	app.checkFrozen("StatusHandler")
	if app.statusHandlers == nil {
		app.statusHandlers = make(map[int]Handler)
	}
	app.statusHandlers[code] = h
}

// SetErrorRenderer sets the renderer that formats error responses. The
// framework handles error classification, logging, HEAD handling, and
// committed-response guards internally; the renderer receives an [ErrorInfo]
// containing the original error, the i18n message key, and the classified
// [ProblemDetails]. Passing nil restores the default RFC 7807 JSON renderer.
//
// Must be called before the server starts; panics if called after compile.
func (app *App) SetErrorRenderer(r ErrorRenderer) {
	app.checkFrozen("SetErrorRenderer")
	app.errorRenderer = r
}

// --- Meta ---

// SetMeta sets a root-level metadata key-value pair.
// Must be called before the server starts; panics if called after compile.
func (app *App) SetMeta(key string, val any) {
	app.root.SetMeta(key, val)
}

// RemoveMeta removes a root-level metadata key.
// Must be called before the server starts; panics if called after compile.
func (app *App) RemoveMeta(key string) {
	app.root.RemoveMeta(key)
}

// Mux returns a route registry view for route introspection (Walk, Routes).
// The returned view includes routes from the default mux and all host-scoped muxes.
func (app *App) Mux() Routes {
	return app
}

// Routes returns introspection data for all registered routes across the default
// mux and all host-scoped muxes.
func (app *App) Routes() []RouteInfo {
	routes := app.mux.Routes()
	count := len(routes)
	hostRoutes := make([][]RouteInfo, 0, len(app.hosts))
	for _, h := range app.hosts {
		hostView := h.mux.Routes()
		hostRoutes = append(hostRoutes, hostView)
		count += len(hostView)
	}

	out := make([]RouteInfo, 0, count)
	out = append(out, routes...)
	for _, hostView := range hostRoutes {
		out = append(out, hostView...)
	}
	return out
}

// --- Named Routes ---

// GetRoute returns a named route by name.
func (app *App) GetRoute(name string) *Route {
	return app.namedRoutes[name]
}

// deregisterName removes a named route from the registry.
func (app *App) deregisterName(name string) {
	delete(app.namedRoutes, name)
}

// registerName registers a named route. Panics on duplicate names.
func (app *App) registerName(name string, route *Route) {
	if existing, exists := app.namedRoutes[name]; exists && existing != route {
		panic(fmt.Sprintf("credo: duplicate route name %q", name))
	}
	app.namedRoutes[name] = route
}
