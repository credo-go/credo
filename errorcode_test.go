package credo

import (
	"net/http"
	"strings"
	"testing"
)

// statusToCodeFixture locks every entry of the frozen statusToCode table.
// The fixture is an independent literal on purpose: a table edit must also
// edit this list, making every change a deliberate, reviewed wire decision.
var statusToCodeFixture = []struct {
	status int
	code   string
}{
	{100, "continue"},
	{101, "switching_protocols"},
	{102, "processing"},
	{103, "early_hints"},
	{200, "ok"},
	{201, "created"},
	{202, "accepted"},
	{203, "non_authoritative_information"},
	{204, "no_content"},
	{205, "reset_content"},
	{206, "partial_content"},
	{207, "multi_status"},
	{208, "already_reported"},
	{226, "im_used"},
	{300, "multiple_choices"},
	{301, "moved_permanently"},
	{302, "found"},
	{303, "see_other"},
	{304, "not_modified"},
	{305, "use_proxy"},
	{307, "temporary_redirect"},
	{308, "permanent_redirect"},
	{400, "bad_request"},
	{401, "unauthorized"},
	{402, "payment_required"},
	{403, "forbidden"},
	{404, "not_found"},
	{405, "method_not_allowed"},
	{406, "not_acceptable"},
	{407, "proxy_authentication_required"},
	{408, "request_timeout"},
	{409, "conflict"},
	{410, "gone"},
	{411, "length_required"},
	{412, "precondition_failed"},
	{413, "request_entity_too_large"},
	{414, "request_uri_too_long"},
	{415, "unsupported_media_type"},
	{416, "requested_range_not_satisfiable"},
	{417, "expectation_failed"},
	{418, "im_a_teapot"},
	{421, "misdirected_request"},
	{422, "unprocessable_entity"},
	{423, "locked"},
	{424, "failed_dependency"},
	{425, "too_early"},
	{426, "upgrade_required"},
	{428, "precondition_required"},
	{429, "too_many_requests"},
	{431, "request_header_fields_too_large"},
	{451, "unavailable_for_legal_reasons"},
	{500, "internal_server_error"},
	{501, "not_implemented"},
	{502, "bad_gateway"},
	{503, "service_unavailable"},
	{504, "gateway_timeout"},
	{505, "http_version_not_supported"},
	{506, "variant_also_negotiates"},
	{507, "insufficient_storage"},
	{508, "loop_detected"},
	{510, "not_extended"},
	{511, "network_authentication_required"},
}

func TestStatusToCodeFrozenFixture(t *testing.T) {
	seen := make(map[int]bool, len(statusToCodeFixture))
	for _, entry := range statusToCodeFixture {
		if seen[entry.status] {
			t.Errorf("fixture lists status %d twice", entry.status)
		}
		seen[entry.status] = true
		if got := statusToCode[entry.status]; got != entry.code {
			t.Errorf("statusToCode[%d] = %q, want %q", entry.status, got, entry.code)
		}
	}
	for status := range statusToCode {
		if !seen[status] {
			t.Errorf("statusToCode has unfixtured entry %d: %q", status, statusToCode[status])
		}
	}
}

func TestStatusToCodeGrammar(t *testing.T) {
	for status, code := range statusToCode {
		if !isValidErrorCode(code) {
			t.Errorf("statusToCode[%d] = %q violates the machine-code grammar", status, code)
		}
	}
}

// TestStatusToCodeMatchesLegacyKeySegments proves byte parity between the
// frozen table and the final segment of every legacy statusToKey entry, the
// pre-inversion source of derived wire codes. When statusToKey is removed,
// this parity guarantee devolves onto TestStatusToCodeFrozenFixture.
func TestStatusToCodeMatchesLegacyKeySegments(t *testing.T) {
	if len(statusToKey) == 0 {
		t.Fatal("legacy statusToKey table is empty")
	}
	for status, key := range statusToKey {
		segment := key[strings.LastIndex(key, ".")+1:]
		if got := statusToCode[status]; got != segment {
			t.Errorf("statusToCode[%d] = %q, want legacy key segment %q (from %q)", status, got, segment, key)
		}
	}
}

func TestDefaultCodeForStatus(t *testing.T) {
	tests := []struct {
		name   string
		status int
		want   string
	}{
		{name: "known status", status: 404, want: "not_found"},
		{name: "known teapot", status: 418, want: "im_a_teapot"},
		{name: "unknown in-range status", status: 499, want: "http_499"},
		{name: "unknown gap status", status: 509, want: "http_509"},
		{name: "unknown high status", status: 599, want: "http_599"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := defaultCodeForStatus(tt.status); got != tt.want {
				t.Fatalf("defaultCodeForStatus(%d) = %q, want %q", tt.status, got, tt.want)
			}
		})
	}
}

func TestIsValidHTTPStatus(t *testing.T) {
	tests := []struct {
		status int
		want   bool
	}{
		{-1, false},
		{0, false},
		{99, false},
		{100, true},
		{404, true},
		{999, true},
		{1000, false},
	}

	for _, tt := range tests {
		if got := isValidHTTPStatus(tt.status); got != tt.want {
			t.Errorf("isValidHTTPStatus(%d) = %t, want %t", tt.status, got, tt.want)
		}
	}
}

func TestIsValidErrorCode(t *testing.T) {
	tests := []struct {
		name string
		code string
		want bool
	}{
		{name: "single segment", code: "conflict", want: true},
		{name: "multi segment", code: "email_exists", want: true},
		{name: "digits", code: "http_499", want: true},
		{name: "single character", code: "a", want: true},
		{name: "numeric segments", code: "a1_2b", want: true},
		{name: "empty", code: "", want: false},
		{name: "uppercase", code: "Email_Exists", want: false},
		{name: "hyphen", code: "email-exists", want: false},
		{name: "dotted key", code: "user.not_found", want: false},
		{name: "space", code: "email exists", want: false},
		{name: "leading underscore", code: "_conflict", want: false},
		{name: "trailing underscore", code: "conflict_", want: false},
		{name: "doubled underscore", code: "email__exists", want: false},
		{name: "underscore only", code: "_", want: false},
		{name: "non-ascii", code: "çakışma", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isValidErrorCode(tt.code); got != tt.want {
				t.Fatalf("isValidErrorCode(%q) = %t, want %t", tt.code, got, tt.want)
			}
		})
	}
}

// TestStatusToCodeDriftCanary is informational only: it reports where a
// current standard-library StatusText would sanitize to something other than
// the frozen table entry. Drift never updates codes automatically and never
// fails this test; the frozen table is the wire contract.
func TestStatusToCodeDriftCanary(t *testing.T) {
	sanitize := func(text string) string {
		var b strings.Builder
		for _, r := range strings.ToLower(text) {
			switch {
			case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
				b.WriteRune(r)
			case r == ' ' || r == '-':
				b.WriteByte('_')
			}
		}
		return b.String()
	}

	for status := 100; status <= 599; status++ {
		text := http.StatusText(status)
		frozen, ok := statusToCode[status]
		switch {
		case text == "" && ok:
			t.Logf("drift: status %d frozen as %q but has no StatusText anymore", status, frozen)
		case text != "" && !ok:
			t.Logf("drift: status %d has StatusText %q but no frozen entry", status, text)
		case text != "" && sanitize(text) != frozen:
			t.Logf("drift: status %d frozen as %q, current StatusText sanitizes to %q", status, frozen, sanitize(text))
		}
	}
}
