package middleware

import (
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/credo-go/credo"
)

// Contract meta keys. Set them on a route, a group or the App with SetMeta;
// ContractGuard reads them — the route's value, else its groups', else the
// App's, through [credo.Route.GetMeta] — and enforces the matching request
// contract before the handler runs. Each key's value type is checked at
// compile time:
//
//	api := app.Group("/api")
//	api.SetMeta(middleware.MetaAccept, []string{"application/json"}) // group-wide
//	api.POST("/users", createUser).
//	    SetMeta(middleware.MetaMaxBody, 1<<20). // 1 MiB
//	    SetMeta(middleware.MetaRequireHeaders, []string{"X-Request-Id"})
//
// The rejections:
//
//	MetaAccept          -> 415 Unsupported Media Type
//	MetaMaxBody         -> 413 Payload Too Large
//	MetaRequireHeaders  -> 400 Bad Request
//	MetaRequireQuery    -> 400 Bad Request
//	MetaAPIVersion      -> 400 Bad Request
//	MetaScope           -> 403 Forbidden (needs ScopeChecker)
//
// A route overrides a contract inherited from its group by setting the key
// itself, and lifts an inherited requirement with the value that requires
// nothing: an empty list for MetaRequireHeaders, MetaRequireQuery and
// MetaScope, []string{"*/*"} for MetaAccept (every media type;
// ContractConfig.RequireContentType still applies), a negative value for
// MetaMaxBody. An empty MetaAPIVersion list accepts no version, so that
// contract can be replaced but not lifted.
var (
	// MetaAccept restricts the request Content-Type to the listed media types.
	// Values may use a "type/*" or "*/*" wildcard. Requests without a
	// Content-Type header (or with an empty one) pass by default; set
	// ContractConfig.RequireContentType to reject them with 415 when the
	// request carries a body.
	MetaAccept = credo.NewKey[[]string]("accept")

	// MetaMaxBody caps the request body size in bytes for the route. It is
	// enforced both eagerly (Content-Length) and while streaming (a tighter
	// http.MaxBytesReader layered on the global server limit — defense in
	// depth). A negative value disables the per-route cap.
	MetaMaxBody = credo.NewKey[int64]("max_body")

	// MetaRequireHeaders lists request headers that must be present and
	// non-empty.
	MetaRequireHeaders = credo.NewKey[[]string]("require_headers")

	// MetaRequireQuery lists query parameters that must be present and
	// non-empty.
	MetaRequireQuery = credo.NewKey[[]string]("require_query")

	// MetaAPIVersion lists the acceptable API versions. The request version is
	// read from the configured header (default "X-API-Version"), falling back
	// to a "version" route parameter (e.g. /v{version}/...).
	MetaAPIVersion = credo.NewKey[[]string]("api_version")

	// MetaScope lists scopes the request must satisfy (all of them). It is
	// evaluated by ContractConfig.ScopeChecker; if no checker is configured,
	// a route declaring this contract is denied (a declared scope is never
	// silently bypassed).
	MetaScope = credo.NewKey[[]string]("scope")
)

// ContractConfig configures the [ContractGuard] middleware.
type ContractConfig struct {
	// Skipper defines a function to skip the middleware for a request.
	Skipper Skipper

	// ScopeChecker enforces the MetaScope contract. It receives the request
	// context and a single required scope and reports whether the request
	// satisfies it. Because authenticated users are stored generically
	// (c.GetUser[T]), the framework cannot inspect scopes on its own —
	// supply this to bridge to your auth model. When nil, any route that
	// declares a scope contract is rejected with 403.
	ScopeChecker func(ctx *credo.Context, requiredScope string) bool

	// APIVersionHeader is the request header inspected for the MetaAPIVersion
	// contract. Defaults to "X-API-Version".
	APIVersionHeader string

	// RequireContentType arms the MetaAccept contract against requests that
	// carry a body but no (or an empty) Content-Type header: such requests
	// are rejected with 415 instead of passing. A body is assumed when
	// Content-Length is positive or unknown (chunked, HTTP/2 streams);
	// requests with Content-Length 0 and bodiless requests pass, and routes
	// without a MetaAccept contract are unaffected. Default false.
	RequireContentType bool

	// CustomChecks run after all built-in contracts have passed, in order.
	// A check returns a non-nil error (typically a *credo.HTTPError) to reject
	// the request. This is the extension point for user-defined contracts.
	CustomChecks []func(ctx *credo.Context) error
}

