package middleware

import (
	"testing"
)

func TestParseOriginPattern(t *testing.T) {
	t.Run("valid single wildcard", func(t *testing.T) {
		pattern, ok := parseOriginPattern("https://*.example.com")
		if !ok {
			t.Fatal("expected pattern to parse")
		}
		if pattern.prefix != "https://" || pattern.suffix != ".example.com" {
			t.Fatalf("pattern = %#v, want prefix=https:// suffix=.example.com", pattern)
		}
	})

	t.Run("pattern lowercased", func(t *testing.T) {
		pattern, ok := parseOriginPattern("https://*.Example.COM")
		if !ok {
			t.Fatal("expected pattern to parse")
		}
		if pattern.prefix != "https://" || pattern.suffix != ".example.com" {
			t.Fatalf("pattern = %#v, want prefix=https:// suffix=.example.com (lowercased)", pattern)
		}
	})

	t.Run("no wildcard", func(t *testing.T) {
		if _, ok := parseOriginPattern("https://example.com"); ok {
			t.Fatal("expected parse to fail without wildcard")
		}
	})

	t.Run("multiple wildcards", func(t *testing.T) {
		if _, ok := parseOriginPattern("https://*.*.example.com"); ok {
			t.Fatal("expected parse to fail with multiple wildcards")
		}
	})
}

func TestCompileOriginMatcher(t *testing.T) {
	matcher := compileOriginMatcher([]string{"*", "https://Example.com", "https://*.tenant.example.com"})

	if !matcher.allowAll {
		t.Fatal("expected allowAll matcher")
	}

	if got := matcher.exact["https://example.com"]; got != "https://Example.com" {
		t.Fatalf("exact match value = %q, want https://Example.com", got)
	}

	if len(matcher.patterns) != 1 {
		t.Fatalf("patterns len = %d, want 1", len(matcher.patterns))
	}
}
