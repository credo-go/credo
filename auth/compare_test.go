package auth_test

import (
	"strings"
	"testing"

	"github.com/credo-go/credo/auth"
)

func TestSecureCompare(t *testing.T) {
	tests := []struct {
		name string
		x    string
		y    string
		want bool
	}{
		{"equal", "s3cret-key", "s3cret-key", true},
		{"both empty", "", "", true},
		{"different same length", "s3cret-key", "s3cret-kez", false},
		{"different lengths", "s3cret", "s3cret-key", false},
		{"empty vs non-empty", "", "x", false},
		{"long equal", strings.Repeat("a", 4096), strings.Repeat("a", 4096), true},
		{"unicode equal", "anahtar-ğü", "anahtar-ğü", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := auth.SecureCompare(tt.x, tt.y); got != tt.want {
				t.Errorf("SecureCompare(%q, %q) = %v, want %v", tt.x, tt.y, got, tt.want)
			}
		})
	}
}
