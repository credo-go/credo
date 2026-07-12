package websocket

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	coderwebsocket "github.com/coder/websocket"

	"github.com/credo-go/credo"
)

func TestConnectionAndAccessLogContract(t *testing.T) {
	capture := &conformanceLogCapture{}
	logger := slog.New(&conformanceLogHandler{capture: capture})
	app, err := credo.New(credo.WithLogger(logger))
	if err != nil {
		t.Fatal(err)
	}
	server := Use(app, Config{Subprotocols: []string{"events.v1"}})
	app.GET("/events", server.Handler(func(_ *credo.Context, conn *Conn) error {
		_, _, readErr := conn.Read(conn.Context())
		return readErr
	})).Name("events")
	httpServer := httptest.NewServer(app)
	defer httpServer.Close()
	client, _, err := coderwebsocket.Dial(
		t.Context(),
		"ws"+strings.TrimPrefix(httpServer.URL, "http")+"/events",
		&coderwebsocket.DialOptions{Subprotocols: []string{"events.v1"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Close(coderwebsocket.StatusNormalClosure, ""); err != nil {
		t.Fatal(err)
	}

	access := waitCapturedLog(t, capture, "request completed")
	if access.level != slog.LevelInfo {
		t.Errorf("access level = %v, want INFO", access.level)
	}
	assertLogInt64(t, access, "status", http.StatusSwitchingProtocols)
	assertLogInt64(t, access, "bytes", 0)
	assertLogAttrCount(t, access, "duration", 1)
	assertLogAttrCount(t, access, "request_id", 1)

	opened := waitCapturedLog(t, capture, "websocket: connection opened")
	if opened.level != slog.LevelDebug {
		t.Errorf("open level = %v, want DEBUG", opened.level)
	}
	assertLogString(t, opened, "classification", "open")
	assertLogString(t, opened, "route", "events")
	assertLogString(t, opened, "subprotocol", "events.v1")
	assertLogAttrCount(t, opened, "connection_id", 1)
	assertLogAttrCount(t, opened, "request_id", 1)

	closed := waitCapturedLog(t, capture, "websocket: connection closed")
	if closed.level != slog.LevelDebug {
		t.Errorf("normal close level = %v, want DEBUG", closed.level)
	}
	assertLogString(t, closed, "classification", "peer_normal")
	assertLogInt64(t, closed, "close_code", int64(StatusNormalClosure))
	assertLogPositiveDuration(t, closed, "lifetime")
	assertLogAttrCount(t, closed, "connection_id", 1)
	assertLogAttrCount(t, closed, "request_id", 1)

	var packageInfo int
	for _, entry := range capture.snapshot() {
		if strings.HasPrefix(entry.message, "websocket:") && entry.level == slog.LevelInfo {
			packageInfo++
		}
	}
	if packageInfo != 0 {
		t.Errorf("normal connection emitted %d package Info logs, want 0", packageInfo)
	}
}

func TestConnectionFailureLogsAreStructuredAndSecretSafe(t *testing.T) {
	for _, tc := range []struct {
		name           string
		classification string
		handler        Handler
	}{
		{
			name:           "application error",
			classification: "application_error",
			handler: func(_ *credo.Context, conn *Conn) error {
				_, _, err := conn.Read(conn.Context())
				if err != nil {
					return err
				}
				return errors.New("application-secret")
			},
		},
		{
			name:           "panic",
			classification: "panic",
			handler: func(*credo.Context, *Conn) error {
				panic("panic-secret")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capture := &conformanceLogCapture{}
			logger := slog.New(&conformanceLogHandler{capture: capture})
			app, err := credo.New(credo.WithLogger(logger), credo.WithoutAccessLog())
			if err != nil {
				t.Fatal(err)
			}
			server := Use(app, Config{AllowedOrigins: []string{"https://allowed.example"}})
			app.GET("/ws", server.Handler(tc.handler)).Name("secret-route")
			httpServer := httptest.NewServer(app)
			defer httpServer.Close()
			headers := make(http.Header)
			headers.Set("Origin", "https://allowed.example")
			headers.Set("Authorization", "Bearer authorization-secret")
			headers.Set("Cookie", "session=cookie-secret")
			client, _, err := coderwebsocket.Dial(
				t.Context(),
				"ws"+strings.TrimPrefix(httpServer.URL, "http")+"/ws?token=query-secret",
				&coderwebsocket.DialOptions{HTTPHeader: headers},
			)
			if err != nil {
				t.Fatal(err)
			}
			defer client.CloseNow()
			if tc.classification == "application_error" {
				if err := client.Write(
					t.Context(), coderwebsocket.MessageText, []byte("payload-secret"),
				); err != nil {
					t.Fatal(err)
				}
			}
			_, _, err = client.Read(t.Context())
			if got := coderwebsocket.CloseStatus(err); got != coderwebsocket.StatusInternalError {
				t.Fatalf("close status = %d, want 1011; error=%v", got, err)
			}

			entry := waitCapturedLog(t, capture, "websocket: connection failed")
			if entry.level != slog.LevelError {
				t.Errorf("failure level = %v, want ERROR", entry.level)
			}
			assertLogString(t, entry, "classification", tc.classification)
			assertLogString(t, entry, "route", "secret-route")
			assertLogInt64(t, entry, "close_code", int64(StatusInternalError))
			assertLogAttrCount(t, entry, "connection_id", 1)
			assertLogAttrCount(t, entry, "request_id", 1)

			serialized := capturedLogsText(capture.snapshot())
			for _, secret := range []string{
				"application-secret", "panic-secret", "authorization-secret",
				"cookie-secret", "query-secret", "payload-secret", "https://allowed.example",
			} {
				if strings.Contains(serialized, secret) {
					t.Errorf("logs leaked %q: %s", secret, serialized)
				}
			}
		})
	}
}

func TestReadLimitLogsWarnWithoutPayload(t *testing.T) {
	capture := &conformanceLogCapture{}
	logger := slog.New(&conformanceLogHandler{capture: capture})
	app, err := credo.New(credo.WithLogger(logger), credo.WithoutAccessLog())
	if err != nil {
		t.Fatal(err)
	}
	server := Use(app, Config{ReadLimit: 8})
	app.GET("/ws", server.Handler(func(_ *credo.Context, conn *Conn) error {
		_, _, readErr := conn.Read(conn.Context())
		return readErr
	}))
	httpServer := httptest.NewServer(app)
	defer httpServer.Close()
	client, _, err := coderwebsocket.Dial(
		t.Context(), "ws"+strings.TrimPrefix(httpServer.URL, "http")+"/ws", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseNow()
	if err := client.Write(
		t.Context(), coderwebsocket.MessageText, []byte("payload-secret-too-large"),
	); err != nil {
		t.Fatal(err)
	}
	_, _, err = client.Read(t.Context())
	if got := coderwebsocket.CloseStatus(err); got != coderwebsocket.StatusMessageTooBig {
		t.Fatalf("close status = %d, want 1009; error=%v", got, err)
	}
	entry := waitCapturedLog(t, capture, "websocket: connection closed")
	if entry.level != slog.LevelWarn {
		t.Errorf("read-limit level = %v, want WARN", entry.level)
	}
	assertLogString(t, entry, "classification", "read_limit")
	assertLogInt64(t, entry, "close_code", int64(StatusMessageTooBig))
	if strings.Contains(capturedLogsText(capture.snapshot()), "payload-secret-too-large") {
		t.Fatal("read-limit logs leaked payload")
	}
}

func TestPeerPolicyCloseLogsWarnWithoutRawReason(t *testing.T) {
	capture := &conformanceLogCapture{}
	logger := slog.New(&conformanceLogHandler{capture: capture})
	app, err := credo.New(credo.WithLogger(logger), credo.WithoutAccessLog())
	if err != nil {
		t.Fatal(err)
	}
	server := Use(app)
	app.GET("/ws", server.Handler(func(_ *credo.Context, conn *Conn) error {
		_, _, readErr := conn.Read(conn.Context())
		return readErr
	}))
	httpServer := httptest.NewServer(app)
	defer httpServer.Close()
	client, _, err := coderwebsocket.Dial(
		t.Context(), "ws"+strings.TrimPrefix(httpServer.URL, "http")+"/ws", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseNow()
	if err := client.Close(coderwebsocket.StatusPolicyViolation, "peer-reason-secret"); err != nil {
		t.Fatal(err)
	}
	entry := waitCapturedLog(t, capture, "websocket: connection closed")
	if entry.level != slog.LevelWarn {
		t.Errorf("peer-policy level = %v, want WARN", entry.level)
	}
	assertLogString(t, entry, "classification", "peer_policy")
	assertLogInt64(t, entry, "close_code", int64(StatusPolicyViolation))
	if strings.Contains(capturedLogsText(capture.snapshot()), "peer-reason-secret") {
		t.Fatal("peer policy log leaked raw close reason")
	}
}

func TestOperationErrorClassificationPreservesCause(t *testing.T) {
	cause := errors.New("transport")
	err := normalizeOperationError("read", cause)
	if !isOperationError(err) || !errors.Is(err, cause) {
		t.Fatalf("operation error = %v, want wrapped transport cause", err)
	}
	if got := safeErrorType(err); got == "" || strings.Contains(got, cause.Error()) {
		t.Errorf("safeErrorType() = %q, want a non-secret type name", got)
	}
}

func waitCapturedLog(
	t *testing.T,
	capture *conformanceLogCapture,
	message string,
) conformanceCapturedLog {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if entry, ok := capture.find(message); ok {
			return entry
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("log %q was not captured", message)
	return conformanceCapturedLog{}
}

func assertLogAttrCount(t *testing.T, entry conformanceCapturedLog, key string, want int) {
	t.Helper()
	var count int
	for _, attr := range entry.attrs {
		if attr.Key == key {
			count++
		}
	}
	if count != want {
		t.Errorf("log attr %q count = %d, want %d; attrs=%v", key, count, want, entry.attrs)
	}
}

func assertLogString(t *testing.T, entry conformanceCapturedLog, key, want string) {
	t.Helper()
	for _, attr := range entry.attrs {
		if attr.Key == key {
			if got := attr.Value.String(); got != want {
				t.Errorf("log attr %q = %q, want %q", key, got, want)
			}
			return
		}
	}
	t.Errorf("log attr %q missing; attrs=%v", key, entry.attrs)
}

func assertLogInt64(t *testing.T, entry conformanceCapturedLog, key string, want int64) {
	t.Helper()
	for _, attr := range entry.attrs {
		if attr.Key == key {
			if got := attr.Value.Int64(); got != want {
				t.Errorf("log attr %q = %d, want %d", key, got, want)
			}
			return
		}
	}
	t.Errorf("log attr %q missing", key)
}

func assertLogPositiveDuration(t *testing.T, entry conformanceCapturedLog, key string) {
	t.Helper()
	for _, attr := range entry.attrs {
		if attr.Key == key {
			if attr.Value.Duration() <= 0 {
				t.Errorf("log attr %q = %v, want positive", key, attr.Value.Duration())
			}
			return
		}
	}
	t.Errorf("log attr %q missing", key)
}

func capturedLogsText(entries []conformanceCapturedLog) string {
	var builder strings.Builder
	for _, entry := range entries {
		fmt.Fprintf(&builder, "%s %s ", entry.level, entry.message)
		for _, attr := range entry.attrs {
			fmt.Fprintf(&builder, "%s=%v ", attr.Key, attr.Value.Any())
		}
	}
	return builder.String()
}
