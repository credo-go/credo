package httpclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"time"
)

// RetryConfig configures the retry transport (see [WithRetry] and
// [NewRetryTransport]). Zero-valued fields fall back to the corresponding
// [DefaultRetryConfig] values.
type RetryConfig struct {
	// MaxAttempts is the total number of attempts including the first.
	// Default 3 (one initial attempt plus two retries).
	MaxAttempts int

	// MinDelay is the backoff ceiling for the first retry. Default 100ms.
	MinDelay time.Duration

	// MaxDelay caps the backoff ceiling. Default 2s.
	MaxDelay time.Duration

	// RetryIf reports whether a request should be retried after the given
	// response or transport error (exactly one of resp/err is non-nil).
	// It receives the caller's original request — implementations must not
	// read its Body, which has already been consumed by the first attempt.
	// Default [DefaultRetryIf].
	RetryIf func(req *http.Request, resp *http.Response, err error) bool
}

// DefaultRetryConfig returns the configuration used by [WithRetry] and
// [NewRetryTransport] when no config is supplied. Each call returns a
// fresh value, so callers cannot mutate the package-wide defaults.
func DefaultRetryConfig() RetryConfig {
	return RetryConfig{
		MaxAttempts: 3,
		MinDelay:    100 * time.Millisecond,
		MaxDelay:    2 * time.Second,
		RetryIf:     DefaultRetryIf,
	}
}

// DefaultRetryIf reports whether a request should be retried:
// idempotent method AND (transport error OR 5xx response), never after
// context cancellation or deadline expiry.
//
// Idempotent methods are GET, HEAD, OPTIONS, TRACE, PUT, and DELETE.
// Anything else — POST in particular — is never retried by default:
// retrying a non-idempotent request can duplicate side effects (double
// payments). Callers with idempotency keys can opt in via
// [RetryConfig.RetryIf]. A 429 response is not retried either; override
// RetryIf to change that.
func DefaultRetryIf(req *http.Request, resp *http.Response, err error) bool {
	if err != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		return false
	}
	switch req.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions,
		http.MethodTrace, http.MethodPut, http.MethodDelete:
	default:
		return false
	}
	if err != nil {
		return true
	}
	return resp != nil && resp.StatusCode >= 500
}

// NewRetryTransport wraps base with retry-with-backoff behavior. Zero args
// use [DefaultRetryConfig]; zero-valued config fields fall back to their
// defaults.
//
// Backoff between attempts is full-jitter exponential: a uniformly random
// duration in [0, min(MaxDelay, MinDelay·2^(attempt-1))). Waits abort
// immediately when the request context is done, returning the context's
// error.
//
// A request with a body is retried only when req.GetBody is set (the stdlib
// sets it automatically for *bytes.Buffer, *bytes.Reader, and
// *strings.Reader bodies). Without GetBody, the first response or error is
// returned as-is — a request is never silently re-sent with a half-consumed
// body.
//
// When attempts are exhausted, the last response (or error) is returned
// unchanged: a final 503 arrives as (resp, nil), exactly like the stdlib.
// The response body of a discarded attempt is drained (up to a small cap)
// and closed so the underlying connection can be reused.
//
// A nil base defaults to [http.DefaultTransport].
func NewRetryTransport(base http.RoundTripper, cfg ...RetryConfig) http.RoundTripper {
	config := DefaultRetryConfig()
	if len(cfg) > 0 {
		config = cfg[0]
	}
	config = normalizeRetryConfig(config)
	if base == nil {
		base = http.DefaultTransport
	}
	return &retryTransport{base: base, cfg: config}
}

func normalizeRetryConfig(config RetryConfig) RetryConfig {
	defaults := DefaultRetryConfig()
	if config.MaxAttempts <= 0 {
		config.MaxAttempts = defaults.MaxAttempts
	}
	if config.MinDelay <= 0 {
		config.MinDelay = defaults.MinDelay
	}
	if config.MaxDelay <= 0 {
		config.MaxDelay = defaults.MaxDelay
	}
	if config.RetryIf == nil {
		config.RetryIf = DefaultRetryIf
	}
	return config
}

type retryTransport struct {
	base http.RoundTripper
	cfg  RetryConfig
}

// attemptKey is the context key under which the retry transport records the
// current attempt number, so the logging transport can report it without
// coupling the two.
type attemptKey struct{}

func withAttempt(ctx context.Context, n int) context.Context {
	return context.WithValue(ctx, attemptKey{}, n)
}

// attemptFromContext returns the attempt number recorded by the retry
// transport, reporting whether one is present.
func attemptFromContext(ctx context.Context) (int, bool) {
	n, ok := ctx.Value(attemptKey{}).(int)
	return n, ok
}

func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	// A body without GetBody cannot be replayed — single shot, returned
	// as-is.
	replayable := req.Body == nil || req.Body == http.NoBody || req.GetBody != nil

	for attempt := 1; ; attempt++ {
		attemptReq := req.Clone(withAttempt(ctx, attempt))
		if attempt > 1 && req.GetBody != nil {
			body, err := req.GetBody()
			if err != nil {
				// The previous attempt's response is already discarded;
				// the body error is all we have.
				return nil, fmt.Errorf("httpclient: replay request body: %w", err)
			}
			attemptReq.Body = body
		}

		resp, err := t.base.RoundTrip(attemptReq)
		if !replayable || attempt >= t.cfg.MaxAttempts || !t.cfg.RetryIf(req, resp, err) {
			return resp, err
		}

		// This attempt is discarded — drain and close its body so the
		// connection can be reused, then back off.
		drainAndClose(resp)
		if werr := sleepBackoff(ctx, backoffDelay(t.cfg, attempt)); werr != nil {
			return nil, werr
		}
	}
}

// backoffDelay returns the full-jitter backoff after the given failed
// attempt (1-based): a uniformly random duration in
// [0, min(MaxDelay, MinDelay·2^(attempt-1))).
func backoffDelay(cfg RetryConfig, attempt int) time.Duration {
	ceiling := cfg.MinDelay
	for i := 1; i < attempt; i++ {
		ceiling *= 2
		// The overflow check (<= 0) guards very high attempt counts from
		// wrapping the shift negative.
		if ceiling >= cfg.MaxDelay || ceiling <= 0 {
			ceiling = cfg.MaxDelay
			break
		}
	}
	return rand.N(ceiling) //nolint:gosec // Retry jitter is not security-sensitive randomness.
}

// sleepBackoff waits for the given delay, aborting immediately with the
// context's error when ctx is done.
func sleepBackoff(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// drainLimit caps how much of a discarded response body is read before
// closing. Beyond the cap the connection is dropped instead of reused —
// cheaper than slurping an arbitrarily large body.
const drainLimit = 4 << 10

func drainAndClose(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, drainLimit))
	_ = resp.Body.Close()
}
