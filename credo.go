// Originally derived from Chi (https://github.com/go-chi/chi), Copyright (c)
// 2015-present Peter Kieltyka, Google Inc., MIT licensed. Substantially
// modified for Credo; see the NOTICES file for full attribution.

package credo

import (
	"cmp"
	"context"
	jsonv2 "encoding/json/v2"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"reflect"
	"slices"

	"github.com/credo-go/credo/config"
	"github.com/credo-go/credo/internal/di"
	internalproxy "github.com/credo-go/credo/internal/proxy"
)

// App is a Credo application: the value [New] returns, which the application
// registers into during bootstrap and then runs. It holds the routes and
// their groups, the middleware tiers ([App.PreRouting] before the match,
// [App.Middleware] after it), the built-in HTTP features installed through
// its Use methods, the dependency injection container with the
// configuration, the health checks, the workers, and the HTTP server with
// its lifecycle.
//
// It holds no error handler to replace: an error that a handler or a
// middleware returns goes through the framework's error pipeline, and
// [App.UseErrorRenderer] shapes the body the pipeline writes.
type App struct {
	// logger is the application-level logger, set via WithLogger.
	// Used by framework internals (error pipeline, server lifecycle, i18n setup).
	// Component-scoped loggers (Infra.Logger, component=<name>) are derived
	// from this.
	logger *slog.Logger

	// lifecycle owns the state machine, the start phase, the bound server and
	// session context, the start and stop hooks, and the drain in tiers. The
	// public Run/Start/Shutdown/State/Addr/OnStart/OnStop methods delegate to
	// it.
	lifecycle *lifecycleManager

	// reload holds the slot serializing Reload (ADR-020).
	reload reloadState

	appPrep
	appRouting
	appRequest
	appFeatures
	appHealth
	appDI
	appServer
}

// New creates a new App with the given options. When no RawConfig is provided,
// New auto-loads configuration with config.Load and registers the resulting
// RawConfig in DI. Passing WithRawConfig bypasses auto-load and registers the
// given RawConfig instead.
//
// The automatic load reads what it finds in the working directory and the
// environment, so New records what it read: one log record naming the
// configuration files found, the .env file read and the environment prefix
// ([config.Sources]), and never a key or a value. The record is written at
// Info when a file or a .env was read and at Debug when the load found only
// the environment. With WithRawConfig New loads nothing and writes no record;
// an application that loads explicitly logs cfg.Sources() itself.
//
// Without WithLogger, New builds the App's logger from the "log" section when
// the configuration has one: log.level (debug, info, warn, error; info by
// default) and log.format (text, json; text by default), written to stderr.
// The section is the framework's and is decoded strictly, so an application
// section of that name fails New until it is renamed. A reload applies a new
// level and a new format to the logger and to every logger derived from it.
// With WithLogger the section is not read. Without the section the logger is
// the framework default, a text handler on stderr at Info.
//
// New returns an error if configuration loading fails, or the server or log
// settings contain invalid values (negative timeouts, an invalid port, an
// unknown log level). An option only records configuration and cannot fail
// by itself.
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
	o, err := parseOptions(opts)
	if err != nil {
		return nil, err
	}
	if err = loadRawConfig(&o); err != nil {
		return nil, err
	}
	// The record of the automatic load comes before any other read of the
	// configuration, so a New that fails on a setting has already named the
	// files the setting came from; when the setting is the logger's own, the
	// record goes to the framework default logger.
	logger, err := resolveLogger(&o)
	if err != nil {
		logAutoLoad(defaultLogger, o.autoLoaded)
		return nil, err
	}
	baseInfra := newInfra(logger)
	logAutoLoad(baseInfra.Logger, o.autoLoaded)
	serverCfg, err := resolveServerConfig(&o)
	if err != nil {
		return nil, err
	}
	trustedProxies, err := internalproxy.ParsePrefixes(serverCfg.TrustedProxies)
	if err != nil {
		return nil, fmt.Errorf("credo: invalid trusted proxies: %w", err)
	}
	c := buildContainer(o.rawConfig, baseInfra)
	return assembleApp(&o, serverCfg, c, baseInfra.Logger, trustedProxies), nil
}

