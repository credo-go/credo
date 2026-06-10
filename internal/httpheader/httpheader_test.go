package httpheader

import (
	"net/http"
	"testing"
)

func TestHasToken(t *testing.T) {
	tests := []struct {
		name   string
		values []string
		token  string
		want   bool
	}{
		{"single token", []string{"upgrade"}, "upgrade", true},
		{"case-insensitive", []string{"Upgrade"}, "upgrade", true},
		{"token in list", []string{"keep-alive, Upgrade"}, "upgrade", true},
		{"surrounding spaces", []string{"  upgrade  "}, "upgrade", true},
		{"across multiple values", []string{"keep-alive", "upgrade"}, "upgrade", true},
		{"absent", []string{"keep-alive"}, "upgrade", false},
		{"substring is not a token", []string{"upgrade-insecure"}, "upgrade", false},
		{"empty header", nil, "upgrade", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := http.Header{}
			for _, v := range tt.values {
				h.Add("Connection", v)
			}
			if got := HasToken(h, "Connection", tt.token); got != tt.want {
				t.Errorf("HasToken(%v, %q) = %v, want %v", tt.values, tt.token, got, tt.want)
			}
		})
	}
}

func TestAddToken_Deduplicates(t *testing.T) {
	h := make(http.Header)
	AddToken(h, "Vary", "Origin")
	AddToken(h, "Vary", "Origin")

	if vary := h.Values("Vary"); len(vary) != 1 {
		t.Fatalf("vary header count = %d, want 1", len(vary))
	}
}

func TestAddToken_AppendsDistinct(t *testing.T) {
	h := make(http.Header)
	AddToken(h, "Vary", "Origin")
	AddToken(h, "Vary", "Accept-Encoding")

	if vary := h.Values("Vary"); len(vary) != 2 {
		t.Fatalf("vary header count = %d, want 2", len(vary))
	}
}

// BenchmarkHasToken measures the strings.Cut loop for comma-separated
// header token search (used by the compress middleware for Accept-Encoding).
func BenchmarkHasToken(b *testing.B) {
	h := http.Header{}
	h.Set("Accept-Encoding", "gzip, deflate, br")

	b.ReportAllocs()
	for b.Loop() {
		HasToken(h, "Accept-Encoding", "deflate")
	}
}
