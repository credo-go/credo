package ratelimit

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/sethvargo/go-limiter"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/middleware"
)

const (
	headerRateLimitLimit     = "X-RateLimit-Limit"
	headerRateLimitRemaining = "X-RateLimit-Remaining"
	headerRateLimitReset     = "X-RateLimit-Reset"
	headerRetryAfter         = "Retry-After"
	maxUnixNano              = uint64(1<<63 - 1)
)

// Config defines configuration for the rate-limiting [Middleware].
type Config struct {
	// Skipper defines a function to skip middleware.
	Skipper middleware.Skipper

	// Store is the backing limiter store. A store passed in stays its
	// owner's: the middleware never closes it.
	// Default: an in-memory store with Tokens/Interval, created by each
	// Middleware call.
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

// DefaultConfig returns the default rate-limiting [Middleware] config.
// Each call returns a fresh value, so callers cannot mutate the
// package-wide defaults.
func DefaultConfig() Config {
	return Config{
		Skipper:  middleware.DefaultSkipper,
		Tokens:   60,
		Interval: time.Minute,
	}
}

// Middleware returns token-bucket rate-limiting middleware, keyed by client IP
// unless Config.KeyFunc says otherwise.
//
// The returned middleware holds one store, shared by every route and group it
// is attached to: one Middleware value attached to two routes spends one
// budget per key, while two Middleware calls keep two budgets. Without
// Config.Store, the middleware creates an in-memory store that runs no
// goroutine — expired buckets are swept inline during Take — so it holds
// nothing the App must release, and is not a component: its memory goes with
// the middleware. A store passed in through Config.Store stays its owner's to
// close.
func Middleware(cfg ...Config) credo.Middleware {
	config := DefaultConfig()
	if len(cfg) > 0 {
		config = cfg[0]
	}
	config = normalizeConfig(config)
	if config.Store == nil {
		config.Store = newInMemoryRateLimitStore(config.Tokens, config.Interval)
	}
	return buildMiddleware(config)
}

func buildMiddleware(config Config) credo.Middleware {
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

			reset = min(reset, maxUnixNano)
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

func normalizeConfig(config Config) Config {
	defaults := DefaultConfig()
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
		config.KeyFunc = keyFromContext
	}
	if config.InternalErrorHandler == nil {
		config.InternalErrorHandler = defaultErrorHandler
	}

	return config
}

func defaultErrorHandler(_ *credo.Context, err error) error {
	return credo.ErrInternalServerError.WithInternal(err)
}

func keyFromContext(ctx *credo.Context) (string, error) {
	if ctx != nil && ctx.Request() != nil {
		if value := ctx.Request().RealIP(); value != "" {
			return value, nil
		}
	}
	return "", errors.New("remote address is empty")
}
