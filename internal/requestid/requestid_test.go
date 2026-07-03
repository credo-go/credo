package requestid

import (
	"strings"
	"testing"
)

func TestIsValid(t *testing.T) {
	tests := []struct {
		name string
		id   string
		want bool
	}{
		{"empty is valid", "", true},
		{"alphanumeric", "abcXYZ0189", true},
		{"allowed punctuation", "a-b_c.d", true},
		{"space rejected", "a b", false},
		{"slash rejected", "a/b", false},
		{"newline rejected", "a\nb", false},
		{"crlf header injection rejected", "a\r\nX-Evil: 1", false},
		{"unicode rejected", "abcé", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsValid(tt.id); got != tt.want {
				t.Errorf("IsValid(%q) = %v, want %v", tt.id, got, tt.want)
			}
		})
	}
}

func TestGenerate(t *testing.T) {
	a := Generate()
	if a == "" {
		t.Fatal("Generate() returned an empty string")
	}
	if !IsValid(a) {
		t.Errorf("Generate() = %q, which is not IsValid", a)
	}
	if b := Generate(); a == b {
		t.Errorf("Generate() returned identical values %q on consecutive calls", a)
	}
}

func TestResolve(t *testing.T) {
	const stub = "STUBGENERATED"
	gen := func() string { return stub }

	t.Run("valid value within limit is preserved", func(t *testing.T) {
		if got := Resolve("client-123", DefaultLimit, gen); got != "client-123" {
			t.Errorf("Resolve = %q, want client-123", got)
		}
	})

	t.Run("empty value is regenerated", func(t *testing.T) {
		if got := Resolve("", DefaultLimit, gen); got != stub {
			t.Errorf("Resolve(empty) = %q, want %q", got, stub)
		}
	})

	t.Run("over-limit value is regenerated", func(t *testing.T) {
		long := strings.Repeat("a", DefaultLimit+1)
		if got := Resolve(long, DefaultLimit, gen); got != stub {
			t.Errorf("Resolve(too long) = %q, want %q", got, stub)
		}
	})

	t.Run("invalid characters are regenerated", func(t *testing.T) {
		if got := Resolve("bad id!", DefaultLimit, gen); got != stub {
			t.Errorf("Resolve(invalid) = %q, want %q", got, stub)
		}
	})

	t.Run("non-positive limit falls back to DefaultLimit", func(t *testing.T) {
		id := strings.Repeat("a", DefaultLimit) // exactly at the default cap
		if got := Resolve(id, 0, gen); got != id {
			t.Errorf("Resolve(limit=0) = %q, want the %d-char value preserved", got, DefaultLimit)
		}
	})

	t.Run("nil generator falls back to Generate", func(t *testing.T) {
		got := Resolve("", DefaultLimit, nil)
		if got == "" || !IsValid(got) {
			t.Errorf("Resolve(nil generator) = %q, want a valid generated id", got)
		}
	})
}
