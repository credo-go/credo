package httpclient

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
)

// TraceContext carries W3C Trace Context headers from an inbound server
// request to outbound client calls. It is a plain string carrier — no OTel
// types are involved.
//
// The TraceParent format is "00-<32 hex trace-id>-<16 hex span-id>-<2 hex
// flags>" (W3C Trace Context version 00). TraceState is optional vendor
// data, forwarded unchanged.
type TraceContext struct {
	TraceParent string
	TraceState  string
}

// traceContextKey is the context key under which SetTraceContext stores a
// TraceContext.
type traceContextKey struct{}

// TraceContextFromRequest reads the W3C traceparent/tracestate headers from
// an inbound request. It reports false when no traceparent header is
// present. The value is not validated here — invalid values are discarded
// at injection time, per the W3C restart guidance.
//
// Typical server-side wiring:
//
//	callCtx := ctx.Context()
//	if tc, ok := httpclient.TraceContextFromRequest(ctx.Request().Request); ok {
//		callCtx = httpclient.SetTraceContext(callCtx, tc)
//	}
//	req, err := http.NewRequestWithContext(callCtx, http.MethodGet, url, nil)
func TraceContextFromRequest(r *http.Request) (TraceContext, bool) {
	tp := r.Header.Get("Traceparent")
	if tp == "" {
		return TraceContext{}, false
	}
	return TraceContext{
		TraceParent: tp,
		TraceState:  r.Header.Get("Tracestate"),
	}, true
}

// SetTraceContext returns a copy of ctx carrying tc. The trace transport
// (see [WithTracePropagation]) derives a child traceparent from it for every
// outbound attempt made with the returned context.
func SetTraceContext(ctx context.Context, tc TraceContext) context.Context {
	return context.WithValue(ctx, traceContextKey{}, tc)
}

// GetTraceContext returns the TraceContext attached to ctx via
// [SetTraceContext], reporting whether one is present.
func GetTraceContext(ctx context.Context) (TraceContext, bool) {
	tc, ok := ctx.Value(traceContextKey{}).(TraceContext)
	return tc, ok
}

// traceID extracts the 32-hex trace ID from the carried traceparent, or ""
// when the traceparent is invalid.
func (tc TraceContext) traceID() string {
	traceID, _, _, ok := parseTraceParent(tc.TraceParent)
	if !ok {
		return ""
	}
	return traceID
}

// NewTraceTransport wraps base with W3C trace context propagation. For each
// request:
//
//   - an already-set traceparent header is left untouched;
//   - a valid [TraceContext] in the request context (see [SetTraceContext])
//     yields a child traceparent — same trace ID and flags, fresh random
//     span ID — with tracestate forwarded unchanged;
//   - otherwise a new root traceparent is generated (random trace and span
//     IDs, flags 01). Invalid inbound values are discarded per the W3C
//     restart guidance.
//
// The derived span IDs do not correspond to recorded spans — there is no
// tracer here. The point is trace-ID continuity, so logs and downstream
// tracers correlate across services.
//
// A nil base defaults to [http.DefaultTransport].
func NewTraceTransport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &traceTransport{base: base}
}

type traceTransport struct {
	base http.RoundTripper
}

func (t *traceTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Header.Get("Traceparent") != "" {
		// The caller set traceparent explicitly — leave it untouched.
		return t.base.RoundTrip(req)
	}

	// RoundTrippers must not modify the caller's request; clone before
	// setting headers. The body reader is shared with the clone, which is
	// fine: only the clone is sent.
	out := req.Clone(req.Context())
	if tc, ok := GetTraceContext(req.Context()); ok {
		if tp, valid := childTraceParent(tc); valid {
			out.Header.Set("Traceparent", tp)
			if tc.TraceState != "" {
				out.Header.Set("Tracestate", tc.TraceState)
			}
			return t.base.RoundTrip(out)
		}
	}
	out.Header.Set("Traceparent", newRootTraceParent())
	return t.base.RoundTrip(out)
}

// childTraceParent derives a child traceparent from tc: same trace ID and
// flags, fresh random span ID. It reports false when tc's traceparent is
// invalid.
func childTraceParent(tc TraceContext) (string, bool) {
	traceID, _, flags, ok := parseTraceParent(tc.TraceParent)
	if !ok {
		return "", false
	}
	return "00-" + traceID + "-" + newSpanID() + "-" + flags, true
}

// newRootTraceParent generates a fresh root traceparent with flags 01
// (sampled).
func newRootTraceParent() string {
	return "00-" + newTraceID() + "-" + newSpanID() + "-01"
}

// parseTraceParent splits and validates a traceparent header value:
// version "-" trace-id "-" parent-id "-" trace-flags. Versions other than
// 00 are accepted as long as the first four fields parse (per the W3C
// forward-compatibility rules); version 00 must have exactly four fields.
func parseTraceParent(tp string) (traceID, spanID, flags string, ok bool) {
	parts := strings.Split(tp, "-")
	if len(parts) < 4 {
		return "", "", "", false
	}
	version := parts[0]
	traceID, spanID, flags = parts[1], parts[2], parts[3]
	switch {
	case len(version) != 2 || !isLowerHex(version) || version == "ff":
		return "", "", "", false
	case version == "00" && len(parts) != 4:
		return "", "", "", false
	case len(traceID) != 32 || !isLowerHex(traceID) || isAllZero(traceID):
		return "", "", "", false
	case len(spanID) != 16 || !isLowerHex(spanID) || isAllZero(spanID):
		return "", "", "", false
	case len(flags) != 2 || !isLowerHex(flags):
		return "", "", "", false
	}
	return traceID, spanID, flags, true
}

func isLowerHex(s string) bool {
	for _, c := range []byte(s) {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func isAllZero(s string) bool {
	for _, c := range []byte(s) {
		if c != '0' {
			return false
		}
	}
	return true
}

func newTraceID() string { return randomHex(16) }

func newSpanID() string { return randomHex(8) }

// randomHex returns 2n lowercase hex characters from n crypto/rand bytes,
// never all-zero (an all-zero ID is invalid per the W3C spec).
func randomHex(n int) string {
	b := make([]byte, n)
	for {
		if _, err := rand.Read(b); err != nil {
			// crypto/rand.Read is documented never to fail.
			panic("httpclient: crypto/rand failed: " + err.Error())
		}
		for _, c := range b {
			if c != 0 {
				return hex.EncodeToString(b)
			}
		}
	}
}
