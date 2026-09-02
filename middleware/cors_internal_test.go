package middleware

import (
	"testing"
)

func TestCompileOriginMatcher(t *testing.T) {
	matcher := compileOriginMatcher([]string{"*", "https://Example.com", "https://*.tenant.example.com"})

	if !matcher.allowAll {
		t.Fatal("expected allowAll matcher")
	}
	if len(matcher.patterns) != 2 {
		t.Fatalf("patterns len = %d, want 2", len(matcher.patterns))
	}

	exact := matcher.patterns[0]
	if exact.Wildcard || exact.Origin.Scheme != "https" || exact.Origin.Host != "example.com" || exact.Origin.Port != 443 {
		t.Fatalf("exact pattern = %#v, want canonical https://example.com:443", exact)
	}

	wildcard := matcher.patterns[1]
	if !wildcard.Wildcard || wildcard.Origin.Host != "tenant.example.com" {
		t.Fatalf("wildcard pattern = %#v, want one-label wildcard over tenant.example.com", wildcard)
	}
}

func TestCompileOriginMatcher_InvalidEntryPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for invalid AllowOrigins entry")
		}
	}()
	compileOriginMatcher([]string{"https://example.com", "https://api-*-prod.example.com"})
}