// parseOptions applies the functional options and validates the values that
// can be checked without configuration.
func parseOptions(opts []Option) (appOptions, error) {
	var o appOptions
	for _, opt := range opts {
		opt(&o)
	}
	return o, nil
}

// loadRawConfig auto-loads configuration with defaults when WithRawConfig was
// not given.
func loadRawConfig(o *appOptions) error {
	if o.rawConfig != nil {
		return nil
	}
	rc, err := config.Load()
	if err != nil {
		return fmt.Errorf("credo: auto-load config: %w", err)
	}
	o.rawConfig = rc
	o.autoLoaded = rc
	return nil
}

// autoLoadRecord is the record New writes about the automatic load.
const autoLoadRecord = "credo: configuration loaded automatically"

// logAutoLoad records what the automatic load read: the files found, the
// .env file and the environment prefix, never a value. A file on disk is
// what an application's author may not know of, so it is reported at Info;
// a load that found only the environment is reported at Debug. loaded is nil
// when the application passed its own RawConfig, and nothing is written.
func logAutoLoad(logger *slog.Logger, loaded *config.Config) {
	if loaded == nil {
		return
	}
	sources := loaded.Sources()
	level := slog.LevelDebug
	if sources.FromFile() {
		level = slog.LevelInfo
	}
	logger.LogAttrs(context.Background(), level, autoLoadRecord, slog.Any("sources", sources))
}

// serverSection is the framework's own section. It is read through its
// descriptor, so the configuration records it as static and a live read of it
// fails: a server setting takes a restart.
const serverSection config.Section[serverConfig] = "server"

// resolveServerConfig produces the effective server settings: the "server"
// config section when present, the explicit With* options overlaid on top
// (see appOptions.overlayServer), then framework defaults and validation. A
// missing section is fine (defaults apply); a decode error is surfaced.
func resolveServerConfig(o *appOptions) (serverConfig, error) {
	var cfg serverConfig
	if o.rawConfig.Exists(string(serverSection)) {
		decoded, err := serverSection.Decode(o.rawConfig)
		if err != nil {
			return serverConfig{}, fmt.Errorf("credo: invalid server config: %w", err)
		}
		cfg = decoded
	}
	o.overlayServer(&cfg)
	applyServerDefaults(&cfg)
	if err := validateServerConfig(&cfg); err != nil {
		return serverConfig{}, err
	}
	return cfg, nil
}

// buildContainer creates the DI container with the framework-provided
// bindings: Infra auto-injection (Model 1) and the always-available RawConfig
// (auto-loaded or explicit).
func buildContainer(rawConfig RawConfig, baseInfra Infra) *di.Container {
	c := di.New()
	c.SetFrameworkProvider(di.FrameworkProvider{
		Type: reflect.TypeFor[Infra](),
		Factory: func(unitName string) any {
			return Infra{Logger: baseInfra.Logger.With("component", unitName)}
		},
	})
	c.SetLogger(baseInfra.Logger)
	c.SetCallContext(func(ctx context.Context, unitName string) context.Context {
		return context.WithValue(ctx, infraKey{}, Infra{Logger: baseInfra.Logger.With("component", unitName)})
	})
	c.MustProvideValue[RawConfig](rawConfig)
	return c
}

