package middleware

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/sethvargo/go-limiter"

	"github.com/credo-go/credo"
)

const (
	headerRateLimitLimit     = "X-RateLimit-Limit"
	headerRateLimitRemaining = "X-RateLimit-Remaining"
	headerRateLimitReset     = "X-RateLimit-Reset"
	headerRetryAfter         = "Retry-After"
	maxUnixNano              = uint64(1<<63 - 1)
)

// RateLimitConfig defines configuration for RateLimit middleware.
type RateLimitConfig struct {
	// Skipper defines a function to skip middleware.
	Skipper Skipper

	// Store is the backing limiter store.
	// Default: in-memory store with Tokens/Interval.
	Store limiter.Store

	// Tokens is the number of allowed requests per interval.
	// Default: 60.
	Tokens uint64

	// Interval is the refill interval.
	// Default: 1 minute.
	Interval time.Duration

	// KeyFunc builds a limiter key from the request context.
	// Default: client IP from ctx.Request().RealIP().
	KeyFunc func(ctx *credo.Context) (string, error)

	// InternalErrorHandler handles internal limiter/key errors (key
	// extraction failure, store failure). Over-limit responses go through
	// DeniedHandler instead.
	InternalErrorHandler func(ctx *credo.Context, err error) error

	// DeniedHandler handles over-limit responses.
	DeniedHandler func(ctx *credo.Context, limit, remaining uint64, reset time.Time) error
}

// DefaultRateLimitConfig returns the default RateLimit middleware config.
// Each call returns a fresh value, so callers cannot mutate the
// package-wide defaults.
func DefaultRateLimitConfig() RateLimitConfig {
	return RateLimitConfig{
		Skipper:  DefaultSkipper,
		Tokens:   60,
		Interval: time.Minute,
	}
}

// RateLimiter builds middleware from a normalized config and optionally owns
// the lifecycle of its internally created store.
type RateLimiter struct {
	config    RateLimitConfig
	ownsStore bool
	closeOnce sync.Once
}

var _ credo.Shutdowner = (*RateLimiter)(nil)

// NewRateLimiter creates a reusable rate limiter instance.
//
// If cfg.Store is nil, NewRateLimiter creates an internal in-memory store and
// owns its lifecycle. Call Close/Shutdown during app shutdown to release
// resources when using this constructor directly.
func NewRateLimiter(cfg RateLimitConfig) *RateLimiter {
	cfg = normalizeRateLimitConfig(cfg)

	ownsStore := false
	if cfg.Store == nil {
		cfg.Store = newInMemoryRateLimitStore(cfg.Tokens, cfg.Interval)
		ownsStore = true
	}

	return &RateLimiter{
		config:    cfg,
		ownsStore: ownsStore,
	}
}

// Middleware returns Credo middleware for this RateLimiter.
func (r *RateLimiter) Middleware() credo.Middleware {
	return buildRateLimitMiddleware(r.config)
}

func buildRateLimitMiddleware(config RateLimitConfig) credo.Middleware {
	return func(next credo.Handler) credo.Handler {
		return func(ctx *credo.Context) error {
			if config.Skipper(ctx) {
				return next(ctx)
			}

			req := ctx.Request().Request
			key, err := config.KeyFunc(ctx)
			if err != nil {
				return config.InternalErrorHandler(ctx, fmt.Errorf("rate limit key: %w", err))
			}

			limit, remaining, reset, ok, err := config.Store.Take(req.Context(), key)
			if err != nil {
				return config.InternalErrorHandler(ctx, fmt.Errorf("rate limit take: %w", err))
			}

			if reset > maxUnixNano {
				reset = maxUnixNano
			}
			resetTime := time.Unix(0, int64(reset)).UTC()
			headers := ctx.Response().Header()
			headers.Set(headerRateLimitLimit, strconv.FormatUint(limit, 10))
			headers.Set(headerRateLimitRemaining, strconv.FormatUint(remaining, 10))
			resetStr := resetTime.Format(time.RFC1123)
			headers.Set(headerRateLimitReset, resetStr)

			if !ok {
				headers.Set(headerRetryAfter, resetStr)
				if config.DeniedHandler != nil {
					return config.DeniedHandler(ctx, limit, remaining, resetTime)
				}
				return credo.NewHTTPError(http.StatusTooManyRequests)
			}

			return next(ctx)
		}
	}
}

// Close closes the internally created store if this limiter owns it.
//
// Custom stores passed via RateLimitConfig.Store are not closed automatically.
func (r *RateLimiter) Close(ctx context.Context) error {
	if !r.ownsStore || r.config.Store == nil {
		return nil
	}

	var closeErr error
	r.closeOnce.Do(func() {
		closeErr = r.config.Store.Close(ctx)
	})

	return closeErr
}

// Shutdown implements credo.Shutdowner.
func (r *RateLimiter) Shutdown(ctx context.Context) error {
	return r.Close(ctx)
}

// RateLimit returns rate-limiting middleware.
//
// This convenience constructor owns its default in-memory store for the
// lifetime of the middleware. That store runs no background goroutines —
// expired buckets are swept inline during Take — so there is nothing that
// must be stopped at shutdown; closing it would only release memory early.
// For deterministic store release (short-lived middleware, tests) or a
// custom store lifecycle, use [NewRateLimiter] and register
// [RateLimiter.Shutdown] with app.OnShutdown.
func RateLimit(cfg ...RateLimitConfig) credo.Middleware {
	config := resolveConfig(cfg, DefaultRateLimitConfig(), normalizeRateLimitConfig)
	if config.Store == nil {
		config.Store = newInMemoryRateLimitStore(config.Tokens, config.Interval)
	}
	return buildRateLimitMiddleware(config)
}

func normalizeRateLimitConfig(config RateLimitConfig) RateLimitConfig {
	defaults := DefaultRateLimitConfig()
	if config.Skipper == nil {
		config.Skipper = defaults.Skipper
	}
	if config.Tokens == 0 {
		config.Tokens = defaults.Tokens
	}
	if config.Interval <= 0 {
		config.Interval = defaults.Interval
	}
	if config.KeyFunc == nil {
		config.KeyFunc = rateLimitKeyFromContext
	}
	if config.InternalErrorHandler == nil {
		config.InternalErrorHandler = defaultRateLimitErrorHandler
	}

	return config
}

func defaultRateLimitErrorHandler(_ *credo.Context, err error) error {
	return credo.ErrInternalServerError.WithInternal(err)
}

func rateLimitKeyFromContext(ctx *credo.Context) (string, error) {
	if ctx != nil && ctx.Request() != nil {
		if value := ctx.Request().RealIP(); value != "" {
			return value, nil
		}
	}
	return "", errors.New("remote address is empty")
}
