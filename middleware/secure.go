// Copyright (c) 2024 LabStack.
// Originally derived from github.com/labstack/echo/middleware (MIT License).

package middleware

import (
	"strconv"

	"github.com/credo-go/credo"
)

// SecureConfig defines configuration for Secure middleware.
type SecureConfig struct {
	// Skipper defines a function to skip middleware.
	Skipper Skipper

	// XSSProtection sets the X-XSS-Protection header value.
	XSSProtection string

	// ContentTypeNosniff sets the X-Content-Type-Options header value.
	ContentTypeNosniff string

	// XFrameOptions sets the X-Frame-Options header value.
	XFrameOptions string

	// HSTSMaxAge sets Strict-Transport-Security max-age in seconds.
	HSTSMaxAge int

	// HSTSExcludeSubdomains disables includeSubDomains in HSTS.
	HSTSExcludeSubdomains bool

	// HSTSPreloadEnabled adds preload token in HSTS.
	HSTSPreloadEnabled bool

	// ContentSecurityPolicy sets CSP or CSP-Report-Only header value.
	ContentSecurityPolicy string

	// CSPReportOnly uses Content-Security-Policy-Report-Only header.
	CSPReportOnly bool

	// ReferrerPolicy sets the Referrer-Policy header value.
	ReferrerPolicy string
}

// DefaultSecureConfig returns the default Secure middleware config.
// Each call returns a fresh value, so callers cannot mutate the
// package-wide defaults.
func DefaultSecureConfig() SecureConfig {
	return SecureConfig{
		Skipper:            DefaultSkipper,
		XSSProtection:      "1; mode=block",
		ContentTypeNosniff: "nosniff",
		XFrameOptions:      "SAMEORIGIN",
	}
}

// Secure returns middleware that sets common security headers.
func Secure(cfg ...SecureConfig) credo.Middleware {
	config := resolveConfig(cfg, DefaultSecureConfig(), normalizeSecureConfig)

	// Pre-compute the HSTS header value (config is immutable after this point).
	var hstsValue string
	if config.HSTSMaxAge != 0 {
		suffix := ""
		if !config.HSTSExcludeSubdomains {
			suffix = "; includeSubDomains"
		}
		if config.HSTSPreloadEnabled {
			suffix += "; preload"
		}
		hstsValue = "max-age=" + strconv.FormatInt(int64(config.HSTSMaxAge), 10) + suffix
	}

	return func(next credo.Handler) credo.Handler {
		return func(ctx *credo.Context) error {
			if config.Skipper(ctx) {
				return next(ctx)
			}

			headers := ctx.Response().Header()

			if config.XSSProtection != "" {
				headers.Set("X-XSS-Protection", config.XSSProtection)
			}
			if config.ContentTypeNosniff != "" {
				headers.Set("X-Content-Type-Options", config.ContentTypeNosniff)
			}
			if config.XFrameOptions != "" {
				headers.Set("X-Frame-Options", config.XFrameOptions)
			}

			if hstsValue != "" && ctx.Request().Scheme() == "https" {
				headers.Set("Strict-Transport-Security", hstsValue)
			}

			if config.ContentSecurityPolicy != "" {
				if config.CSPReportOnly {
					headers.Set("Content-Security-Policy-Report-Only", config.ContentSecurityPolicy)
				} else {
					headers.Set("Content-Security-Policy", config.ContentSecurityPolicy)
				}
			}

			if config.ReferrerPolicy != "" {
				headers.Set("Referrer-Policy", config.ReferrerPolicy)
			}

			return next(ctx)
		}
	}
}

func normalizeSecureConfig(config SecureConfig) SecureConfig {
	if config.Skipper == nil {
		config.Skipper = DefaultSkipper
	}
	return config
}
