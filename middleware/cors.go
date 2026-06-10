// Copyright (c) 2024 LabStack.
// Originally derived from github.com/labstack/echo/middleware (MIT License).

package middleware

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/internal/httpheader"
)

// CORSConfig defines configuration for CORS middleware.
type CORSConfig struct {
	// Skipper defines a function to skip middleware.
	Skipper Skipper

	// AllowOrigins defines allowed origins.
	// Default: ["*"].
	AllowOrigins []string

	// AllowOriginFunc overrides AllowOrigins matching.
	AllowOriginFunc func(ctx *credo.Context, origin string) (allowedOrigin string, allowed bool, err error)

	// AllowMethods defines allowed methods for preflight.
	// Default: GET, HEAD, PUT, PATCH, POST, DELETE.
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

type originMatcher struct {
	allowAll bool
	exact    map[string]string
	patterns []originPattern
}

type originPattern struct {
	prefix string
	suffix string
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
	config.AllowOrigins = append([]string(nil), config.AllowOrigins...)
	config.AllowMethods = append([]string(nil), config.AllowMethods...)
	config.AllowHeaders = append([]string(nil), config.AllowHeaders...)
	config.ExposeHeaders = append([]string(nil), config.ExposeHeaders...)

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

	if allowedOrigin, ok := matcher.exact[strings.ToLower(origin)]; ok {
		return allowedOrigin, true, nil
	}

	lowerOrigin := strings.ToLower(origin)
	for _, pattern := range matcher.patterns {
		// The length guard prevents the prefix and suffix from matching
		// overlapping regions of the origin: without it, the pattern
		// "https://api-*-prod.example.com" would match the origin
		// "https://api-prod.example.com" (the wildcard covering "negative"
		// text), allowing origins the pattern author never intended.
		if len(lowerOrigin) >= len(pattern.prefix)+len(pattern.suffix) &&
			strings.HasPrefix(lowerOrigin, pattern.prefix) &&
			strings.HasSuffix(lowerOrigin, pattern.suffix) {
			return origin, true, nil
		}
	}

	return "", false, nil
}

func compileOriginMatcher(allowOrigins []string) originMatcher {
	matcher := originMatcher{
		exact: make(map[string]string, len(allowOrigins)),
	}

	for _, allowed := range allowOrigins {
		allowed = strings.TrimSpace(allowed)
		if allowed == "" {
			continue
		}

		if allowed == "*" {
			matcher.allowAll = true
			continue
		}

		if pattern, ok := parseOriginPattern(allowed); ok {
			matcher.patterns = append(matcher.patterns, pattern)
			continue
		}

		matcher.exact[strings.ToLower(allowed)] = allowed
	}

	return matcher
}

func parseOriginPattern(pattern string) (originPattern, bool) {
	if pattern == "" || !strings.Contains(pattern, "*") {
		return originPattern{}, false
	}

	// Normalize to lowercase for case-insensitive matching, consistent
	// with exact origin matching in compileOriginMatcher.
	pattern = strings.ToLower(pattern)

	first := strings.IndexByte(pattern, '*')
	last := strings.LastIndexByte(pattern, '*')
	if first != last {
		return originPattern{}, false
	}

	return originPattern{
		prefix: pattern[:first],
		suffix: pattern[first+1:],
	}, true
}
