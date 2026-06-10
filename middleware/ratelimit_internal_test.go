package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/credo-go/credo"
	"github.com/sethvargo/go-limiter"
)

type stubLimiterStore struct {
	closeCalls int
}

func (s *stubLimiterStore) Take(context.Context, string) (uint64, uint64, uint64, bool, error) {
	return 1, 0, uint64(time.Now().UTC().UnixNano()), true, nil
}

func (s *stubLimiterStore) Get(context.Context, string) (uint64, uint64, error) {
	return 1, 1, nil
}

func (s *stubLimiterStore) Set(context.Context, string, uint64, time.Duration) error {
	return nil
}

func (s *stubLimiterStore) Burst(context.Context, string, uint64) error {
	return nil
}

func (s *stubLimiterStore) Close(context.Context) error {
	s.closeCalls++
	return nil
}

func TestRateLimitKeyFromContext(t *testing.T) {
	tests := []struct {
		name string
		req  *http.Request
		want string
		err  bool
	}{
		{
			name: "forwarded headers ignored without trusted proxy config",
			req: func() *http.Request {
				r := httptestRequest("10.0.0.1:1000")
				r.Header.Set("X-Forwarded-For", "203.0.113.10")
				return r
			}(),
			want: "10.0.0.1",
		},
		{
			name: "remote host port",
			req:  httptestRequest("192.0.2.1:9000"),
			want: "192.0.2.1",
		},
		{
			name: "raw remote addr",
			req:  httptestRequest("client-name"),
			want: "client-name",
		},
		{
			name: "empty remote addr",
			req:  httptestRequest(""),
			err:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := credo.NewContext(httptest.NewRecorder(), tt.req)
			got, err := rateLimitKeyFromContext(ctx)
			if tt.err {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("key = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestInMemoryRateLimitStore_TakeResetAndSweep(t *testing.T) {
	store := newInMemoryRateLimitStore(1, 2*time.Millisecond).(*inMemoryRateLimitStore)
	ctx := context.Background()

	limit, remaining, _, ok, err := store.Take(ctx, "old")
	if err != nil {
		t.Fatalf("take first: %v", err)
	}
	if !ok || limit != 1 || remaining != 0 {
		t.Fatalf("first take = (limit=%d remaining=%d ok=%v), want (1,0,true)", limit, remaining, ok)
	}

	_, _, _, ok, err = store.Take(ctx, "old")
	if err != nil {
		t.Fatalf("take second: %v", err)
	}
	if ok {
		t.Fatal("expected second take to be denied")
	}

	time.Sleep(3 * time.Millisecond)
	_, remaining, _, ok, err = store.Take(ctx, "old")
	if err != nil {
		t.Fatalf("take after reset: %v", err)
	}
	if !ok || remaining != 0 {
		t.Fatalf("take after reset = (remaining=%d ok=%v), want (0,true)", remaining, ok)
	}

	time.Sleep(5 * time.Millisecond)

	// Directly trigger sweep on the shard containing "old" to verify
	// stale bucket cleanup (sharded store sweeps per-shard).
	sh := store.shard("old")
	sh.mu.Lock()
	sweepShardLocked(sh, time.Now().UTC())
	_, exists := sh.buckets["old"]
	sh.mu.Unlock()
	if exists {
		t.Fatal("expected stale bucket to be swept")
	}
}

func TestInMemoryRateLimitStore_SetBurstGetCloseAndContext(t *testing.T) {
	store := newInMemoryRateLimitStore(1, time.Second).(*inMemoryRateLimitStore)
	bg := context.Background()

	if err := store.Set(bg, "k", 2, time.Second); err != nil {
		t.Fatalf("set: %v", err)
	}

	limit, remaining, err := store.Get(bg, "k")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if limit != 2 || remaining != 2 {
		t.Fatalf("get = (limit=%d remaining=%d), want (2,2)", limit, remaining)
	}

	if _, _, _, ok, err := store.Take(bg, "k"); err != nil || !ok {
		t.Fatalf("take after set = (ok=%v err=%v), want (true,nil)", ok, err)
	}

	if err := store.Burst(bg, "k", 3); err != nil {
		t.Fatalf("burst: %v", err)
	}

	_, remaining, err = store.Get(bg, "k")
	if err != nil {
		t.Fatalf("get after burst: %v", err)
	}
	if remaining != 4 {
		t.Fatalf("remaining after burst = %d, want 4", remaining)
	}

	ctx, cancel := context.WithCancel(bg)
	cancel()

	if _, _, _, _, err := store.Take(ctx, "k"); !errors.Is(err, context.Canceled) {
		t.Fatalf("take canceled error = %v, want context.Canceled", err)
	}
	if _, _, err := store.Get(ctx, "k"); !errors.Is(err, context.Canceled) {
		t.Fatalf("get canceled error = %v, want context.Canceled", err)
	}
	if err := store.Set(ctx, "k", 1, time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("set canceled error = %v, want context.Canceled", err)
	}
	if err := store.Burst(ctx, "k", 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("burst canceled error = %v, want context.Canceled", err)
	}
	if err := store.Close(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("close canceled error = %v, want context.Canceled", err)
	}

	if err := store.Close(bg); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := store.Close(bg); err != nil {
		t.Fatalf("close second: %v", err)
	}

	if _, _, _, _, err := store.Take(bg, "k"); !errors.Is(err, limiter.ErrStopped) {
		t.Fatalf("take after close error = %v, want limiter.ErrStopped", err)
	}
}

func TestRateLimiter_CloseAndShutdown(t *testing.T) {
	r := NewRateLimiter(RateLimitConfig{})

	if err := r.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := r.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func TestRateLimiter_Close_DoesNotCloseCustomStore(t *testing.T) {
	store := &stubLimiterStore{}
	r := NewRateLimiter(RateLimitConfig{Store: store})

	if err := r.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}

	if store.closeCalls != 0 {
		t.Fatalf("custom store close calls = %d, want 0", store.closeCalls)
	}
}

func httptestRequest(remoteAddr string) *http.Request {
	req, _ := http.NewRequest(http.MethodGet, "http://example.com", nil)
	req.RemoteAddr = remoteAddr
	return req
}
