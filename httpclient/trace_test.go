package httpclient_test

import (
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/credo-go/credo/httpclient"
)

const (
	validParent  = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	validTraceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	validSpanID  = "00f067aa0ba902b7"
)

var traceParentRE = regexp.MustCompile(`^00-[0-9a-f]{32}-[0-9a-f]{16}-[0-9a-f]{2}$`)

// captureRT records every outgoing request and answers 200.
type captureRT struct{ reqs []*http.Request }

func (c *captureRT) RoundTrip(r *http.Request) (*http.Response, error) {
	c.reqs = append(c.reqs, r)
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: make(http.Header)}, nil
}

// doTraced sends one GET through a trace transport over capture, with tc
// attached to the context when non-nil.
func doTraced(t *testing.T, capture *captureRT, tc *httpclient.TraceContext) {
	t.Helper()
	client := &http.Client{Transport: httpclient.NewTraceTransport(capture)}
	ctx := t.Context()
	if tc != nil {
		ctx = httpclient.SetTraceContext(ctx, *tc)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://example.com/", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	resp.Body.Close()
}

func TestTraceContext_ContextRoundTrip(t *testing.T) {
	if _, ok := httpclient.GetTraceContext(t.Context()); ok {
		t.Fatal("GetTraceContext on bare context reported ok")
	}
	tc := httpclient.TraceContext{TraceParent: validParent, TraceState: "vendor=x"}
	ctx := httpclient.SetTraceContext(t.Context(), tc)
	got, ok := httpclient.GetTraceContext(ctx)
	if !ok || got != tc {
		t.Fatalf("GetTraceContext() = %+v, %v; want %+v, true", got, ok, tc)
	}
}

func TestTraceContextFromRequest(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "http://example.com/", nil)

	if _, ok := httpclient.TraceContextFromRequest(req); ok {
		t.Fatal("TraceContextFromRequest without headers reported ok")
	}

	req.Header.Set("traceparent", validParent)
	req.Header.Set("tracestate", "vendor=x")
	tc, ok := httpclient.TraceContextFromRequest(req)
	if !ok {
		t.Fatal("TraceContextFromRequest() reported not ok")
	}
	if tc.TraceParent != validParent || tc.TraceState != "vendor=x" {
		t.Errorf("TraceContextFromRequest() = %+v", tc)
	}
}

func TestTraceTransport_ChildDerivation(t *testing.T) {
	capture := new(captureRT)
	doTraced(t, capture, &httpclient.TraceContext{TraceParent: validParent, TraceState: "vendor=x"})

	got := capture.reqs[0].Header.Get("Traceparent")
	if !traceParentRE.MatchString(got) {
		t.Fatalf("traceparent = %q, not a valid header", got)
	}
	parts := strings.Split(got, "-")
	if parts[1] != validTraceID {
		t.Errorf("trace ID = %q, want inbound %q preserved", parts[1], validTraceID)
	}
	if parts[2] == validSpanID {
		t.Error("span ID equals inbound parent span — child must get a fresh span ID")
	}
	if parts[3] != "01" {
		t.Errorf("flags = %q, want inbound %q preserved", parts[3], "01")
	}
	if got := capture.reqs[0].Header.Get("Tracestate"); got != "vendor=x" {
		t.Errorf("tracestate = %q, want forwarded unchanged", got)
	}
}

func TestTraceTransport_FreshSpanIDPerRequest(t *testing.T) {
	capture := new(captureRT)
	tc := &httpclient.TraceContext{TraceParent: validParent}
	doTraced(t, capture, tc)
	doTraced(t, capture, tc)

	first := strings.Split(capture.reqs[0].Header.Get("Traceparent"), "-")
	second := strings.Split(capture.reqs[1].Header.Get("Traceparent"), "-")
	if first[1] != second[1] {
		t.Errorf("trace IDs differ across requests: %q vs %q", first[1], second[1])
	}
	if first[2] == second[2] {
		t.Errorf("span ID %q repeated — must be fresh per request", first[2])
	}
}

