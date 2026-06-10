package httpclient_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/credo-go/credo/httpclient"
)

// rtFunc adapts a function to http.RoundTripper (black-box test stub).
type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// statusRT returns a stub transport answering every request with the given
// status and an empty body.
func statusRT(status int) http.RoundTripper {
	return rtFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: status, Body: http.NoBody, Header: make(http.Header)}, nil
	})
}

// newLogRecorder returns a JSON slog logger and the buffer it writes to.
func newLogRecorder() (*slog.Logger, *bytes.Buffer) {
	buf := new(bytes.Buffer)
	return slog.New(slog.NewJSONHandler(buf, nil)), buf
}

// parseLogLines decodes one JSON object per non-empty line.
func parseLogLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for line := range strings.SplitSeq(buf.String(), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("parse log line %q: %v", line, err)
		}
		lines = append(lines, m)
	}
	return lines
}

func TestLoggingTransport_SuccessLine(t *testing.T) {
	logger, buf := newLogRecorder()
	client := &http.Client{Transport: httpclient.NewLoggingTransport(statusRT(http.StatusOK), logger)}

	// Query string and userinfo both carry secrets — neither may be logged.
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		"http://user:pass@example.com/orders?api_key=secret", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	resp.Body.Close()

	lines := parseLogLines(t, buf)
	if len(lines) != 1 {
		t.Fatalf("log lines = %d, want 1", len(lines))
	}
	line := lines[0]
	if line["msg"] != "outbound request" {
		t.Errorf("msg = %q, want %q", line["msg"], "outbound request")
	}
	if line["level"] != "INFO" {
		t.Errorf("level = %q, want INFO", line["level"])
	}
	if line["method"] != "GET" {
		t.Errorf("method = %q, want GET", line["method"])
	}
	if line["url"] != "http://example.com/orders" {
		t.Errorf("url = %q, want %q (query and userinfo stripped)", line["url"], "http://example.com/orders")
	}
	if line["status"] != float64(http.StatusOK) {
		t.Errorf("status = %v, want 200", line["status"])
	}
	if _, ok := line["duration"]; !ok {
		t.Error("duration attribute missing")
	}
	if _, ok := line["attempt"]; ok {
		t.Error("attempt attribute present without retry transport")
	}
	if _, ok := line["trace_id"]; ok {
		t.Error("trace_id attribute present without trace context")
	}
	if _, ok := line["error"]; ok {
		t.Error("error attribute present on success")
	}
}

func TestLoggingTransport_LevelMapping(t *testing.T) {
	tests := []struct {
		name      string
		base      http.RoundTripper
		wantLevel string
		wantErr   bool
	}{
		{"200 is info", statusRT(200), "INFO", false},
		{"301 is info", statusRT(301), "INFO", false},
		{"404 is warn", statusRT(404), "WARN", false},
		{"429 is warn", statusRT(429), "WARN", false},
		{"500 is error", statusRT(500), "ERROR", false},
		{"503 is error", statusRT(503), "ERROR", false},
		{"transport error is error", rtFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("connection refused")
		}), "ERROR", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger, buf := newLogRecorder()
			client := &http.Client{Transport: httpclient.NewLoggingTransport(tt.base, logger)}

			resp, err := client.Get("http://example.com/")
			if (err != nil) != tt.wantErr {
				t.Fatalf("Get() error = %v, wantErr %v", err, tt.wantErr)
			}
			if resp != nil {
				resp.Body.Close()
			}

			lines := parseLogLines(t, buf)
			if len(lines) != 1 {
				t.Fatalf("log lines = %d, want 1", len(lines))
			}
			line := lines[0]
			if line["level"] != tt.wantLevel {
				t.Errorf("level = %q, want %q", line["level"], tt.wantLevel)
			}
			if _, ok := line["error"]; ok != tt.wantErr {
				t.Errorf("error attribute present = %v, want %v", ok, tt.wantErr)
			}
			if _, ok := line["status"]; ok == tt.wantErr {
				t.Errorf("status attribute present = %v, want %v", ok, !tt.wantErr)
			}
		})
	}
}

func TestLoggingTransport_TraceID(t *testing.T) {
	const parent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

	logger, buf := newLogRecorder()
	client := &http.Client{Transport: httpclient.NewLoggingTransport(statusRT(http.StatusOK), logger)}

	ctx := httpclient.SetTraceContext(t.Context(), httpclient.TraceContext{TraceParent: parent})
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://example.com/", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	resp.Body.Close()

	lines := parseLogLines(t, buf)
	if len(lines) != 1 {
		t.Fatalf("log lines = %d, want 1", len(lines))
	}
	if got, want := lines[0]["trace_id"], "4bf92f3577b34da6a3ce929d0e0e4736"; got != want {
		t.Errorf("trace_id = %v, want %q", got, want)
	}
}

func TestLoggingTransport_InvalidTraceContext_NoTraceID(t *testing.T) {
	logger, buf := newLogRecorder()
	client := &http.Client{Transport: httpclient.NewLoggingTransport(statusRT(http.StatusOK), logger)}

	ctx := httpclient.SetTraceContext(t.Context(), httpclient.TraceContext{TraceParent: "garbage"})
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://example.com/", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	resp.Body.Close()

	lines := parseLogLines(t, buf)
	if _, ok := lines[0]["trace_id"]; ok {
		t.Error("trace_id attribute present for invalid traceparent")
	}
}

func TestLoggingTransport_AttemptAttr(t *testing.T) {
	logger, buf := newLogRecorder()
	// Logging inside retry — the canonical layering — so the attempt
	// number recorded by retry is visible to logging.
	rt := httpclient.NewRetryTransport(
		httpclient.NewLoggingTransport(statusRT(http.StatusServiceUnavailable), logger),
		httpclient.RetryConfig{MaxAttempts: 2, MinDelay: time.Millisecond, MaxDelay: 2 * time.Millisecond},
	)
	client := &http.Client{Transport: rt}

	resp, err := client.Get("http://example.com/")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	resp.Body.Close()

	lines := parseLogLines(t, buf)
	if len(lines) != 2 {
		t.Fatalf("log lines = %d, want 2 (one per attempt)", len(lines))
	}
	for i, line := range lines {
		if got, want := line["attempt"], float64(i+1); got != want {
			t.Errorf("line %d attempt = %v, want %v", i, got, want)
		}
	}
}

func TestNewLoggingTransport_NilLoggerPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("NewLoggingTransport(base, nil) did not panic")
		}
	}()
	httpclient.NewLoggingTransport(http.DefaultTransport, nil)
}
