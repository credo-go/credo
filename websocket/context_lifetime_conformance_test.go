package websocket

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/store"
)

type conformanceContextKey struct{}

type conformancePrincipal struct {
	ID string
}

func TestConnectionContextContractDetachesCancellationAndPreservesValues(t *testing.T) {
	root := context.WithValue(t.Context(), conformanceContextKey{}, "custom-value")
	root = store.WithTx[string](root, "tx-42")
	requestCtx, cancelRequest := context.WithDeadline(root, time.Now().Add(time.Hour))
	defer cancelRequest()

	req := httptest.NewRequest("GET", "http://example.com/ws", nil).WithContext(requestCtx)
	requestContext := credo.NewContext(httptest.NewRecorder(), req)
	wantUser := conformancePrincipal{ID: "user-7"}
	requestContext.SetUser(wantUser)

	connectionCtx, cancelConnection := context.WithCancelCause(context.WithoutCancel(requestContext.Context()))
	t.Cleanup(func() { cancelConnection(context.Canceled) })

	if _, ok := connectionCtx.Deadline(); ok {
		t.Fatal("connection context retained request deadline")
	}
	if got := connectionCtx.Value(conformanceContextKey{}); got != "custom-value" {
		t.Fatalf("custom value = %v, want custom-value", got)
	}
	if got, ok := store.GetTx[string](connectionCtx); !ok || got != "tx-42" {
		t.Fatalf("store transaction = %q, %v; want tx-42, true", got, ok)
	}

	probeRequest := req.Clone(connectionCtx)
	probeContext := credo.NewContext(httptest.NewRecorder(), probeRequest)
	if got, ok := probeContext.GetUser[conformancePrincipal](); !ok || got != wantUser {
		t.Fatalf("typed user = %+v, %v; want %+v, true", got, ok, wantUser)
	}

	cancelRequest()
	if err := requestCtx.Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("request context error = %v, want canceled", err)
	}
	select {
	case <-connectionCtx.Done():
		t.Fatalf("request cancellation propagated to connection: %v", context.Cause(connectionCtx))
	default:
	}
}

func TestConnectionContextContractFirstCauseWins(t *testing.T) {
	t.Parallel()

	first := errors.New("first terminal cause")
	ctx, cancel := context.WithCancelCause(context.WithoutCancel(t.Context()))
	cancel(first)
	cancel(errors.New("later cleanup cause"))

	if got := context.Cause(ctx); !errors.Is(got, first) {
		t.Fatalf("connection cause = %v, want %v", got, first)
	}
}

type conformanceRawConfig struct{}

func (conformanceRawConfig) Unmarshal(string, any) error { return nil }
func (conformanceRawConfig) Exists(string) bool          { return false }

type conformanceCapturedLog struct {
	message string
	attrs   []slog.Attr
}

type conformanceLogCapture struct {
	mu      sync.Mutex
	entries []conformanceCapturedLog
}

func (c *conformanceLogCapture) append(entry conformanceCapturedLog) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = append(c.entries, entry)
}

func (c *conformanceLogCapture) find(message string) (conformanceCapturedLog, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, entry := range c.entries {
		if entry.message == message {
			return entry, true
		}
	}
	return conformanceCapturedLog{}, false
}

type conformanceLogHandler struct {
	capture *conformanceLogCapture
	attrs   []slog.Attr
}

func (h *conformanceLogHandler) Enabled(context.Context, slog.Level) bool {
	return true
}

func (h *conformanceLogHandler) Handle(_ context.Context, record slog.Record) error {
	attrs := slices.Clone(h.attrs)
	record.Attrs(func(attr slog.Attr) bool {
		attrs = append(attrs, attr)
		return true
	})
	h.capture.append(conformanceCapturedLog{message: record.Message, attrs: attrs})
	return nil
}

func (h *conformanceLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clone := *h
	clone.attrs = append(slices.Clone(h.attrs), attrs...)
	return &clone
}

func (h *conformanceLogHandler) WithGroup(string) slog.Handler {
	return h
}

func TestContextLifetimeContractSnapshotsLoggerAcrossRequests(t *testing.T) {
	capture := &conformanceLogCapture{}
	logger := slog.New(&conformanceLogHandler{capture: capture})
	app, err := credo.New(
		credo.WithRawConfig(conformanceRawConfig{}),
		credo.WithLogger(logger),
	)
	if err != nil {
		t.Fatalf("credo.New() error = %v", err)
	}

	var loggerSnapshot *slog.Logger
	var requestIDSnapshot string
	var nextRequestID string

	app.GET("/ws", func(ctx *credo.Context) error {
		if loggerSnapshot == nil {
			ctx.AddLogAttrs("tenant_id", "tenant-one", "user_id", "user-7")
			requestIDSnapshot = ctx.RequestID()
			loggerSnapshot = ctx.Logger().With("connection_id", "conn-one")
			return ctx.Response().NoContent(http.StatusNoContent)
		}

		nextRequestID = ctx.RequestID()
		ctx.Logger().InfoContext(ctx.Context(), "next-handler")
		return ctx.Response().NoContent(http.StatusNoContent)
	})

	for _, requestID := range []string{"request-0", "request-1"} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/ws", nil)
		r.Header.Set("X-Request-Id", requestID)
		app.ServeHTTP(w, r)
	}
	if requestIDSnapshot != "request-0" {
		t.Fatalf("request ID snapshot = %q, want request-0", requestIDSnapshot)
	}
	if nextRequestID != "request-1" {
		t.Fatalf("next request ID = %q, want request-1", nextRequestID)
	}

	loggerSnapshot.InfoContext(t.Context(), "connection-finished")

	connectionLog, ok := capture.find("connection-finished")
	if !ok {
		t.Fatal("connection-finished log was not captured")
	}
	assertConformanceLogAttrs(t, connectionLog, map[string]string{
		"request_id":    "request-0",
		"tenant_id":     "tenant-one",
		"user_id":       "user-7",
		"connection_id": "conn-one",
	})

	nextLog, ok := capture.find("next-handler")
	if !ok {
		t.Fatal("next-handler log was not captured")
	}
	assertConformanceLogAttrs(t, nextLog, map[string]string{
		"request_id": nextRequestID,
	})
	for _, attr := range nextLog.attrs {
		if attr.Key == "tenant_id" || attr.Key == "user_id" || attr.Key == "connection_id" {
			t.Fatalf("request logger leaked stale attribute %q", attr.Key)
		}
	}
}

func assertConformanceLogAttrs(t *testing.T, entry conformanceCapturedLog, want map[string]string) {
	t.Helper()

	counts := make(map[string]int)
	values := make(map[string]string)
	for _, attr := range entry.attrs {
		counts[attr.Key]++
		if attr.Value.Kind() == slog.KindString {
			values[attr.Key] = attr.Value.String()
		}
	}
	for key, wantValue := range want {
		if counts[key] != 1 {
			t.Fatalf("log attribute %q count = %d, want 1", key, counts[key])
		}
		if values[key] != wantValue {
			t.Fatalf("log attribute %q = %q, want %q", key, values[key], wantValue)
		}
	}
}