// assembleApp wires the App value from the resolved pieces: router, root
// group and lifecycle manager.
func assembleApp(
	o *appOptions,
	serverCfg serverConfig,
	c *di.Container,
	logger *slog.Logger,
	trustedProxies []netip.Prefix,
) *App {
	app := &App{
		logger: logger,
		appRouting: appRouting{
			mux:                   newMux(),
			staticHosts:           make(map[string]*hostEntry),
			namedRoutes:           make(map[string]*Route),
			redirectTrailingSlash: serverCfg.RedirectTrailingSlash == nil || *serverCfg.RedirectTrailingSlash,
		},
		appRequest: appRequest{
			debug:          o.debug || serverCfg.Debug,
			strictBodies:   o.strictBodies || serverCfg.StrictBodies,
			jsonOpts:       jsonv2.JoinOptions(append([]jsonv2.Options{defaultJSONOptions}, o.jsonOptions...)...),
			trustedProxies: trustedProxies,
		},
		appDI: appDI{
			container: c,
			rawConfig: o.rawConfig,
		},
		appServer: appServer{
			serverCfg:            serverCfg,
			tlsConfig:            o.tlsConfig.value,
			tlsConfigSet:         o.tlsConfig.isSet,
			tlsFilesSet:          o.tlsFiles.isSet,
			httpRedirectAddr:     o.httpRedirectAddr,
			disableReloadSignals: o.disableReloadSignals,
			configureServer:      o.configureServer,
		},
	}
	app.errorJSONOpts = jsonv2.JoinOptions(app.jsonOpts, jsonv2.Deterministic(true))
	if !o.disableRecover {
		app.recover = newRecoverFeature(o.recoverCfg)
	}
	app.root = &Group{registrar: app}
	app.lifecycle = &lifecycleManager{deps: lifecycleDeps{
		logger:     app.Logger(),
		container:  c,
		config:     app.rawConfig,
		server:     &app.appServer,
		reloadSlot: &app.reload,
		prep:       &app.appPrep,
		newInfra:   app.NewInfra,
		prepare:    app.prepare,
		newServer: func() *http.Server {
			return buildServer(app.serverCfg, app, app.Logger(), app.configureServer)
		},
		features: app.effectiveFeatures,
		reload: func(ctx context.Context) error {
			_, err := app.Reload(ctx)
			return err
		},
	}}
	return app
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

// QUERY registers an RFC 10008 safe, idempotent query route whose query
// representation travels in the request body. [Request.BindBody] provides the
// standard decode-and-validate path. QUERY requests must include Content-Type.
// Panics under the same conditions as [App.GET].
func (app *App) QUERY(pattern string, h Handler) *Route {
	return app.root.QUERY(pattern, h)
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

// PreRouting appends middleware that runs before the match, on every request,
// the router's own 404 and 405 included. No route is matched yet, so a
// middleware here cannot read route meta: one that does belongs on
// [App.Middleware]. PreRouting is for what acts before the match or on a
// request no route matches — CORS answering a preflight, Rewrite changing
// the path that routing matches, Secure's headers and a rate limit that
// cover the 404 and 405 too.
//
// The user tiers run PreRouting → [App.Middleware] → [Group.Middleware] →
// [Route.Middleware], each in registration order. Must be called before the
// server starts; panics if called after compile.
func (app *App) PreRouting(middlewares ...Middleware) {
	app.checkFrozen("App.PreRouting")
	app.preRoutingMW = append(app.preRoutingMW, middlewares...)
}

// Middleware appends middleware to the routed tier: it runs on every route
// registered through the App — directly, in a group, in a host group, as a
// static route or as a mount — after the match and before group and route
// middleware, so it reads the matched route and its meta:
//
//	app.Middleware(middleware.ContractGuard()) // never runs for a 404 or a 405
//
// It does not run for the router's own 404 and 405, which reach the
// [App.PreRouting] tier only. Like group middleware it is collected when the
// routes' chains are compiled, so it applies to routes registered before the
// call too. Must be called before the server starts; panics if called after
// compile.
func (app *App) Middleware(middlewares ...Middleware) {
	app.checkFrozen("App.Middleware")
	app.root.mws = append(app.root.mws, middlewares...)
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
// identical match semantics, contains an invalid wildcard, a parameter without
// a name, a parameter name used twice, an empty or invalid regex constraint,
// or a port. Registering a route on the returned Group panics if
// a route parameter name collides with a host parameter name.
// Must be called before the server starts; panics if called after compile.
func (app *App) Host(pattern string) *Group {
	app.checkFrozen("App.Host")

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
		registrar:   app,
		parent:      app.root, // inherit app-level meta
		mux:         m,
		hostPattern: normalized,
	}
}

// --- Status Handlers ---

// StatusHandler sets an app-level custom handler for the router's own 404 or
// 405 answer. The handler registered for [http.StatusNotFound] runs when no
// route matches the request; the one for [http.StatusMethodNotAllowed] runs
// when routes match the path but none of them serves the method, with the
// Allow header already set. It answers in place of the default error response
// for that case, inside the [App.PreRouting] middleware and without a matched
// route ([Context.HasRoute] reports false); the routed tier does not run.
//
// Only these two codes are consulted, and StatusHandler panics for any other
// code, so a registration that would never run fails at startup instead of
// being silently ignored. No other code is supported: a 5xx handler would
// re-enter application code inside error rendering. The handlers are not
// error handlers either: an error that a route handler or a middleware
// returns — [ErrNotFound] included — goes through the centralized error
// handling and does not reach them; use [App.UseErrorRenderer] to shape error
// bodies.
//
// Must be called before the server starts; panics if called after compile.
func (app *App) StatusHandler(code int, h Handler) {
	app.checkFrozen("App.StatusHandler")
	if code != http.StatusNotFound && code != http.StatusMethodNotAllowed {
		panic(fmt.Sprintf("credo: App.StatusHandler(%d): only 404 (http.StatusNotFound) and "+
			"405 (http.StatusMethodNotAllowed) are consulted; return an error from the handler "+
			"and shape its body with App.UseErrorRenderer instead", code))
	}
	if app.statusHandlers == nil {
		app.statusHandlers = make(map[int]Handler)
	}
	app.statusHandlers[code] = h
}

// --- Meta ---

// SetMeta sets the App's value of a meta key, which every route inherits
// unless a group or the route sets the key itself. Must be called before the
// server starts; panics if called after compile, and for a zero Key.
func (app *App) SetMeta[T any](key Key[T], val T) {
	key.mustID("App.SetMeta")
	app.root.SetMeta(key, val)
}

// RemoveMeta removes the App's value of a meta key. Must be called before the
// server starts; panics if called after compile, and for a zero Key.
func (app *App) RemoveMeta[T any](key Key[T]) {
	key.mustID("App.RemoveMeta")
	app.root.RemoveMeta(key)
}

// Routes returns introspection data for every registered route across the
// default mux and all host-scoped muxes, plus one entry per [App.Mount] prefix.
// See [RouteInfo] for the per-entry fields, including the route Name, the
// resolved meta ([RouteInfo.GetMeta]), and a mount's forwarded method set.
//
// The result is a deterministic total order — sorted by (Host, Pattern, Method,
// Kind), independent of registration order and host compile-sort state — so
// route/permission catalogs and golden-file tests stay stable. The returned
// slice is freshly allocated; the caller owns it.
//
// Routes reads live *Route fields (Name, metadata) without locking, so it must
// not run concurrently with route registration or configuration (Name, SetMeta,
// Mount). It is a post-wiring (or post-freeze) operation.
func (app *App) Routes() []RouteInfo {
	routes := app.mux.Routes()
	count := len(routes) + len(app.mounts)
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
	for _, m := range app.mounts {
		out = append(out, RouteInfo{
			Kind:         RouteKindMount,
			Pattern:      m.route.pattern,
			Methods:      mountForwardedMethods(),
			RegisteredAt: m.registeredAt.String(),
			meta:         m.route.resolveAllMeta(),
		})
	}

	// Total order independent of registration order, host compile-sort state,
	// and mount fan-out — catalog and golden-file generation need determinism.
	slices.SortStableFunc(out, compareRouteInfo)
	return out
}

// compareRouteInfo orders RouteInfo by (Host, Pattern, Method, Kind), giving
// App.Routes a stable, deterministic total order.
func compareRouteInfo(a, b RouteInfo) int {
	return cmp.Or(
		cmp.Compare(a.Host, b.Host),
		cmp.Compare(a.Pattern, b.Pattern),
		cmp.Compare(a.Method, b.Method),
		cmp.Compare(string(a.Kind), string(b.Kind)),
	)
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
