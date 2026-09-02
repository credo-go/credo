package static

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
)

// recorder adapts httptest.ResponseRecorder to ResponseWriter.
type recorder struct {
	*httptest.ResponseRecorder
	committed bool
}

func (r *recorder) WriteHeader(code int) {
	r.committed = true
	r.ResponseRecorder.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	r.committed = true
	return r.ResponseRecorder.Write(b)
}

func (r *recorder) Committed() bool { return r.committed }

func TestNewCacheContext_SPAFallbackResolvedIndex(t *testing.T) {
	cacheCtx := newCacheContext("/x", "index.html", "index.html", Config{})

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

func TestSanitizePath(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", ".", false},
		{"css/app.css", "css/app.css", false},
		{"/css//app.css", "css/app.css", false},
		{"a/./b", "a/b", false},
		{"../etc/passwd", "", true},
		{"a/../../b", "", true},
		{"a\\b", "", true},
		{"a\x00b", "", true},
	}
	for _, tt := range tests {
		got, err := sanitizePath(tt.in)
		if (err != nil) != tt.wantErr {
			t.Errorf("sanitizePath(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			continue
		}
		if err != nil && !errors.Is(err, ErrBadRequest) {
			t.Errorf("sanitizePath(%q) error = %v, want ErrBadRequest", tt.in, err)
		}
		if got != tt.want {
			t.Errorf("sanitizePath(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestHasFileExtension(t *testing.T) {
	for in, want := range map[string]bool{
		"app.js": true, "dir/style.css": true, "admin/users": false, "v1.2/users": false, "": false,
	} {
		if got := hasFileExtension(in); got != want {
			t.Errorf("hasFileExtension(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestDirectoryListingName(t *testing.T) {
	for in, want := range map[string]string{
		"": "listing.html", ".": "listing.html", "/": "listing.html",
		"docs": "docs.html", "a/b/": "b.html",
	} {
		if got := directoryListingName(in); got != want {
			t.Errorf("directoryListingName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFormatFileSize(t *testing.T) {
	for in, want := range map[int64]string{
		0: "0 B", 512: "512 B", 1024: "1.0 KB", 1536: "1.5 KB", 1 << 20: "1.0 MB", 3 << 30: "3.0 GB",
	} {
		if got := formatFileSize(in); got != want {
			t.Errorf("formatFileSize(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestServerAndFileServer(t *testing.T) {
	fsys := fstest.MapFS{
		"index.html":  {Data: []byte("<h1>root</h1>")},
		"css/app.css": {Data: []byte("body{}")},
		"docs/a.txt":  {Data: []byte("a")},
	}
	cfg := Config{
		Browse: true,
		CacheControl: func(c CacheContext) string {
			if c.IsHTML {
				return "no-cache"
			}
			return "max-age=60"
		},
	}
	srv := NewServer(fsys, cfg)

	serve := func(filePath string) *recorder {
		t.Helper()
		rec := &recorder{ResponseRecorder: httptest.NewRecorder()}
		req := httptest.NewRequest(http.MethodGet, "/static/"+filePath, nil)
		if err := srv.Serve(rec, req, "/static/"+filePath, filePath); err != nil {
			t.Fatalf("Serve(%q): %v", filePath, err)
		}
		return rec
	}

	if rec := serve("css/app.css"); rec.Code != 200 || rec.Header().Get("Cache-Control") != "max-age=60" {
		t.Errorf("asset: code=%d headers=%v", rec.Code, rec.Header())
	}
	if rec := serve(""); rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-cache" {
		t.Errorf("index: code=%d headers=%v", rec.Code, rec.Header())
	}
	if rec := serve("docs"); rec.Code != 200 || rec.Header().Get("Content-Type") != "text/html; charset=utf-8" {
		t.Errorf("listing: code=%d headers=%v", rec.Code, rec.Header())
	}

	rec := &recorder{ResponseRecorder: httptest.NewRecorder()}
	req := httptest.NewRequest(http.MethodGet, "/missing", nil)
	if err := srv.Serve(rec, req, "/missing", "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing: err = %v, want ErrNotFound", err)
	}
	if err := srv.Serve(rec, req, "/bad", "%zz"); !errors.Is(err, ErrBadRequest) {
		t.Errorf("bad escape: err = %v, want ErrBadRequest", err)
	}

	file := NewFileServer(fsys, "/docs/a.txt", Config{Download: true})
	rec = &recorder{ResponseRecorder: httptest.NewRecorder()}
	if err := file.Serve(rec, httptest.NewRequest(http.MethodGet, "/a", nil), "/a"); err != nil {
		t.Fatalf("FileServer.Serve: %v", err)
	}
	if rec.Body.String() != "a" || rec.Header().Get("Content-Disposition") == "" {
		t.Errorf("file: body=%q headers=%v", rec.Body.String(), rec.Header())
	}
	dir := NewFileServer(fsys, "docs", Config{})
	if err := dir.Serve(rec, httptest.NewRequest(http.MethodGet, "/d", nil), "/d"); !errors.Is(err, ErrNotFound) {
		t.Errorf("directory via FileServer: err = %v, want ErrNotFound", err)
	}
}
