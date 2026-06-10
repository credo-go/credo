package credo

import (
	"net/http/httptest"
	"testing"
)

func TestNewStaticCacheContext_SPAFallbackResolvedIndex(t *testing.T) {
	ctx := NewContext(httptest.NewRecorder(), httptest.NewRequest("GET", "/x", nil))
	ctx.originalPath = "/x"

	cacheCtx := newStaticCacheContext(ctx, "index.html", "index.html", StaticConfig{})

	if cacheCtx.RequestPath != "/x" {
		t.Errorf("RequestPath = %q, want /x", cacheCtx.RequestPath)
	}
	if cacheCtx.FilePath != "index.html" {
		t.Errorf("FilePath = %q, want index.html", cacheCtx.FilePath)
	}
	if cacheCtx.FileName != "index.html" {
		t.Errorf("FileName = %q, want index.html", cacheCtx.FileName)
	}
	if !cacheCtx.IsHTML {
		t.Error("IsHTML = false, want true")
	}
}
