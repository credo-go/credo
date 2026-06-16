package httpclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fastRetry returns a config with sub-millisecond backoff so tests stay fast.
func fastRetry(attempts int) RetryConfig {
	return RetryConfig{
		MaxAttempts: attempts,
		MinDelay:    time.Millisecond,
		MaxDelay:    2 * time.Millisecond,
	}
}

// scriptServer returns a test server that answers the i-th request with the
// i-th status (the last status repeats) and a body of "attempt-<i>".
func scriptServer(t *testing.T, statuses ...int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	hits := new(atomic.Int32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(hits.Add(1))
		idx := min(n, len(statuses)) - 1
		w.WriteHeader(statuses[idx])
		fmt.Fprintf(w, "attempt-%d", n)
	}))
	t.Cleanup(srv.Close)
	return srv, hits
}

func TestDefaultRetryIf(t *testing.T) {
	resp := func(status int) *http.Response {
		return &http.Response{StatusCode: status, Body: http.NoBody}
	}
	tests := []struct {
		name   string
		method string
		status int
		err    error
		want   bool
	}{
		{"GET transport error", http.MethodGet, 0, errors.New("connection reset"), true},
		{"GET 503", http.MethodGet, 503, nil, true},
		{"GET 500", http.MethodGet, 500, nil, true},
		{"GET 200", http.MethodGet, 200, nil, false},
		{"GET 404", http.MethodGet, 404, nil, false},
		{"GET 429 not retried", http.MethodGet, 429, nil, false},
		{"HEAD 503", http.MethodHead, 503, nil, true},
		{"OPTIONS 503", http.MethodOptions, 503, nil, true},
		{"TRACE 503", http.MethodTrace, 503, nil, true},
		{"PUT 503", http.MethodPut, 503, nil, true},
		{"DELETE transport error", http.MethodDelete, 0, errors.New("eof"), true},
		{"POST 503 not retried", http.MethodPost, 503, nil, false},
		{"POST transport error not retried", http.MethodPost, 0, errors.New("eof"), false},
		{"PATCH 503 not retried", http.MethodPatch, 503, nil, false},
		{"GET canceled never retried", http.MethodGet, 0, fmt.Errorf("do: %w", context.Canceled), false},
		{"GET deadline never retried", http.MethodGet, 0, fmt.Errorf("do: %w", context.DeadlineExceeded), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var testResp *http.Response
			if tt.status != 0 {
				testResp = resp(tt.status)
				t.Cleanup(func() { _ = testResp.Body.Close() })
			}
			req := httptest.NewRequest(tt.method, "http://example.com/", nil)
			if got := DefaultRetryIf(req, testResp, tt.err); got != tt.want {
				t.Errorf("DefaultRetryIf() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBackoffDelay_Bounds(t *testing.T) {
	cfg := RetryConfig{MinDelay: 100 * time.Millisecond, MaxDelay: 2 * time.Second}
	ceilings := []time.Duration{
		100 * time.Millisecond, // attempt 1
		200 * time.Millisecond, // attempt 2
		400 * time.Millisecond, // attempt 3
		800 * time.Millisecond, // attempt 4
		1600 * time.Millisecond,
		2 * time.Second, // capped
		2 * time.Second,
	}
	for i, ceiling := range ceilings {
		attempt := i + 1
		for range 100 {
			d := backoffDelay(cfg, attempt)
			if d < 0 || d >= ceiling {
				t.Fatalf("backoffDelay(attempt=%d) = %v, want in [0, %v)", attempt, d, ceiling)
			}
		}
	}
}

func TestBackoffDelay_ShiftOverflowGuard(t *testing.T) {
	cfg := RetryConfig{MinDelay: 100 * time.Millisecond, MaxDelay: 2 * time.Second}
	// Without the overflow guard the doubling would wrap negative and
	// rand.N would panic.
	for _, attempt := range []int{64, 70, 200} {
		d := backoffDelay(cfg, attempt)
		if d < 0 || d >= cfg.MaxDelay {
			t.Fatalf("backoffDelay(attempt=%d) = %v, want in [0, %v)", attempt, d, cfg.MaxDelay)
		}
	}
}

func TestRetry_5xxThenSuccess(t *testing.T) {
	srv, hits := scriptServer(t, http.StatusServiceUnavailable, http.StatusOK)
	client := &http.Client{Transport: NewRetryTransport(nil, fastRetry(3))}

	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if got := hits.Load(); got != 2 {
		t.Errorf("server hits = %d, want 2", got)
	}
}

func TestRetry_TransportErrorThenSuccess(t *testing.T) {
	srv, hits := scriptServer(t, http.StatusOK)
	calls := new(atomic.Int32)
	flaky := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			return nil, errors.New("connection reset by peer")
		}
		return http.DefaultTransport.RoundTrip(req)
	})
	client := &http.Client{Transport: NewRetryTransport(flaky, fastRetry(3))}

	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if got, want := calls.Load(), int32(2); got != want {
		t.Errorf("transport calls = %d, want %d", got, want)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("server hits = %d, want 1", got)
	}
}

func TestRetry_POSTNotRetriedByDefault(t *testing.T) {
	srv, hits := scriptServer(t, http.StatusServiceUnavailable)
	client := &http.Client{Transport: NewRetryTransport(nil, fastRetry(3))}

	resp, err := client.Post(srv.URL, "text/plain", strings.NewReader("hello"))
	if err != nil {
		t.Fatalf("Post() error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("server hits = %d, want 1 (POST must not be retried)", got)
	}
}

func TestRetry_RetryIfOverride_ReplaysBody(t *testing.T) {
	var (
		mu     sync.Mutex
		bodies []string
	)
	hits := new(atomic.Int32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		if hits.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	cfg := fastRetry(3)
	cfg.RetryIf = func(req *http.Request, resp *http.Response, err error) bool {
		// The caller has idempotency keys — POST retry is safe here.
		return err != nil || (resp != nil && resp.StatusCode >= 500)
	}
	client := &http.Client{Transport: NewRetryTransport(nil, cfg)}

	// strings.Reader bodies get GetBody set by http.NewRequest automatically.
	resp, err := client.Post(srv.URL, "text/plain", strings.NewReader("hello"))
	if err != nil {
		t.Fatalf("Post() error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 || bodies[0] != "hello" || bodies[1] != "hello" {
		t.Errorf("server-received bodies = %q, want [hello hello]", bodies)
	}
}

func TestRetry_NoGetBody_NoRetry(t *testing.T) {
	srv, hits := scriptServer(t, http.StatusServiceUnavailable)
	client := &http.Client{Transport: NewRetryTransport(nil, fastRetry(3))}

	// io.NopCloser is not one of the types http.NewRequest derives GetBody
	// from — the body cannot be replayed.
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, srv.URL,
		io.NopCloser(strings.NewReader("payload")))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	if req.GetBody != nil {
		t.Fatal("precondition failed: GetBody is set")
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("server hits = %d, want 1 (no GetBody → single shot)", got)
	}
}

func TestRetry_ExhaustionReturnsLastResponse(t *testing.T) {
	srv, hits := scriptServer(t, http.StatusServiceUnavailable)
	client := &http.Client{Transport: NewRetryTransport(nil, fastRetry(3))}

	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("Get() error = %v, want nil (exhaustion keeps stdlib semantics)", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
	if got := hits.Load(); got != 3 {
		t.Errorf("server hits = %d, want 3", got)
	}
	// The final attempt's body must remain readable — only discarded
	// attempts are drained.
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll(final body) error = %v", err)
	}
	if string(body) != "attempt-3" {
		t.Errorf("final body = %q, want %q", body, "attempt-3")
	}
}

func TestRetry_ContextCancelAbortsBackoff(t *testing.T) {
	srv, hits := scriptServer(t, http.StatusServiceUnavailable)
	ctx, cancel := context.WithCancel(t.Context())

	cfg := RetryConfig{
		MaxAttempts: 3,
		// Absurd delays: if the abort failed, the hits assertion below
		// would catch a completed retry instead.
		MinDelay: 30 * time.Second,
		MaxDelay: 30 * time.Second,
		RetryIf: func(req *http.Request, resp *http.Response, err error) bool {
			cancel() // cancel between the failed attempt and its backoff wait
			return true
		},
	}
	client := &http.Client{Transport: NewRetryTransport(nil, cfg)}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	start := time.Now()
	_, err = client.Do(req) //nolint:bodyclose // error path returns no body
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Do() error = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("Do() took %v, backoff wait did not abort", elapsed)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("server hits = %d, want 1", got)
	}
}

func TestRetry_ZeroConfigUsesDefaults(t *testing.T) {
	rt, ok := NewRetryTransport(nil).(*retryTransport)
	if !ok {
		t.Fatal("NewRetryTransport did not return *retryTransport")
	}
	if rt.cfg.MaxAttempts != 3 {
		t.Errorf("MaxAttempts = %d, want 3", rt.cfg.MaxAttempts)
	}
	if rt.cfg.MinDelay != 100*time.Millisecond {
		t.Errorf("MinDelay = %v, want 100ms", rt.cfg.MinDelay)
	}
	if rt.cfg.MaxDelay != 2*time.Second {
		t.Errorf("MaxDelay = %v, want 2s", rt.cfg.MaxDelay)
	}
	if rt.cfg.RetryIf == nil {
		t.Error("RetryIf = nil, want DefaultRetryIf")
	}
	if rt.base == nil {
		t.Error("base = nil, want http.DefaultTransport")
	}
}

func TestRetry_PartialConfigFillsDefaults(t *testing.T) {
	rt := NewRetryTransport(nil, RetryConfig{MaxAttempts: 5}).(*retryTransport)
	if rt.cfg.MaxAttempts != 5 {
		t.Errorf("MaxAttempts = %d, want 5", rt.cfg.MaxAttempts)
	}
	if rt.cfg.MinDelay != 100*time.Millisecond || rt.cfg.MaxDelay != 2*time.Second {
		t.Errorf("delays = %v/%v, want defaults 100ms/2s", rt.cfg.MinDelay, rt.cfg.MaxDelay)
	}
	if rt.cfg.RetryIf == nil {
		t.Error("RetryIf = nil, want DefaultRetryIf")
	}
}

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
