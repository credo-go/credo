package websocket

import (
	"context"
	"errors"
	"log/slog"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	coderwebsocket "github.com/coder/websocket"

	"github.com/credo-go/credo"
)

// moduleLogApp builds an App that logs to appLogger, with request IDs and a
// middleware that enriches the request's logger, and serves one echo route
// on a server built with cfg.
func moduleLogApp(t *testing.T, appLogger *slog.Logger, cfg Config) string {
	t.Helper()
	app, err := credo.New(credo.WithLogger(appLogger), credo.WithRawConfig(conformanceRawConfig{}))
	if err != nil {
		t.Fatal(err)
	}
	app.UseRequestID()
	app.PreRouting(func(next credo.Handler) credo.Handler {
		return func(ctx *credo.Context) error {
			ctx.AddLogAttrs(slog.String("tenant", "acme"))
			return next(ctx)
		}
	})
	server := useTestServer(app, cfg)
	app.GET("/events", server.Handler(func(_ *credo.Context, conn *Conn) error {
		_, _, readErr := conn.Read(conn.Context())
		return readErr
	}))
	startTestApp(t, app)
	httpServer := httptest.NewServer(app)
	t.Cleanup(httpServer.Close)
	return "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/events"
}

func dialAndClose(t *testing.T, url string) {
	t.Helper()
	client, err := dialHandlerTest(t.Context(), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Close(coderwebsocket.StatusNormalClosure, ""); err != nil {
		t.Fatal(err)
	}
}

// TestModuleLogger_RequestLoggerByDefault: without Config.Logger a
// connection log goes to the request's logger, with connection_id,
// module=websocket, request_id and the application's enrichment.
func TestModuleLogger_RequestLoggerByDefault(t *testing.T) {
	capture := &conformanceLogCapture{}
	url := moduleLogApp(t, slog.New(&conformanceLogHandler{capture: capture}), Config{})
	dialAndClose(t, url)

	for _, message := range []string{"websocket: connection opened", "websocket: connection closed"} {
		entry := waitCapturedLog(t, capture, message)
		assertLogAttrCount(t, entry, "connection_id", 1)
		assertLogString(t, entry, "module", "websocket")
		assertLogAttrCount(t, entry, "request_id", 1)
		assertLogString(t, entry, "tenant", "acme")
	}
}

// TestModuleLogger_ConfigLoggerTakesEveryRecord: with Config.Logger every
// record reaches it; a connection log carries connection_id and the
// explicit request_id but not the request's enrichment, and the App's
// logger receives no websocket record.
func TestModuleLogger_ConfigLoggerTakesEveryRecord(t *testing.T) {
	appCapture := &conformanceLogCapture{}
	own := &conformanceLogCapture{}
	url := moduleLogApp(t, slog.New(&conformanceLogHandler{capture: appCapture}),
		Config{Logger: slog.New(&conformanceLogHandler{capture: own})})
	dialAndClose(t, url)

	for _, message := range []string{"websocket: connection opened", "websocket: connection closed"} {
		entry := waitCapturedLog(t, own, message)
		assertLogAttrCount(t, entry, "connection_id", 1)
		assertLogString(t, entry, "module", "websocket")
		assertLogAttrCount(t, entry, "request_id", 1)
		assertLogAttrCount(t, entry, "tenant", 0)
	}
	for _, entry := range appCapture.snapshot() {
		if strings.HasPrefix(entry.message, "websocket:") {
			t.Errorf("the App's logger received %q; with Config.Logger every record goes to it", entry.message)
		}
	}
}

// TestModuleLogger_ShutdownIncompleteCarriesNoConnection: the server-level
// record goes to the module's logger with module=websocket, without
// connection_id or request_id.
func TestModuleLogger_ShutdownIncompleteCarriesNoConnection(t *testing.T) {
	own := &conformanceLogCapture{}
	server := New(Config{Logger: slog.New(&conformanceLogHandler{capture: own})})
	if err := server.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	server.mu.Lock()
	server.activeTokens = 1 // a handler that never returns
	server.mu.Unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := server.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown() = %v, want the deadline", err)
	}
	entry := waitCapturedLog(t, own, "websocket: shutdown incomplete")
	assertLogString(t, entry, "module", "websocket")
	assertLogAttrCount(t, entry, "connection_id", 0)
	assertLogAttrCount(t, entry, "request_id", 0)
}

// TestModuleLogger_StartUsesTheAppLogger: without Config.Logger the server's
// own logger is the App's, scoped to the server's component, which Start
// receives through credo.InfraFrom.
func TestModuleLogger_StartUsesTheAppLogger(t *testing.T) {
	capture := &conformanceLogCapture{}
	app, err := credo.New(credo.WithLogger(slog.New(&conformanceLogHandler{capture: capture})),
		credo.WithRawConfig(conformanceRawConfig{}))
	if err != nil {
		t.Fatal(err)
	}
	server := useTestServer(app, Config{})
	startTestApp(t, app)
	server.mu.Lock()
	logger := server.logger
	server.mu.Unlock()
	logger.Info("probe")
	entry := waitCapturedLog(t, capture, "probe")
	assertLogString(t, entry, "module", "websocket")
	assertLogString(t, entry, "component", "*websocket.Server")
}

// routingHandler sends records whose accumulated attributes carry
// module=websocket to a separate sink: the recipe the guides print.
type routingHandler struct {
	app, ws *conformanceLogCapture
	attrs   []slog.Attr
}

func (h *routingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *routingHandler) Handle(_ context.Context, r slog.Record) error {
	sink := h.app
	attrs := slices.Clone(h.attrs)
	r.Attrs(func(a slog.Attr) bool { attrs = append(attrs, a); return true })
	for _, a := range attrs {
		if a.Key == "module" && a.Value.String() == "websocket" {
			sink = h.ws
		}
	}
	sink.append(conformanceCapturedLog{message: r.Message, level: r.Level, attrs: attrs})
	return nil
}

func (h *routingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clone := *h
	clone.attrs = append(slices.Clone(h.attrs), attrs...)
	return &clone
}

func (h *routingHandler) WithGroup(string) slog.Handler { return h }

// TestModuleLogger_RoutingHandlerSeparatesTheModule: a handler that keeps
// the attributes it receives through WithAttrs routes the module=websocket
// records to their own sink.
func TestModuleLogger_RoutingHandlerSeparatesTheModule(t *testing.T) {
	appSink, wsSink := &conformanceLogCapture{}, &conformanceLogCapture{}
	url := moduleLogApp(t, slog.New(&routingHandler{app: appSink, ws: wsSink}), Config{})
	dialAndClose(t, url)

	waitCapturedLog(t, wsSink, "websocket: connection opened")
	waitCapturedLog(t, wsSink, "websocket: connection closed")
	for _, entry := range appSink.snapshot() {
		if strings.HasPrefix(entry.message, "websocket:") {
			t.Errorf("app sink received %q", entry.message)
		}
	}
}
