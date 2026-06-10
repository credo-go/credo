package httpclient_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/credo-go/credo/httpclient"
)

func TestNew_Defaults(t *testing.T) {
	client := httpclient.New()
	if client.Timeout != 0 {
		t.Errorf("Timeout = %v, want 0", client.Timeout)
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport is %T, want *http.Transport", client.Transport)
	}
	if http.RoundTripper(transport) == http.DefaultTransport {
		t.Error("Transport is the shared http.DefaultTransport, want a clone")
	}
}

func TestNew_WithTimeout(t *testing.T) {
	client := httpclient.New(httpclient.WithTimeout(5 * time.Second))
	if client.Timeout != 5*time.Second {
		t.Errorf("Timeout = %v, want 5s", client.Timeout)
	}
}

func TestNew_WithBaseTransport(t *testing.T) {
	called := false
	base := rtFunc(func(*http.Request) (*http.Response, error) {
		called = true
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: make(http.Header)}, nil
	})
	client := httpclient.New(httpclient.WithBaseTransport(base))

	resp, err := client.Get("http://example.com/")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	resp.Body.Close()
	if !called {
		t.Error("custom base transport was not used")
	}
}

func TestNew_TimeoutBoundsTotalCall(t *testing.T) {
	hits := new(atomic.Int32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		select {
		case <-time.After(30 * time.Second):
		case <-r.Context().Done(): // released when the client gives up
		}
	}))
	t.Cleanup(srv.Close)

	client := httpclient.New(
		httpclient.WithTimeout(100*time.Millisecond),
		httpclient.WithRetry(httpclient.RetryConfig{
			MaxAttempts: 3,
			MinDelay:    time.Millisecond,
			MaxDelay:    2 * time.Millisecond,
		}),
	)

	start := time.Now()
	_, err := client.Get(srv.URL) //nolint:bodyclose // error path returns no body
	if err == nil {
		t.Fatal("Get() error = nil, want timeout")
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Errorf("Get() took %v — Client.Timeout did not bound the total call", elapsed)
	}
	// The deadline error must also suppress further retries.
	if got := hits.Load(); got != 1 {
		t.Errorf("server hits = %d, want 1 (deadline error is never retried)", got)
	}
}

func TestNew_ChainOrderIndependentOfOptionOrder(t *testing.T) {
	const inboundParent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	const inboundTraceID = "4bf92f3577b34da6a3ce929d0e0e4736"

	var (
		mu      sync.Mutex
		parents []string
	)
	hits := new(atomic.Int32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		parents = append(parents, r.Header.Get("Traceparent"))
		mu.Unlock()
		if hits.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	logger, buf := newLogRecorder()

	// Options deliberately passed in a scrambled order — assembly is
	// canonical: timeout → retry → logging → trace → base.
	client := httpclient.New(
		httpclient.WithTracePropagation(),
		httpclient.WithLogging(logger),
		httpclient.WithTimeout(30*time.Second),
		httpclient.WithRetry(httpclient.RetryConfig{
			MaxAttempts: 3,
			MinDelay:    time.Millisecond,
			MaxDelay:    2 * time.Millisecond,
		}),
	)

	ctx := httpclient.SetTraceContext(t.Context(), httpclient.TraceContext{TraceParent: inboundParent})
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/orders?token=secret", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 after retry", resp.StatusCode)
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("server hits = %d, want 2", got)
	}

	// Logging ran inside retry: one line per attempt, with attempt numbers,
	// levels per status, the trace ID from the context, and the query
	// string stripped from the URL.
	lines := parseLogLines(t, buf)
	if len(lines) != 2 {
		t.Fatalf("log lines = %d, want 2 (one per attempt)", len(lines))
	}
	wantLines := []struct {
		level   string
		status  float64
		attempt float64
	}{
		{"ERROR", http.StatusServiceUnavailable, 1},
		{"INFO", http.StatusOK, 2},
	}
	for i, want := range wantLines {
		line := lines[i]
		if line["level"] != want.level || line["status"] != want.status || line["attempt"] != want.attempt {
			t.Errorf("line %d = level %v status %v attempt %v, want %v %v %v",
				i, line["level"], line["status"], line["attempt"], want.level, want.status, want.attempt)
		}
		if line["trace_id"] != inboundTraceID {
			t.Errorf("line %d trace_id = %v, want %q", i, line["trace_id"], inboundTraceID)
		}
		url, _ := line["url"].(string)
		if strings.Contains(url, "token") || strings.Contains(url, "?") {
			t.Errorf("line %d url = %q leaks the query string", i, url)
		}
	}

	// Trace ran inside retry: each attempt carried a child traceparent —
	// same trace ID, fresh span ID per attempt.
	mu.Lock()
	defer mu.Unlock()
	if len(parents) != 2 {
		t.Fatalf("captured traceparents = %d, want 2", len(parents))
	}
	first := strings.Split(parents[0], "-")
	second := strings.Split(parents[1], "-")
	if len(first) != 4 || len(second) != 4 {
		t.Fatalf("malformed traceparents: %q", parents)
	}
	if first[1] != inboundTraceID || second[1] != inboundTraceID {
		t.Errorf("trace IDs = %q/%q, want inbound %q on both attempts", first[1], second[1], inboundTraceID)
	}
	if first[2] == second[2] {
		t.Errorf("span ID %q repeated across attempts, want fresh per attempt", first[2])
	}
}

func TestWithLogging_NilPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("WithLogging(nil) did not panic")
		}
	}()
	httpclient.WithLogging(nil)
}
