// Copyright (c) 2024 LabStack.
// Originally derived from github.com/labstack/echo/middleware (MIT License).

package middleware

import (
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/internal/httpheader"
	internalorigin "github.com/credo-go/credo/internal/origin"
)

const methodQuery = "QUERY"

// CORSConfig defines configuration for CORS middleware.
type CORSConfig struct {
	// Skipper defines a function to skip middleware.
	Skipper Skipper

	// AllowOrigins defines allowed origins. Each entry is either "*" (allow
	// every origin) or an origin in the strict scheme://host[:port] grammar
	// shared with the websocket adapter's AllowedOrigins: http or https only;
	// no path, query, fragment, or userinfo; scheme and host compare
	// case-insensitively; the scheme's default port is implied. One wildcard
	// may stand for exactly the left-most DNS label: "https://*.example.com"
	// matches "https://app.example.com" but not "https://example.com" or
	// "https://a.b.example.com". Any other shape (mid-label "*", several
	// wildcards, IP wildcards, empty entries) panics at construction.
	// Default: ["*"].
	AllowOrigins []string

	// AllowOriginFunc overrides AllowOrigins matching.
	AllowOriginFunc func(ctx *credo.Context, origin string) (allowedOrigin string, allowed bool, err error)

	// AllowMethods defines allowed methods for preflight.
	// Default: GET, HEAD, PUT, PATCH, POST, DELETE, QUERY.
	AllowMethods []string

	// AllowHeaders defines allowed request headers for preflight.
	// If empty, Access-Control-Request-Headers is echoed.
	AllowHeaders []string

	// AllowCredentials enables Access-Control-Allow-Credentials.
	AllowCredentials bool

	// ExposeHeaders defines exposed response headers.
	ExposeHeaders []string

	// MaxAge sets Access-Control-Max-Age in seconds.
	// Zero disables this header.
	MaxAge int
}

// originMatcher is the compiled AllowOrigins allow-list. Origin grammar and
// matching live in internal/origin so CORS and the websocket adapter share
// one definition.
type originMatcher struct {
	allowAll bool
	patterns []internalorigin.Pattern
}

// DefaultCORSConfig returns the default CORS middleware config.
// Each call returns a fresh value (including fresh slices), so callers
// cannot mutate the package-wide defaults.
func DefaultCORSConfig() CORSConfig {
	return CORSConfig{
		Skipper:      DefaultSkipper,
		AllowOrigins: []string{"*"},
		AllowMethods: []string{
			http.MethodGet,
			http.MethodHead,
			http.MethodPut,
			http.MethodPatch,
			http.MethodPost,
			http.MethodDelete,
			methodQuery,
		},
	}
}

// CORS returns CORS middleware.
func CORS(cfg ...CORSConfig) credo.Middleware {
	config := resolveConfig(cfg, DefaultCORSConfig(), normalizeCORSConfig)

	matcher := compileOriginMatcher(config.AllowOrigins)

	allowMethods := strings.Join(config.AllowMethods, ",")
	allowHeaders := strings.Join(config.AllowHeaders, ",")
	exposeHeaders := strings.Join(config.ExposeHeaders, ",")

	return func(next credo.Handler) credo.Handler {
		return func(ctx *credo.Context) error {
			if config.Skipper(ctx) {
				return next(ctx)
			}

			req := ctx.Request().Request
			resHeaders := ctx.Response().Header()
			origin := req.Header.Get("Origin")
			isPreflight := req.Method == http.MethodOptions && req.Header.Get("Access-Control-Request-Method") != ""

			httpheader.AddToken(resHeaders, "Vary", "Origin")

			if origin == "" {
				return next(ctx)
			}

			allowedOrigin, allowed, err := resolveAllowedOrigin(config, matcher, ctx, origin)
			if err != nil {
				return err
			}

			if !allowed {
				if isPreflight {
					return ctx.Response().NoContent(http.StatusNoContent)
				}
				return next(ctx)
			}

			if config.AllowCredentials && allowedOrigin == "*" {
				allowedOrigin = origin
			}

			resHeaders.Set("Access-Control-Allow-Origin", allowedOrigin)
			if config.AllowCredentials {
				resHeaders.Set("Access-Control-Allow-Credentials", "true")
			}

			if !isPreflight {
				if exposeHeaders != "" {
					resHeaders.Set("Access-Control-Expose-Headers", exposeHeaders)
				}
				return next(ctx)
			}

			httpheader.AddToken(resHeaders, "Vary", "Access-Control-Request-Method")
			httpheader.AddToken(resHeaders, "Vary", "Access-Control-Request-Headers")

			resHeaders.Set("Access-Control-Allow-Methods", allowMethods)
			if allowHeaders != "" {
				resHeaders.Set("Access-Control-Allow-Headers", allowHeaders)
			} else if reqHeaders := req.Header.Get("Access-Control-Request-Headers"); reqHeaders != "" {
				resHeaders.Set("Access-Control-Allow-Headers", reqHeaders)
			}

			if config.MaxAge != 0 {
				maxAge := max(config.MaxAge, 0)
				resHeaders.Set("Access-Control-Max-Age", strconv.Itoa(maxAge))
			}

			return ctx.Response().NoContent(http.StatusNoContent)
		}
	}
}

func normalizeCORSConfig(config CORSConfig) CORSConfig {
	config.AllowOrigins = slices.Clone(config.AllowOrigins)
	config.AllowMethods = slices.Clone(config.AllowMethods)
	config.AllowHeaders = slices.Clone(config.AllowHeaders)
	config.ExposeHeaders = slices.Clone(config.ExposeHeaders)

	if config.Skipper == nil {
		config.Skipper = DefaultSkipper
	}
	if len(config.AllowOrigins) == 0 && config.AllowOriginFunc == nil {
		config.AllowOrigins = []string{"*"}
	}
	if len(config.AllowMethods) == 0 {
		config.AllowMethods = DefaultCORSConfig().AllowMethods
	}

	return config
}

func resolveAllowedOrigin(cfg CORSConfig, matcher originMatcher, ctx *credo.Context, origin string) (string, bool, error) {
	if cfg.AllowOriginFunc != nil {
		return cfg.AllowOriginFunc(ctx, origin)
	}

	if matcher.allowAll {
		return "*", true, nil
	}

	// A malformed Origin header can never match an allow-list entry; it is
	// treated as a foreign origin rather than an error.
	parsed, err := internalorigin.Parse(origin)
	if err != nil {
		return "", false, nil
	}
	for _, pattern := range matcher.patterns {
		if pattern.Matches(parsed) {
			// Echo the request's own serialization: browsers compare
			// Access-Control-Allow-Origin byte-for-byte against the Origin
			// they sent, so the canonical form must not be substituted.
			return origin, true, nil
		}
	}

	return "", false, nil
}

// compileOriginMatcher parses AllowOrigins under the strict shared origin
// grammar. Invalid entries are configuration errors and panic.
func compileOriginMatcher(allowOrigins []string) originMatcher {
	var matcher originMatcher
	for i, allowed := range allowOrigins {
		if allowed == "*" {
			matcher.allowAll = true
			continue
		}
		pattern, err := internalorigin.ParsePattern(allowed)
		if err != nil {
			panic(fmt.Sprintf("credo: middleware.CORS: AllowOrigins[%d] %q: %v", i, allowed, err))
		}
		matcher.patterns = append(matcher.patterns, pattern)
	}
	return matcher
}
