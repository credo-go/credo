// Copyright (c) 2024 LabStack.
// Originally derived from github.com/labstack/echo/middleware (MIT License).

package middleware

import (
	"strconv"

	"github.com/credo-go/credo"
)

// SecureConfig defines configuration for Secure middleware. A zero field
// selects its default: a config that sets one field keeps the defaults of the
// others, so the two headers that are sent by default, X-Content-Type-Options
// and X-Frame-Options, are turned off only by their Disable fields.
type SecureConfig struct {
	// Skipper defines a function to skip middleware.
	// Default: no request is skipped.
	Skipper Skipper

	// XSSProtection sets the X-XSS-Protection header value.
	// Default: the header is not sent. Browsers have removed the XSS
	// auditor it controlled; an application that still wants to switch the
	// auditor off in an old browser sets "0".
	XSSProtection string

	// DisableContentTypeNosniff stops the middleware from sending
	// "X-Content-Type-Options: nosniff", the header's one value.
	// Default: the header is sent.
	DisableContentTypeNosniff bool

	// XFrameOptions sets the X-Frame-Options header value.
	// Default: "SAMEORIGIN".
	XFrameOptions string

	// DisableXFrameOptions stops the middleware from sending
	// X-Frame-Options, for an application whose Content-Security-Policy
	// decides framing with frame-ancestors. When it is set, XFrameOptions
	// is not read.
	// Default: the header is sent.
	DisableXFrameOptions bool

	// HSTSMaxAge sets Strict-Transport-Security max-age in seconds.
	// Default: the header is not sent.
	HSTSMaxAge int

	// HSTSExcludeSubdomains disables includeSubDomains in HSTS.
	HSTSExcludeSubdomains bool

	// HSTSPreloadEnabled adds preload token in HSTS.
	HSTSPreloadEnabled bool

	// ContentSecurityPolicy sets CSP or CSP-Report-Only header value.
	// Default: the header is not sent.
	ContentSecurityPolicy string

	// CSPReportOnly uses Content-Security-Policy-Report-Only header.
	CSPReportOnly bool

	// ReferrerPolicy sets the Referrer-Policy header value.
	// Default: the header is not sent.
	ReferrerPolicy string
}

// Secure returns middleware that sets common security headers. With no
// config, or with a config that leaves them out, it sends
// "X-Content-Type-Options: nosniff" and "X-Frame-Options: SAMEORIGIN"; every
// other header is sent only when its field is set.
func Secure(cfg ...SecureConfig) credo.Middleware {
	config := resolveConfig(cfg, normalizeSecureConfig)

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
			if !config.DisableContentTypeNosniff {
				headers.Set("X-Content-Type-Options", "nosniff")
			}
			if !config.DisableXFrameOptions {
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
		config.Skipper = neverSkip
	}
	if config.XFrameOptions == "" {
		config.XFrameOptions = "SAMEORIGIN"
	}
	return config
}
