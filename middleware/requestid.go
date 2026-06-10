// Copyright (c) 2015-present Peter Kieltyka (https://github.com/pkieltyka), Google Inc.
// Originally derived from github.com/go-chi/chi/middleware (MIT License).

package middleware

import (
	"github.com/credo-go/credo"
	internalrequestid "github.com/credo-go/credo/internal/requestid"
)

// RequestIDKey is the key used to store the request ID in the credo.Context
// request-scoped store. Exported for advanced integrations. Prefer
// [credo.Context.RequestID] or [GetRequestID] for normal request access.
const RequestIDKey = internalrequestid.Key

// RequestIDConfig defines configuration for the RequestID middleware.
type RequestIDConfig struct {
	// Header is the HTTP header name used to read/write the request ID.
	// Default: "X-Request-Id".
	Header string

	// Generator creates a new request ID when one is not provided by the
	// client. Default: crypto/rand.Text (128-bit base32 string).
	Generator func() string

	// Limit is the maximum allowed length for incoming request IDs. IDs
	// exceeding this length are discarded and a new one is generated.
	// This prevents clients from injecting arbitrarily large values.
	// Default: 64.
	Limit int
}

// DefaultRequestIDConfig returns the default RequestID middleware config.
// Each call returns a fresh value, so callers cannot mutate the
// package-wide defaults.
func DefaultRequestIDConfig() RequestIDConfig {
	return RequestIDConfig{
		Header:    internalrequestid.Header,
		Generator: internalrequestid.Generate,
		Limit:     internalrequestid.DefaultLimit,
	}
}

// RequestID returns middleware that injects a unique request ID into each
// request's context and response headers. If the incoming request already
// has an X-Request-Id header (within the configured length limit), that
// value is used. Otherwise, a new ID is generated.
//
// Like the built-in request ID tier, it also enriches the request-scoped
// logger with a "request_id" attribute (via [credo.Context.AddLogAttrs]),
// so handler logs and the access log carry the ID automatically.
//
// The request ID can be retrieved in downstream handlers via
// [credo.Context.RequestID] or [GetRequestID].
func RequestID(cfg ...RequestIDConfig) credo.Middleware {
	config := resolveConfig(cfg, DefaultRequestIDConfig(), normalizeRequestIDConfig)

	return func(next credo.Handler) credo.Handler {
		return func(ctx *credo.Context) error {
			id := internalrequestid.Resolve(ctx.Request().Header.Get(config.Header), config.Limit, config.Generator)

			// Store in context-local store (no context.WithValue alloc).
			ctx.Set(RequestIDKey, id)

			// Set on response header
			ctx.Response().Header().Set(config.Header, id)

			// Enrich the request-scoped logger, mirroring the built-in tier.
			ctx.AddLogAttrs("request_id", id)

			return next(ctx)
		}
	}
}

func normalizeRequestIDConfig(config RequestIDConfig) RequestIDConfig {
	defaults := DefaultRequestIDConfig()
	if config.Header == "" {
		config.Header = defaults.Header
	}
	if config.Generator == nil {
		config.Generator = defaults.Generator
	}
	if config.Limit <= 0 {
		config.Limit = defaults.Limit
	}
	return config
}

// GetRequestID returns the request ID from the Credo context.
// It returns an empty string if no request ID has been set.
func GetRequestID(ctx *credo.Context) string {
	return ctx.RequestID()
}