// DefaultContractConfig returns the default [ContractGuard] configuration.
// Each call returns a fresh value, so callers cannot mutate the
// package-wide defaults.
func DefaultContractConfig() ContractConfig {
	return ContractConfig{
		Skipper:          DefaultSkipper,
		APIVersionHeader: "X-API-Version",
	}
}

// errContractNoRoute is the guard's answer to a request without a route.
var errContractNoRoute = errors.New("contractguard: no route is matched, so no contract can be checked: " +
	"the guard is registered with App.PreRouting, which runs before the match; " +
	"register it with app.Middleware, on a group or on a route; request denied")

// ContractGuard returns a single middleware that enforces declarative,
// meta-driven request contracts. Routes (or groups) opt in by setting the
// Meta* keys; routes without any contract meta pass through untouched.
//
// Register it on the routed tier, a group or a route: the guard reads the
// matched route's meta, which exists only after the match.
//
//	app.Middleware(middleware.ContractGuard()) // every route of the App
//	app.POST("/users", createUser).
//	    SetMeta(middleware.MetaAccept, []string{"application/json"}).
//	    SetMeta(middleware.MetaMaxBody, 1<<20)
//
// It fails closed without a route. Registered with App.PreRouting it sees no
// route on any request, so it answers 500 through the error pipeline for
// every request and logs one Error naming the registration and the remedy.
// Each contract maps a route meta key to an HTTP rejection: see the Meta*
// keys for the value types and status codes.
func ContractGuard(cfg ...ContractConfig) credo.Middleware {
	config := resolveConfig(cfg, DefaultContractConfig(), normalizeContractConfig)
	var reported atomic.Bool

	return func(next credo.Handler) credo.Handler {
		return func(ctx *credo.Context) error {
			if config.Skipper(ctx) {
				return next(ctx)
			}
			if !ctx.HasRoute() {
				if reported.CompareAndSwap(false, true) {
					ctx.Logger().LogAttrs(ctx.Context(), slog.LevelError, errContractNoRoute.Error())
				}
				return credo.NewHTTPError(http.StatusInternalServerError).WithInternal(errContractNoRoute)
			}
			route := ctx.Route()

			if accepted, ok := route.GetMeta(MetaAccept); ok {
				if err := checkAccept(ctx, accepted, config.RequireContentType); err != nil {
					return err
				}
			}
			if names, ok := route.GetMeta(MetaRequireHeaders); ok {
				if err := checkRequireHeaders(ctx, names); err != nil {
					return err
				}
			}
			if names, ok := route.GetMeta(MetaRequireQuery); ok {
				if err := checkRequireQuery(ctx, names); err != nil {
					return err
				}
			}
			if allowed, ok := route.GetMeta(MetaAPIVersion); ok {
				if err := checkAPIVersion(ctx, config.APIVersionHeader, allowed); err != nil {
					return err
				}
			}
			if required, ok := route.GetMeta(MetaScope); ok {
				if err := checkScope(ctx, config.ScopeChecker, required); err != nil {
					return err
				}
			}
			if limit, ok := route.GetMeta(MetaMaxBody); ok {
				if err := enforceMaxBody(ctx, limit); err != nil {
					return err
				}
			}
			for _, check := range config.CustomChecks {
				if err := check(ctx); err != nil {
					return err
				}
			}
			return next(ctx)
		}
	}
}

func normalizeContractConfig(c ContractConfig) ContractConfig {
	if c.Skipper == nil {
		c.Skipper = DefaultSkipper
	}
	if c.APIVersionHeader == "" {
		c.APIVersionHeader = DefaultContractConfig().APIVersionHeader
	}
	return c
}

