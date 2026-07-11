package credo_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/store"
)

type healthStoreProbe struct {
	delay   time.Duration
	release <-chan struct{}
	calls   *atomic.Int32
	health  store.Health
}

func (*healthStoreProbe) Ping(context.Context) error     { return nil }
func (*healthStoreProbe) Shutdown(context.Context) error { return nil }
func (p *healthStoreProbe) Health(context.Context) store.Health {
	if p.calls != nil {
		p.calls.Add(1)
	}
	if p.release != nil {
		<-p.release
	}
	if p.delay > 0 {
		time.Sleep(p.delay)
	}
	if p.health.Status != "" || p.health.Cause != nil || p.health.Details != nil {
		result := p.health.Clone()
		if result.Latency == 0 {
			result.Latency = p.delay
		}
		return result
	}
	return store.Health{Status: store.StatusUp, Latency: p.delay}
}

type firstHealthStore struct{ *healthStoreProbe }
type secondHealthStore struct{ *healthStoreProbe }
type hangingHealthStore struct{ *healthStoreProbe }
type diagnosticHealthStore struct{ *healthStoreProbe }

func quietHealthApp(t *testing.T) *credo.App {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return mustNew(t, credo.WithLogger(logger), credo.WithoutRequestID())
}

func TestReadiness_RegisteredStoresRunInParallel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		app := quietHealthApp(t)
		if err := store.Register[*firstHealthStore](app, &firstHealthStore{
			healthStoreProbe: &healthStoreProbe{delay: 5 * time.Second},
		}, store.WithName("first")); err != nil {
			t.Fatalf("register first: %v", err)
		}
		if err := store.Register[*secondHealthStore](app, &secondHealthStore{
			healthStoreProbe: &healthStoreProbe{delay: 3 * time.Second},
		}, store.WithName("second")); err != nil {
			t.Fatalf("register second: %v", err)
		}
		app.UseHealth(credo.HealthConfig{CheckTimeout: 10 * time.Second})

		start := time.Now()
		w := httptest.NewRecorder()
		app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ready", nil))
		if elapsed := time.Since(start); elapsed != 5*time.Second {
			t.Fatalf("elapsed = %v, want 5s (maximum store duration)", elapsed)
		}
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
		}
	})
}

func TestReadiness_RepeatedRequestsReuseHungStoreFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		var releaseOnce sync.Once
		t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
		var calls atomic.Int32

		app := quietHealthApp(t)
		if err := store.Register[*hangingHealthStore](app, &hangingHealthStore{
			healthStoreProbe: &healthStoreProbe{release: release, calls: &calls},
		}, store.WithName("hung")); err != nil {
			t.Fatalf("register hung store: %v", err)
		}
		app.UseHealth(credo.HealthConfig{CheckTimeout: time.Second})

		for request := range 32 {
			w := httptest.NewRecorder()
			app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ready", nil))
			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("request %d status = %d, want 503", request, w.Code)
			}
		}
		if got := calls.Load(); got != 1 {
			t.Fatalf("Health invocation count = %d, want 1 shared hung flight", got)
		}

		releaseOnce.Do(func() { close(release) })
		synctest.Wait()

		w := httptest.NewRecorder()
		app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ready", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("status after release = %d, want 200; body: %s", w.Code, w.Body.String())
		}
		if got := calls.Load(); got != 2 {
			t.Fatalf("Health invocation count after release = %d, want 2", got)
		}
	})
}

func TestReadiness_RegisteredStoreCauseContract(t *testing.T) {
	const typedSecret = "dial tcp 10.0.1.5:5432: connection refused"
	const forgedDetail = "forged details error must not be promoted"
	tests := []struct {
		name             string
		health           store.Health
		exposeErrors     bool
		wantTypedInBody  bool
		wantTypedInLog   bool
		wantForgedInBody bool
		wantForgedInLog  bool
	}{
		{
			name: "typed cause is logged and opt-in exposed",
			health: store.Health{
				Status:  store.StatusDown,
				Cause:   errors.New(typedSecret),
				Details: map[string]any{"error": forgedDetail},
			},
			exposeErrors:    true,
			wantTypedInBody: true,
			wantTypedInLog:  true,
		},
		{
			name: "details error is never promoted",
			health: store.Health{
				Status:  store.StatusDown,
				Details: map[string]any{"error": forgedDetail},
			},
			exposeErrors: true,
		},
		{
			name: "up with cause fails closed",
			health: store.Health{
				Status: store.StatusUp,
				Cause:  errors.New(typedSecret),
			},
			wantTypedInLog: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger, logs := newTestLogger(t)
			app := mustNew(t, credo.WithLogger(logger), credo.WithoutRequestID())
			if err := store.Register[*diagnosticHealthStore](app, &diagnosticHealthStore{
				healthStoreProbe: &healthStoreProbe{health: tt.health},
			}, store.WithName("diagnostic")); err != nil {
				t.Fatalf("register diagnostic store: %v", err)
			}
			app.UseHealth(credo.HealthConfig{ExposeErrors: tt.exposeErrors})

			w := httptest.NewRecorder()
			app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ready", nil))
			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503; body: %s", w.Code, w.Body.String())
			}

			body := w.Body.String()
			logText := logs.String()
			if got := strings.Contains(body, typedSecret); got != tt.wantTypedInBody {
				t.Errorf("body contains typed cause = %v, want %v; body: %s", got, tt.wantTypedInBody, body)
			}
			if got := strings.Contains(logText, typedSecret); got != tt.wantTypedInLog {
				t.Errorf("log contains typed cause = %v, want %v; log: %s", got, tt.wantTypedInLog, logText)
			}
			if got := strings.Contains(body, forgedDetail); got != tt.wantForgedInBody {
				t.Errorf("body contains forged Details error = %v, want %v; body: %s", got, tt.wantForgedInBody, body)
			}
			if got := strings.Contains(logText, forgedDetail); got != tt.wantForgedInLog {
				t.Errorf("log contains forged Details error = %v, want %v; log: %s", got, tt.wantForgedInLog, logText)
			}
		})
	}
}