func TestTraceTransport_NewRootWithoutInbound(t *testing.T) {
	capture := new(captureRT)
	doTraced(t, capture, nil)

	got := capture.reqs[0].Header.Get("Traceparent")
	if !traceParentRE.MatchString(got) {
		t.Fatalf("traceparent = %q, not a valid header", got)
	}
	if !strings.HasSuffix(got, "-01") {
		t.Errorf("flags = %q, want 01 (sampled) on new root", got)
	}
	if capture.reqs[0].Header.Get("Tracestate") != "" {
		t.Error("tracestate set on new root")
	}
}

func TestTraceTransport_InvalidInboundStartsNewRoot(t *testing.T) {
	tests := []struct {
		name   string
		parent string
	}{
		{"garbage", "garbage"},
		{"too few fields", "00-4bf92f3577b34da6a3ce929d0e0e4736-01"},
		{"short trace id", "00-abc-00f067aa0ba902b7-01"},
		{"all-zero trace id", "00-00000000000000000000000000000000-00f067aa0ba902b7-01"},
		{"all-zero span id", "00-4bf92f3577b34da6a3ce929d0e0e4736-0000000000000000-01"},
		{"version ff", "ff-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"},
		{"uppercase hex", "00-4BF92F3577B34DA6A3CE929D0E0E4736-00f067aa0ba902b7-01"},
		{"non-hex flags", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-zz"},
		{"version 00 with extra field", validParent + "-extra"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			capture := new(captureRT)
			doTraced(t, capture, &httpclient.TraceContext{TraceParent: tt.parent, TraceState: "vendor=x"})

			got := capture.reqs[0].Header.Get("Traceparent")
			if !traceParentRE.MatchString(got) {
				t.Fatalf("traceparent = %q, not a valid new root", got)
			}
			if strings.Contains(got, validTraceID) {
				t.Errorf("traceparent = %q reuses the invalid inbound trace ID", got)
			}
			if capture.reqs[0].Header.Get("Tracestate") != "" {
				t.Error("tracestate forwarded for discarded inbound context")
			}
		})
	}
}

func TestTraceTransport_FutureVersionAccepted(t *testing.T) {
	// W3C forward compatibility: a higher version with extra fields still
	// yields a child with the same trace ID.
	capture := new(captureRT)
	doTraced(t, capture, &httpclient.TraceContext{
		TraceParent: "01-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01-extra",
	})

	got := capture.reqs[0].Header.Get("Traceparent")
	if !traceParentRE.MatchString(got) {
		t.Fatalf("traceparent = %q, not valid", got)
	}
	if parts := strings.Split(got, "-"); parts[1] != validTraceID {
		t.Errorf("trace ID = %q, want %q preserved from future-version parent", parts[1], validTraceID)
	}
}

func TestTraceTransport_PresetHeaderUntouched(t *testing.T) {
	const preset = "00-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-bbbbbbbbbbbbbbbb-00"

	capture := new(captureRT)
	client := &http.Client{Transport: httpclient.NewTraceTransport(capture)}
	ctx := httpclient.SetTraceContext(t.Context(), httpclient.TraceContext{TraceParent: validParent})
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://example.com/", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("traceparent", preset)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	resp.Body.Close()

	if got := capture.reqs[0].Header.Get("Traceparent"); got != preset {
		t.Errorf("traceparent = %q, want preset %q untouched", got, preset)
	}
}

func TestTraceTransport_DoesNotMutateCallerRequest(t *testing.T) {
	capture := new(captureRT)
	client := &http.Client{Transport: httpclient.NewTraceTransport(capture)}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.com/", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	resp.Body.Close()

	if got := req.Header.Get("Traceparent"); got != "" {
		t.Errorf("caller's request header mutated: traceparent = %q", got)
	}
	if got := capture.reqs[0].Header.Get("Traceparent"); got == "" {
		t.Error("outgoing request missing traceparent")
	}
}
