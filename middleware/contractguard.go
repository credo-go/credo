package middleware

import (
	"net/http"
	"slices"
	"strings"

	"github.com/credo-go/credo"
)

// Contract meta keys. Set them on a route or group with SetMeta; ContractGuard
// reads them — with parent-chain inheritance via [credo.Route.LookupMeta] — and
// enforces the matching request contract before the handler runs:
//
//	api := app.Group("/api")
//	api.SetMeta(middleware.MetaAccept, "application/json") // group-wide
//	api.POST("/users", createUser).
//	    SetMeta(middleware.MetaMaxBody, int64(1<<20)).      // 1 MiB
//	    SetMeta(middleware.MetaRequireHeaders, []string{"X-Request-Id"})
//
// Accepted value types:
//
//	MetaAccept          string | []string  -> 415 Unsupported Media Type
//	MetaMaxBody         int | int32 | int64 (bytes) -> 413 Payload Too Large
//	MetaRequireHeaders  string | []string  -> 400 Bad Request
//	MetaRequireQuery    string | []string  -> 400 Bad Request
//	MetaAPIVersion      string | []string  -> 400 Bad Request
//	MetaScope           string | []string  -> 403 Forbidden (needs ScopeChecker)
const (
	// MetaAccept restricts the request Content-Type to the listed media types.
	// Values may use a "type/*" or "*/*" wildcard. Requests without a
	// Content-Type header pass (there is no body to police).
	MetaAccept = "accept"

	// MetaMaxBody caps the request body size in bytes for the route. It is
	// enforced both eagerly (Content-Length) and while streaming (a tighter
	// http.MaxBytesReader layered on the global server limit — defense in
	// depth). A negative value disables the per-route cap.
	MetaMaxBody = "max_body"

	// MetaRequireHeaders lists request headers that must be present and
	// non-empty.
	MetaRequireHeaders = "require_headers"

	// MetaRequireQuery lists query parameters that must be present and
	// non-empty.
	MetaRequireQuery = "require_query"

	// MetaAPIVersion lists the acceptable API versions. The request version is
	// read from the configured header (default "X-API-Version"), falling back
	// to a "version" route parameter (e.g. /v{version}/...).
	MetaAPIVersion = "api_version"

	// MetaScope lists scopes the request must satisfy (all of them). It is
	// evaluated by ContractConfig.ScopeChecker; if no checker is configured,
	// a route declaring this contract is denied (a declared scope is never
	// silently bypassed). This shares the "scope" meta key documented for the
	// auth package.
	MetaScope = "scope"
)

// ContractConfig configures the [ContractGuard] middleware.
type ContractConfig struct {
	// Skipper defines a function to skip the middleware for a request.
	Skipper Skipper

	// ScopeChecker enforces the MetaScope contract. It receives the request
	// context and a single required scope and reports whether the request
	// satisfies it. Because authenticated users are stored generically
	// (auth.GetUser[T]), the framework cannot inspect scopes on its own —
	// supply this to bridge to your auth model. When nil, any route that
	// declares a scope contract is rejected with 403.
	ScopeChecker func(ctx *credo.Context, requiredScope string) bool

	// APIVersionHeader is the request header inspected for the MetaAPIVersion
	// contract. Defaults to "X-API-Version".
	APIVersionHeader string

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

// ContractGuard returns a single middleware that enforces declarative,
// meta-driven request contracts. Routes (or groups) opt in by setting the
// Meta* keys; routes without any contract meta pass through untouched.
//
// Register it at the group (or route) level, not via App.GlobalMiddleware:
// the guard reads matched-route metadata, and a route is only matched after
// app-global middleware has run. Group and route middleware run after the
// match, so the route — and its inherited group meta — is available there.
//
//	api := app.Group("/api")
//	api.Middleware(middleware.ContractGuard())
//	api.POST("/users", createUser).
//	    SetMeta(middleware.MetaAccept, "application/json").
//	    SetMeta(middleware.MetaMaxBody, int64(1<<20))
//
// Applied globally the guard is a safe no-op (it skips when no route is
// matched) rather than an error. Each contract maps a route Meta key to an
// HTTP rejection: see the Meta* constants for the value types and status
// codes.
func ContractGuard(cfg ...ContractConfig) credo.Middleware {
	config := resolveConfig(cfg, DefaultContractConfig(), normalizeContractConfig)

	return func(next credo.Handler) credo.Handler {
		return func(ctx *credo.Context) error {
			if config.Skipper(ctx) || !ctx.HasRoute() {
				return next(ctx)
			}
			route := ctx.Route()

			if v, ok := route.LookupMeta(MetaAccept); ok {
				if err := checkAccept(ctx, contractStrings(v)); err != nil {
					return err
				}
			}
			if v, ok := route.LookupMeta(MetaRequireHeaders); ok {
				if err := checkRequireHeaders(ctx, contractStrings(v)); err != nil {
					return err
				}
			}
			if v, ok := route.LookupMeta(MetaRequireQuery); ok {
				if err := checkRequireQuery(ctx, contractStrings(v)); err != nil {
					return err
				}
			}
			if v, ok := route.LookupMeta(MetaAPIVersion); ok {
				if err := checkAPIVersion(ctx, config.APIVersionHeader, contractStrings(v)); err != nil {
					return err
				}
			}
			if v, ok := route.LookupMeta(MetaScope); ok {
				if err := checkScope(ctx, config.ScopeChecker, contractStrings(v)); err != nil {
					return err
				}
			}
			if v, ok := route.LookupMeta(MetaMaxBody); ok {
				if limit, ok := contractInt64(v); ok {
					if err := enforceMaxBody(ctx, limit); err != nil {
						return err
					}
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
// Requests without a Content-Type header pass (no body to police).
func checkAccept(ctx *credo.Context, accepted []string) error {
	ct := ctx.Request().Header.Get("Content-Type")
	if ct == "" {
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
	return credo.NewHTTPError(http.StatusUnsupportedMediaType,
		"content type not accepted: "+ct)
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
			return credo.NewHTTPError(http.StatusBadRequest,
				"missing required header: "+name)
		}
	}
	return nil
}

func checkRequireQuery(ctx *credo.Context, names []string) error {
	for _, name := range names {
		if ctx.Request().QueryParam(name) == "" {
			return credo.NewHTTPError(http.StatusBadRequest,
				"missing required query parameter: "+name)
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
	return credo.NewHTTPError(http.StatusBadRequest, "unsupported or missing API version")
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
			return credo.NewHTTPError(http.StatusForbidden, "missing required scope: "+s)
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
		return credo.NewHTTPError(http.StatusRequestEntityTooLarge, "request body exceeds limit")
	}
	if req.Body != nil {
		req.Body = http.MaxBytesReader(ctx.Response().Unwrap(), req.Body, limit)
	}
	return nil
}

// contractStrings coerces a Meta value into a string slice. It accepts a single
// string, a []string, or a []any of strings.
func contractStrings(v any) []string {
	switch t := v.(type) {
	case string:
		return []string{t}
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

// contractInt64 coerces a Meta value into an int64 byte count.
func contractInt64(v any) (int64, bool) {
	switch t := v.(type) {
	case int64:
		return t, true
	case int:
		return int64(t), true
	case int32:
		return int64(t), true
	default:
		return 0, false
	}
}