// checkAccept enforces the Content-Type against the accepted media types.
// A missing or empty Content-Type passes unless require is set and the
// request carries a body, in which case it is rejected with 415.
func checkAccept(ctx *credo.Context, accepted []string, require bool) error {
	ct := ctx.Request().Header.Get("Content-Type")
	if strings.TrimSpace(ct) == "" {
		if require && requestHasBody(ctx.Request().Request) {
			return credo.NewHTTPError(http.StatusUnsupportedMediaType, "content_type_required")
		}
		return nil
	}
	got := ct
	if i := strings.IndexByte(got, ';'); i >= 0 {
		got = got[:i]
	}
	got = strings.TrimSpace(strings.ToLower(got))
	for _, a := range accepted {
		if mediaTypeMatches(got, strings.TrimSpace(strings.ToLower(a))) {
			return nil
		}
	}
	return credo.NewHTTPError(http.StatusUnsupportedMediaType, "content_type_not_accepted").
		WithDetails(map[string]any{"content_type": ct})
}

// requestHasBody reports whether the request is expected to carry a body
// without reading it: a positive Content-Length, or an unknown length (-1:
// chunked transfer or an HTTP/2 stream). Content-Length 0 — which the
// server also uses for bodiless requests — reports false.
func requestHasBody(req *http.Request) bool {
	return req.ContentLength > 0 || req.ContentLength == -1
}

// mediaTypeMatches reports whether got matches pattern, honoring "*/*" and
// "type/*" wildcards.
func mediaTypeMatches(got, pattern string) bool {
	switch {
	case pattern == "*/*" || pattern == "*":
		return true
	case strings.HasSuffix(pattern, "/*"):
		return strings.HasPrefix(got, pattern[:len(pattern)-1]) // keep "type/"
	default:
		return got == pattern
	}
}

func checkRequireHeaders(ctx *credo.Context, names []string) error {
	h := ctx.Request().Header
	for _, name := range names {
		if h.Get(name) == "" {
			return credo.NewHTTPError(http.StatusBadRequest, "missing_required_header").
				WithDetails(map[string]any{"header": name})
		}
	}
	return nil
}

func checkRequireQuery(ctx *credo.Context, names []string) error {
	for _, name := range names {
		if ctx.Request().QueryParam(name) == "" {
			return credo.NewHTTPError(http.StatusBadRequest, "missing_required_query_parameter").
				WithDetails(map[string]any{"query": name})
		}
	}
	return nil
}

func checkAPIVersion(ctx *credo.Context, header string, allowed []string) error {
	got := ctx.Request().Header.Get(header)
	if got == "" {
		got = ctx.Request().RouteParam("version")
	}
	if got != "" && slices.Contains(allowed, got) {
		return nil
	}
	return credo.NewHTTPError(http.StatusBadRequest, "unsupported_api_version")
}

// checkScope enforces that the request satisfies every required scope. A nil
// checker means the contract cannot be evaluated; rather than silently allow a
// route that declares a scope requirement, the request is denied.
func checkScope(ctx *credo.Context, checker func(*credo.Context, string) bool, required []string) error {
	if checker == nil {
		ctx.Logger().Warn("contractguard: route declares a scope contract but no ScopeChecker is configured; denying request",
			"scopes", required)
		return credo.NewHTTPError(http.StatusForbidden)
	}
	for _, s := range required {
		if !checker(ctx, s) {
			return credo.NewHTTPError(http.StatusForbidden, "missing_required_scope").
				WithDetails(map[string]any{"scope": s})
		}
	}
	return nil
}

// enforceMaxBody rejects an oversize body eagerly via Content-Length and wraps
// the body with a tighter http.MaxBytesReader so the limit is also enforced
// while streaming (the 413 then surfaces at read time via the framework's body
// decode error mapping). It layers on top of the global server limit.
func enforceMaxBody(ctx *credo.Context, limit int64) error {
	if limit < 0 {
		return nil // negative disables the per-route cap
	}
	req := ctx.Request()
	if req.ContentLength > limit {
		return credo.NewHTTPError(http.StatusRequestEntityTooLarge)
	}
	if req.Body != nil {
		req.Body = http.MaxBytesReader(ctx.Response().Unwrap(), req.Body, limit)
	}
	return nil
}
