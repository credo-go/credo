package credo_test

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/credo-go/credo"
)

// testFS creates a minimal in-memory filesystem for static serving tests.
func testFS() fs.FS {
	return fstest.MapFS{
		"index.html":           {Data: []byte("<html>index</html>")},
		"app.js":               {Data: []byte("console.log('hello')")},
		"css/style.css":        {Data: []byte("body{}")},
		"images/logo.png":      {Data: []byte("PNG-DATA")},
		"sub/index.html":       {Data: []byte("<html>sub index</html>")},
		"sub/page.html":        {Data: []byte("<html>sub page</html>")},
		"robots.txt":           {Data: []byte("User-agent: *")},
		"downloads/report.pdf": {Data: []byte("PDF-DATA")},
	}
}

func serve(t *testing.T, app *credo.App, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	r := httptest.NewRequest(method, path, nil)
	app.ServeHTTP(w, r)
	return w
}

// --- Static: basic file serving ---

func TestStatic_ServesFile(t *testing.T) {
	app := mustNew(t)
	app.Static("/static", testFS())

	w := serve(t, app, "GET", "/static/app.js")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Body.String(); got != "console.log('hello')" {
		t.Errorf("body = %q, want %q", got, "console.log('hello')")
	}
}

func TestStatic_ServesNestedFile(t *testing.T) {
	app := mustNew(t)
	app.Static("/static", testFS())

	w := serve(t, app, "GET", "/static/css/style.css")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Body.String(); got != "body{}" {
		t.Errorf("body = %q, want %q", got, "body{}")
	}
}

func TestStatic_ServesIndexForPrefix(t *testing.T) {
	app := mustNew(t)
	app.Static("/static", testFS())

	w := serve(t, app, "GET", "/static")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Body.String(); !strings.Contains(got, "index") {
		t.Errorf("body = %q, want to contain 'index'", got)
	}
}

func TestStatic_ServesIndexForTrailingSlash(t *testing.T) {
	app := mustNew(t)
	app.Static("/static", testFS())

	// /static/ should match the catch-all with empty _static → serve index
	w := serve(t, app, "GET", "/static/")
	// May redirect or serve directly depending on trailing-slash behavior.
	if w.Code != http.StatusOK && w.Code != http.StatusMovedPermanently {
		t.Fatalf("status = %d, want 200 or 301", w.Code)
	}
}

func TestStatic_ServesSubdirectoryIndex(t *testing.T) {
	app := mustNew(t)
	app.Static("/static", testFS())

	w := serve(t, app, "GET", "/static/sub/")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Body.String(); !strings.Contains(got, "sub index") {
		t.Errorf("body = %q, want to contain 'sub index'", got)
	}
}

func TestStatic_NotFound(t *testing.T) {
	app := mustNew(t)
	app.Static("/static", testFS())

	w := serve(t, app, "GET", "/static/nonexistent.txt")
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestStatic_HeadRequest(t *testing.T) {
	app := mustNew(t)
	app.Static("/static", testFS())

	w := serve(t, app, "HEAD", "/static/app.js")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Errorf("HEAD body should be empty, got %d bytes", w.Body.Len())
	}
}

// --- Static: nosniff header ---

func TestStatic_NosniffHeader(t *testing.T) {
	app := mustNew(t)
	app.Static("/static", testFS())

	w := serve(t, app, "GET", "/static/app.js")
	if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want %q", got, "nosniff")
	}
}

// --- Static: SPA mode ---

func TestStatic_SPA_NavigationFallback(t *testing.T) {
	app := mustNew(t)
	app.Static("/", testFS(), credo.StaticConfig{SPA: true})

	w := serve(t, app, "GET", "/dashboard")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (SPA fallback)", w.Code)
	}
	if got := w.Body.String(); !strings.Contains(got, "index") {
		t.Errorf("body = %q, want SPA index", got)
	}
}

func TestStatic_SPA_DeepNavigation(t *testing.T) {
	app := mustNew(t)
	app.Static("/", testFS(), credo.StaticConfig{SPA: true})

	w := serve(t, app, "GET", "/users/123/profile")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (SPA fallback)", w.Code)
	}
}

func TestStatic_SPA_AssetNotFound(t *testing.T) {
	app := mustNew(t)
	app.Static("/", testFS(), credo.StaticConfig{SPA: true})

	w := serve(t, app, "GET", "/nonexistent.js")
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (missing asset)", w.Code)
	}
}

func TestStatic_SPA_CSSNotFound(t *testing.T) {
	app := mustNew(t)
	app.Static("/", testFS(), credo.StaticConfig{SPA: true})

	w := serve(t, app, "GET", "/missing.css")
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (missing CSS)", w.Code)
	}
}

func TestStatic_SPA_EncodedAssetDotNotFallback(t *testing.T) {
	app := mustNew(t)
	app.Static("/", testFS(), credo.StaticConfig{SPA: true})

	w := serve(t, app, "GET", "/missing%2Ejs")
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (encoded asset path)", w.Code)
	}
}

func TestStatic_SPA_ExistingFileServed(t *testing.T) {
	app := mustNew(t)
	app.Static("/", testFS(), credo.StaticConfig{SPA: true})

	w := serve(t, app, "GET", "/app.js")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Body.String(); got != "console.log('hello')" {
		t.Errorf("body = %q, want actual file", got)
	}
}

func TestStatic_SPA_POSTNotFallback(t *testing.T) {
	app := mustNew(t)
	app.Static("/", testFS(), credo.StaticConfig{SPA: true})

	// POST to a path under the static prefix matches the catch-all
	// route pattern but finds no POST handler — the router must return
	// 405 Method Not Allowed, not 404 and not the SPA shell.
	w := serve(t, app, "POST", "/api/data")
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405 (Method Not Allowed)", w.Code)
	}
	if allow := w.Header().Get("Allow"); allow == "" {
		t.Error("Allow header missing on 405 response")
	}
}

// --- Static: Browse mode ---

func TestStatic_Browse_DirectoryListing(t *testing.T) {
	// Use a directory WITHOUT index.html so Browse listing is triggered.
	fsys := fstest.MapFS{
		"docs/readme.txt":  {Data: []byte("readme")},
		"docs/changes.txt": {Data: []byte("changes")},
	}
	app := mustNew(t)
	app.Static("/files", fsys, credo.StaticConfig{Browse: true})

	w := serve(t, app, "GET", "/files/docs/")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "readme.txt") {
		t.Errorf("listing should contain 'readme.txt', got: %s", body)
	}
	if !strings.Contains(body, "changes.txt") {
		t.Errorf("listing should contain 'changes.txt', got: %s", body)
	}
}

func TestStatic_Browse_AppliesHeaders(t *testing.T) {
	fsys := fstest.MapFS{
		"docs/readme.txt": {Data: []byte("readme")},
	}
	app := mustNew(t)
	app.Static("/files", fsys, credo.StaticConfig{
		Browse:       true,
		Download:     true,
		CacheControl: credo.StaticCacheMaxAge(time.Hour),
	})

	w := serve(t, app, "GET", "/files/docs/")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Header().Get("Cache-Control"); got != "public, max-age=3600" {
		t.Errorf("Cache-Control = %q, want %q", got, "public, max-age=3600")
	}
	cd := w.Header().Get("Content-Disposition")
	if !strings.Contains(cd, "attachment") {
		t.Errorf("Content-Disposition = %q, want attachment", cd)
	}
	if !strings.Contains(cd, "docs.html") {
		t.Errorf("Content-Disposition = %q, want docs.html filename", cd)
	}
}

func TestStatic_Browse_NoListingByDefault(t *testing.T) {
	app := mustNew(t)
	fsys := fstest.MapFS{
		"dir/file.txt": {Data: []byte("hello")},
	}
	app.Static("/files", fsys)

	// Requesting a directory without index and without Browse → 404
	w := serve(t, app, "GET", "/files/dir/")
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (Browse disabled)", w.Code)
	}
}

// --- Static: Download mode ---

func TestStatic_Download_ContentDisposition(t *testing.T) {
	app := mustNew(t)
	app.Static("/dl", testFS(), credo.StaticConfig{Download: true})

	w := serve(t, app, "GET", "/dl/robots.txt")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	cd := w.Header().Get("Content-Disposition")
	if !strings.Contains(cd, "attachment") {
		t.Errorf("Content-Disposition = %q, want attachment", cd)
	}
	if !strings.Contains(cd, "robots.txt") {
		t.Errorf("Content-Disposition = %q, want filename", cd)
	}
}

func TestStatic_Download_ContentDispositionEncoding(t *testing.T) {
	tests := []struct {
		name     string
		fileName string
		wantCD   string
	}{
		{
			name:     "quote cannot break out of the parameter",
			fileName: `eviL".txt`,
			wantCD:   `attachment; filename="eviL\".txt"`,
		},
		{
			name:     "non-ASCII uses RFC 2231 filename*",
			fileName: "rapor ğü.pdf",
			wantCD:   `attachment; filename*=utf-8''rapor%20%C4%9F%C3%BC.pdf`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := mustNew(t)
			fsys := fstest.MapFS{
				tt.fileName: {Data: []byte("data")},
			}
			app.Static("/dl", fsys, credo.StaticConfig{Download: true})

			w := serve(t, app, "GET", "/dl/"+url.PathEscape(tt.fileName))
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", w.Code)
			}
			if cd := w.Header().Get("Content-Disposition"); cd != tt.wantCD {
				t.Errorf("Content-Disposition = %q, want %q", cd, tt.wantCD)
			}
		})
	}
}

// --- Static: CacheControl hook & presets ---

func TestStatic_CacheMaxAgePreset_SetsHeader(t *testing.T) {
	app := mustNew(t)
	app.Static("/assets", testFS(), credo.StaticConfig{
		CacheControl: credo.StaticCacheMaxAge(24 * time.Hour),
	})

	w := serve(t, app, "GET", "/assets/app.js")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	cc := w.Header().Get("Cache-Control")
	if cc != "public, max-age=86400" {
		t.Errorf("Cache-Control = %q, want %q", cc, "public, max-age=86400")
	}
}

func TestStatic_NilCacheControl_NoHeader(t *testing.T) {
	app := mustNew(t)
	app.Static("/assets", testFS())

	w := serve(t, app, "GET", "/assets/app.js")
	if cc := w.Header().Get("Cache-Control"); cc != "" {
		t.Errorf("Cache-Control = %q, want empty (no CacheControl hook)", cc)
	}
}

func TestStatic_CacheMaxAgePreset_ZeroNoHeader(t *testing.T) {
	app := mustNew(t)
	app.Static("/assets", testFS(), credo.StaticConfig{
		CacheControl: credo.StaticCacheMaxAge(0),
	})

	w := serve(t, app, "GET", "/assets/app.js")
	if cc := w.Header().Get("Cache-Control"); cc != "" {
		t.Errorf("Cache-Control = %q, want empty (maxAge 0 floors to no header)", cc)
	}
}

func TestStatic_CacheMaxAgePreset_SubSecondFloor(t *testing.T) {
	app := mustNew(t)
	app.Static("/assets", testFS(), credo.StaticConfig{
		CacheControl: credo.StaticCacheMaxAge(1500 * time.Millisecond),
	})

	w := serve(t, app, "GET", "/assets/app.js")
	cc := w.Header().Get("Cache-Control")
	if cc != "public, max-age=1" {
		t.Errorf("Cache-Control = %q, want %q", cc, "public, max-age=1")
	}
}

func TestStatic_ImmutableAssetsPreset_SetsHeaderForAssets(t *testing.T) {
	app := mustNew(t)
	app.Static("/assets", testFS(), credo.StaticConfig{
		CacheControl: credo.StaticCacheImmutableAssets(365 * 24 * time.Hour),
	})

	w := serve(t, app, "GET", "/assets/app.js")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	cc := w.Header().Get("Cache-Control")
	want := "public, max-age=31536000, immutable"
	if cc != want {
		t.Errorf("Cache-Control = %q, want %q", cc, want)
	}
}

func TestStatic_ImmutableAssetsPreset_NoCacheForHTMLResponses(t *testing.T) {
	app := mustNew(t)
	app.Static("/", testFS(), credo.StaticConfig{
		SPA:          true,
		CacheControl: credo.StaticCacheImmutableAssets(365 * 24 * time.Hour),
	})

	w := serve(t, app, "GET", "/dashboard")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-cache, must-revalidate" {
		t.Errorf("Cache-Control for SPA HTML = %q, want no-cache", cc)
	}

	w = serve(t, app, "GET", "/app.js")
	if w.Code != http.StatusOK {
		t.Fatalf("asset status = %d, want 200", w.Code)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
		t.Errorf("asset Cache-Control = %q, want immutable", cc)
	}
}

func TestStatic_ImmutableAssetsPreset_NoCacheForPrerenderedHTML(t *testing.T) {
	app := mustNew(t)
	app.Static("/", spaPrerenderFS(), credo.StaticConfig{
		SPA:          true,
		CacheControl: credo.StaticCacheImmutableAssets(365 * 24 * time.Hour),
	})

	w := serve(t, app, "GET", "/admin/users")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-cache, must-revalidate" {
		t.Errorf("Cache-Control for prerendered HTML = %q, want no-cache", cc)
	}
}

func TestStatic_ImmutableAssetsPreset_NoCacheForDirectoryListing(t *testing.T) {
	app := mustNew(t)
	app.Static("/files", testFS(), credo.StaticConfig{
		Browse:       true,
		CacheControl: credo.StaticCacheImmutableAssets(time.Hour),
	})

	w := serve(t, app, "GET", "/files/css")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-cache, must-revalidate" {
		t.Errorf("Cache-Control for listing = %q, want no-cache", cc)
	}
}

func TestStatic_CacheControlHook_ReceivesResolvedContext(t *testing.T) {
	app := mustNew(t)
	fsys := fstest.MapFS{
		"index.html":                {Data: []byte("<html>spa</html>")},
		"_app/immutable/app.abc.js": {Data: []byte("console.log('hashed')")},
		"version.json":              {Data: []byte(`{"version":"1"}`)},
	}
	var seen []credo.StaticCacheContext
	app.Static("/", fsys, credo.StaticConfig{
		SPA: true,
		CacheControl: func(c credo.StaticCacheContext) string {
			seen = append(seen, c)
			if !c.IsHTML && strings.HasPrefix(c.FilePath, "_app/immutable/") {
				return "public, max-age=31536000, immutable"
			}
			return "no-cache, must-revalidate"
		},
	})

	w := serve(t, app, "GET", "/_app/immutable/app.abc.js")
	if cc := w.Header().Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
		t.Errorf("hashed asset Cache-Control = %q, want immutable", cc)
	}

	w = serve(t, app, "GET", "/version.json")
	if cc := w.Header().Get("Cache-Control"); cc != "no-cache, must-revalidate" {
		t.Errorf("version Cache-Control = %q, want no-cache", cc)
	}

	w = serve(t, app, "GET", "/dashboard")
	if cc := w.Header().Get("Cache-Control"); cc != "no-cache, must-revalidate" {
		t.Errorf("SPA HTML Cache-Control = %q, want no-cache", cc)
	}

	if len(seen) != 3 {
		t.Fatalf("hook calls = %d, want 3 (every successful response)", len(seen))
	}
	if got := seen[0].RequestPath; got != "/_app/immutable/app.abc.js" {
		t.Errorf("first RequestPath = %q, want hashed request path", got)
	}
	if got := seen[0].FilePath; got != "_app/immutable/app.abc.js" {
		t.Errorf("first FilePath = %q, want resolved hashed file", got)
	}
	if got := seen[0].FileName; got != "app.abc.js" {
		t.Errorf("first FileName = %q, want app.abc.js", got)
	}
	if seen[0].IsHTML || seen[1].IsHTML {
		t.Errorf("asset contexts must not be flagged HTML, got %+v", seen[:2])
	}
	if !seen[2].IsHTML {
		t.Errorf("SPA fallback context must be flagged HTML, got %+v", seen[2])
	}
}

func TestStatic_CacheControlHook_NotCalledForErrorStatus(t *testing.T) {
	app := mustNew(t)
	calls := 0
	app.Static("/assets", testFS(), credo.StaticConfig{
		CacheControl: func(credo.StaticCacheContext) string {
			calls++
			return "public, max-age=3600"
		},
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/assets/app.js", nil)
	r.Header.Set("Range", "bytes=999-1000")
	app.ServeHTTP(w, r)

	if w.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusRequestedRangeNotSatisfiable)
	}
	if calls != 0 {
		t.Errorf("hook calls = %d, want 0 for error status", calls)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "" {
		t.Errorf("Cache-Control = %q, want empty", cc)
	}
}

func TestStatic_CacheControlHook_CalledForPartialContent(t *testing.T) {
	app := mustNew(t)
	calls := 0
	app.Static("/assets", testFS(), credo.StaticConfig{
		CacheControl: func(c credo.StaticCacheContext) string {
			calls++
			if c.FilePath == "app.js" {
				return "public, max-age=3600, immutable"
			}
			return ""
		},
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/assets/app.js", nil)
	r.Header.Set("Range", "bytes=0-6")
	app.ServeHTTP(w, r)

	if w.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusPartialContent)
	}
	if calls != 1 {
		t.Errorf("hook calls = %d, want 1", calls)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "public, max-age=3600, immutable" {
		t.Errorf("Cache-Control = %q, want immutable", cc)
	}
}

func TestStatic_CacheControlHook_VerbatimValueAndEmptySkip(t *testing.T) {
	app := mustNew(t)
	fsys := fstest.MapFS{
		"index.html":   {Data: []byte("<html>spa</html>")},
		"version.json": {Data: []byte(`{"version":"1"}`)},
	}
	app.Static("/", fsys, credo.StaticConfig{
		SPA: true,
		CacheControl: func(c credo.StaticCacheContext) string {
			if c.IsHTML {
				return "private, no-cache"
			}
			return "" // explicit skip: no header for non-HTML
		},
	})

	w := serve(t, app, "GET", "/dashboard")
	if cc := w.Header().Get("Cache-Control"); cc != "private, no-cache" {
		t.Errorf("HTML Cache-Control = %q, want verbatim hook value", cc)
	}

	w = serve(t, app, "GET", "/version.json")
	if cc := w.Header().Get("Cache-Control"); cc != "" {
		t.Errorf("Cache-Control = %q, want empty (hook returned \"\")", cc)
	}
}

func TestStatic_CacheControl_NotFoundNoHeader(t *testing.T) {
	app := mustNew(t)
	app.Static("/assets", testFS(), credo.StaticConfig{
		CacheControl: credo.StaticCacheImmutableAssets(time.Hour),
	})

	w := serve(t, app, "GET", "/assets/missing.js")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "" {
		t.Errorf("Cache-Control = %q, want empty", cc)
	}
}

// --- Static: Custom Index ---

func TestStatic_CustomIndex(t *testing.T) {
	fsys := fstest.MapFS{
		"home.html": {Data: []byte("<html>custom home</html>")},
	}
	app := mustNew(t)
	app.Static("/site", fsys, credo.StaticConfig{Index: "home.html"})

	w := serve(t, app, "GET", "/site")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Body.String(); !strings.Contains(got, "custom home") {
		t.Errorf("body = %q, want custom index", got)
	}
}

// --- Static: path traversal security ---

func TestStatic_PathTraversal_DotDot(t *testing.T) {
	app := mustNew(t)
	app.Static("/static", testFS())

	w := serve(t, app, "GET", "/static/../../../etc/passwd")
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestStatic_PathTraversal_Backslash(t *testing.T) {
	app := mustNew(t)
	app.Static("/static", testFS())

	w := httptest.NewRecorder()
	// Manually craft a request with backslash in path.
	r := httptest.NewRequest("GET", "/static/..%5C..%5Cetc%5Cpasswd", nil)
	app.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestStatic_PathTraversal_NullByte(t *testing.T) {
	app := mustNew(t)
	app.Static("/static", testFS())

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/static/app.js%00.html", nil)
	app.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

// --- File: basic ---

func TestFile_ServesFile(t *testing.T) {
	app := mustNew(t)
	app.File("/favicon.ico", testFS(), "images/logo.png")

	w := serve(t, app, "GET", "/favicon.ico")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Body.String(); got != "PNG-DATA" {
		t.Errorf("body = %q, want %q", got, "PNG-DATA")
	}
}

func TestFile_NotFound(t *testing.T) {
	app := mustNew(t)
	app.File("/missing.txt", testFS(), "nonexistent.txt")

	w := serve(t, app, "GET", "/missing.txt")
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestFile_NosniffHeader(t *testing.T) {
	app := mustNew(t)
	app.File("/robots.txt", testFS(), "robots.txt")

	w := serve(t, app, "GET", "/robots.txt")
	if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want %q", got, "nosniff")
	}
}

func TestFile_Download(t *testing.T) {
	app := mustNew(t)
	app.File("/report", testFS(), "downloads/report.pdf", credo.StaticConfig{
		Download: true,
	})

	w := serve(t, app, "GET", "/report")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	cd := w.Header().Get("Content-Disposition")
	if !strings.Contains(cd, "attachment") {
		t.Errorf("Content-Disposition = %q, want attachment", cd)
	}
}

func TestFile_CacheMaxAgePreset(t *testing.T) {
	app := mustNew(t)
	app.File("/logo", testFS(), "images/logo.png", credo.StaticConfig{
		CacheControl: credo.StaticCacheMaxAge(7 * 24 * time.Hour),
	})

	w := serve(t, app, "GET", "/logo")
	cc := w.Header().Get("Cache-Control")
	if cc != "public, max-age=604800" {
		t.Errorf("Cache-Control = %q, want %q", cc, "public, max-age=604800")
	}
}

func TestFile_ImmutableAssetsPreset(t *testing.T) {
	app := mustNew(t)
	app.File("/logo", testFS(), "images/logo.png", credo.StaticConfig{
		CacheControl: credo.StaticCacheImmutableAssets(7 * 24 * time.Hour),
	})

	w := serve(t, app, "GET", "/logo")
	cc := w.Header().Get("Cache-Control")
	if cc != "public, max-age=604800, immutable" {
		t.Errorf("Cache-Control = %q, want %q", cc, "public, max-age=604800, immutable")
	}
}

func TestFile_ImmutableAssetsPreset_NoCacheForHTML(t *testing.T) {
	app := mustNew(t)
	app.File("/home", testFS(), "index.html", credo.StaticConfig{
		CacheControl: credo.StaticCacheImmutableAssets(7 * 24 * time.Hour),
	})

	w := serve(t, app, "GET", "/home")
	cc := w.Header().Get("Cache-Control")
	if cc != "no-cache, must-revalidate" {
		t.Errorf("Cache-Control = %q, want %q", cc, "no-cache, must-revalidate")
	}
}

func TestFile_HeadRequest(t *testing.T) {
	app := mustNew(t)
	app.File("/robots.txt", testFS(), "robots.txt")

	w := serve(t, app, "HEAD", "/robots.txt")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Errorf("HEAD body should be empty, got %d bytes", w.Body.Len())
	}
}

// --- StaticRoute fluent API ---

func TestStaticRoute_Name(t *testing.T) {
	app := mustNew(t)
	sr := app.Static("/assets", testFS())
	sr.Name("assets")

	route := app.GetRoute("assets")
	if route == nil {
		t.Fatal("named route 'assets' not found")
	}
}

func TestStaticRoute_SetMeta(t *testing.T) {
	app := mustNew(t)
	called := 0
	authMW := func(next credo.Handler) credo.Handler {
		return func(ctx *credo.Context) error {
			if val, ok := ctx.Route().LookupMeta("public"); ok && val.(bool) {
				called++
			}
			return next(ctx)
		}
	}

	sr := app.Static("/assets", testFS())
	sr.SetMeta("public", true).Middleware(authMW)

	// Test catch-all route
	serve(t, app, "GET", "/assets/app.js")
	// Test exact match route
	serve(t, app, "GET", "/assets")

	if called != 2 {
		t.Errorf("middleware called %d times, want 2 (both routes)", called)
	}
}

func TestStaticRoute_BuildURI(t *testing.T) {
	app := mustNew(t)
	sr := app.Static("/assets", testFS())

	tests := []struct {
		args []string
		want string
	}{
		{nil, "/assets/"},
		{[]string{""}, "/assets/"},
		{[]string{"css/app.css"}, "/assets/css/app.css"},
		{[]string{"logo.png"}, "/assets/logo.png"},
	}

	for _, tt := range tests {
		got := sr.BuildURI(tt.args...)
		if got != tt.want {
			t.Errorf("BuildURI(%v) = %q, want %q", tt.args, got, tt.want)
		}
	}
}

func TestStaticRoute_BuildURI_RootPrefix(t *testing.T) {
	app := mustNew(t)
	sr := app.Static("/", testFS())

	tests := []struct {
		args []string
		want string
	}{
		{nil, "/"},
		{[]string{""}, "/"},
		{[]string{"app.js"}, "/app.js"},
		{[]string{"/css/style.css"}, "/css/style.css"},
	}

	for _, tt := range tests {
		got := sr.BuildURI(tt.args...)
		if got != tt.want {
			t.Errorf("BuildURI(%v) = %q, want %q", tt.args, got, tt.want)
		}
	}
}

// --- Group.Static ---

func TestGroup_Static(t *testing.T) {
	app := mustNew(t)
	admin := app.Group("/admin")
	admin.Static("/assets", testFS())

	w := serve(t, app, "GET", "/admin/assets/app.js")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Body.String(); got != "console.log('hello')" {
		t.Errorf("body = %q, want %q", got, "console.log('hello')")
	}
}

func TestGroup_File(t *testing.T) {
	app := mustNew(t)
	api := app.Group("/api")
	api.File("/spec.json", fstest.MapFS{
		"openapi.json": {Data: []byte(`{"openapi":"3.1"}`)},
	}, "openapi.json")

	w := serve(t, app, "GET", "/api/spec.json")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
}

// --- Browse: URL-encoding special characters ---

func TestStatic_Browse_URLEncodesSpecialChars(t *testing.T) {
	fsys := fstest.MapFS{
		"docs/my file.txt":  {Data: []byte("space")},
		"docs/test#1.txt":   {Data: []byte("hash")},
		"docs/data?v=1.csv": {Data: []byte("question")},
		"docs/100%.txt":     {Data: []byte("percent")},
	}
	app := mustNew(t)
	app.Static("/files", fsys, credo.StaticConfig{Browse: true})

	w := serve(t, app, "GET", "/files/docs/")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()

	// Verify that href attributes use percent-encoding for URL-unsafe chars.
	// Space → %20 (or +), # → %23, ? → %3F, % → %25
	tests := []struct {
		rawName    string
		wantInHref string // percent-encoded form expected in href
		wantInText string // raw display name expected in link text
	}{
		{"my file.txt", "my%20file.txt", "my file.txt"},
		{"test#1.txt", "test%231.txt", "test#1.txt"},
		{"data?v=1.csv", "data%3Fv=1.csv", "data?v=1.csv"},
		{"100%.txt", "100%25.txt", "100%.txt"},
	}

	for _, tt := range tests {
		if !strings.Contains(body, "href=\""+tt.wantInHref+"\"") {
			t.Errorf("href for %q: want %q in body, got:\n%s", tt.rawName, tt.wantInHref, body)
		}
		if !strings.Contains(body, ">"+tt.wantInText+"<") {
			t.Errorf("display name for %q: want %q in body", tt.rawName, tt.wantInText)
		}
	}
}

// --- Registration-time panics ---

func TestStatic_PanicsOnBraceInPrefix(t *testing.T) {
	app := mustNew(t)
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for prefix with {}")
		}
	}()
	app.Static("/files/{id}", testFS())
}

func TestStaticCacheMaxAge_PanicsOnNegative(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for negative maxAge")
		}
	}()
	credo.StaticCacheMaxAge(-1 * time.Second)
}

func TestStaticCacheImmutableAssets_PanicsOnZero(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for maxAge of zero")
		}
	}()
	credo.StaticCacheImmutableAssets(0)
}

func TestStaticCacheImmutableAssets_PanicsOnSubSecond(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for sub-second maxAge")
		}
	}()
	credo.StaticCacheImmutableAssets(500 * time.Millisecond)
}

func TestFile_PanicsOnBrowse(t *testing.T) {
	app := mustNew(t)
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for Browse on File()")
		}
	}()
	app.File("/x", testFS(), "app.js", credo.StaticConfig{Browse: true})
}

func TestFile_PanicsOnSPA(t *testing.T) {
	app := mustNew(t)
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for SPA on File()")
		}
	}()
	app.File("/x", testFS(), "app.js", credo.StaticConfig{SPA: true})
}

func TestFile_PanicsOnIndex(t *testing.T) {
	app := mustNew(t)
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for Index on File()")
		}
	}()
	app.File("/x", testFS(), "app.js", credo.StaticConfig{Index: "home.html"})
}

// --- Static: root prefix ---

func TestStatic_RootPrefix(t *testing.T) {
	app := mustNew(t)
	app.Static("/", testFS())

	w := serve(t, app, "GET", "/app.js")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Body.String(); got != "console.log('hello')" {
		t.Errorf("body = %q, want %q", got, "console.log('hello')")
	}
}

func TestStatic_RootPrefix_Index(t *testing.T) {
	app := mustNew(t, credo.WithRedirectTrailingSlash(false))
	app.Static("/", testFS())

	w := serve(t, app, "GET", "/")
	// / should serve index.html
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Body.String(); !strings.Contains(got, "index") {
		t.Errorf("body = %q, want to contain 'index'", got)
	}
}

// --- SPA: sibling .html prerender support (SvelteKit / Astro / Next.js export) ---

// spaPrerenderFS mirrors a typical static-adapter export tree: a SPA shell
// at the root, a prerendered "users" page as a sibling .html file, and a
// "reports" parent route living next to its child-routes directory.
func spaPrerenderFS() fs.FS {
	return fstest.MapFS{
		"index.html":       {Data: []byte("<html>spa shell</html>")},
		"admin/users.html": {Data: []byte("<html>users prerender</html>")},
		"reports.html":     {Data: []byte("<html>reports parent</html>")},
		"reports/crm.html": {Data: []byte("<html>crm child</html>")},
		"plain/file.txt":   {Data: []byte("just a file, no .html sibling")},
		"app.js":           {Data: []byte("console.log('hi')")},
	}
}

func TestStatic_SPA_SiblingHTMLForMissingFile(t *testing.T) {
	app := mustNew(t)
	app.Static("/", spaPrerenderFS(), credo.StaticConfig{SPA: true})

	w := serve(t, app, "GET", "/admin/users")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (sibling .html fallback)", w.Code)
	}
	if got := w.Body.String(); !strings.Contains(got, "users prerender") {
		t.Errorf("body = %q, want sibling .html content (not SPA shell)", got)
	}
}

func TestStatic_SPA_SiblingHTMLForDirWithoutIndex(t *testing.T) {
	app := mustNew(t)
	app.Static("/", spaPrerenderFS(), credo.StaticConfig{SPA: true})

	// /reports is a directory (contains crm.html) with no index.html.
	// The prerendered reports.html sibling must take precedence over the
	// SPA root index, so the parent route renders correctly.
	w := serve(t, app, "GET", "/reports")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Body.String(); !strings.Contains(got, "reports parent") {
		t.Errorf("body = %q, want parent route content (not SPA shell)", got)
	}

	// Child routes inside the directory still resolve via sibling .html.
	w = serve(t, app, "GET", "/reports/crm")
	if w.Code != http.StatusOK {
		t.Fatalf("child route status = %d, want 200", w.Code)
	}
	if got := w.Body.String(); !strings.Contains(got, "crm child") {
		t.Errorf("child body = %q, want crm child content", got)
	}
}

func TestStatic_SPA_DirWithoutIndexFallsToRoot(t *testing.T) {
	// Regression: prior to the dir-branch SPA fallback, a directory with no
	// index.html returned 404 even in SPA mode. Now it must fall through to
	// the root index when no sibling .html is available either.
	app := mustNew(t)
	app.Static("/", spaPrerenderFS(), credo.StaticConfig{SPA: true})

	w := serve(t, app, "GET", "/plain")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (SPA root-index fallback)", w.Code)
	}
	if got := w.Body.String(); !strings.Contains(got, "spa shell") {
		t.Errorf("body = %q, want SPA root index", got)
	}
}

func TestStatic_NoSPA_SiblingHTMLNotServed(t *testing.T) {
	// Without SPA mode the sibling .html behavior must NOT trigger;
	// /admin/users with no exact match returns 404 even though
	// admin/users.html exists.
	app := mustNew(t)
	app.Static("/", spaPrerenderFS())

	w := serve(t, app, "GET", "/admin/users")
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (sibling fallback only when SPA=true)", w.Code)
	}
}

func TestStatic_SPA_ExtensionedPathSkipsSibling(t *testing.T) {
	// Paths whose last segment contains a dot (asset-like) must return 404
	// rather than the sibling .html — preserves the dot-heuristic guarantee.
	app := mustNew(t)
	app.Static("/", spaPrerenderFS(), credo.StaticConfig{SPA: true})

	w := serve(t, app, "GET", "/admin/users.json")
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (asset path must not trigger sibling)", w.Code)
	}
}

// --- HEAD propagation: Route.headTwin auto-inherits Middleware/SetMeta ---

func TestStaticRoute_HEAD_AppliesMiddleware(t *testing.T) {
	app := mustNew(t)
	var calls []string
	sr := app.Static("/assets", testFS())
	sr.Middleware(func(next credo.Handler) credo.Handler {
		return func(ctx *credo.Context) error {
			calls = append(calls, ctx.Request().Method+" "+ctx.Request().URL.Path)
			return next(ctx)
		}
	})

	serve(t, app, "GET", "/assets/app.js")
	serve(t, app, "HEAD", "/assets/app.js")
	serve(t, app, "GET", "/assets")
	serve(t, app, "HEAD", "/assets")

	want := []string{
		"GET /assets/app.js",
		"HEAD /assets/app.js",
		"GET /assets",
		"HEAD /assets",
	}
	if len(calls) != len(want) {
		t.Fatalf("middleware calls = %v, want %v", calls, want)
	}
	for i, c := range calls {
		if c != want[i] {
			t.Errorf("call[%d] = %q, want %q", i, c, want[i])
		}
	}
}

func TestStaticRoute_HEAD_AppliesMeta(t *testing.T) {
	app := mustNew(t)
	var seen []string
	metaCheck := func(next credo.Handler) credo.Handler {
		return func(ctx *credo.Context) error {
			if val, ok := ctx.Route().LookupMeta("zone"); ok {
				seen = append(seen, ctx.Request().Method+":"+val.(string))
			}
			return next(ctx)
		}
	}

	sr := app.Static("/assets", testFS())
	sr.SetMeta("zone", "public").Middleware(metaCheck)

	serve(t, app, "GET", "/assets/app.js")
	serve(t, app, "HEAD", "/assets/app.js")
	serve(t, app, "GET", "/assets")
	serve(t, app, "HEAD", "/assets")

	want := []string{"GET:public", "HEAD:public", "GET:public", "HEAD:public"}
	if len(seen) != len(want) {
		t.Fatalf("seen = %v, want %v", seen, want)
	}
	for i, s := range seen {
		if s != want[i] {
			t.Errorf("seen[%d] = %q, want %q", i, s, want[i])
		}
	}
}

func TestRoute_GET_HEADTwinInheritsMiddleware(t *testing.T) {
	// Universal: middleware appended via Route.Middleware on a GET route
	// must also run for HEAD requests so auth and rate-limiting can never be
	// silently bypassed via HEAD.
	app := mustNew(t)
	var calls []string
	mw := func(next credo.Handler) credo.Handler {
		return func(ctx *credo.Context) error {
			calls = append(calls, ctx.Request().Method)
			return next(ctx)
		}
	}
	app.GET("/users", func(c *credo.Context) error { return nil }).Middleware(mw)

	serve(t, app, "GET", "/users")
	serve(t, app, "HEAD", "/users")

	want := []string{"GET", "HEAD"}
	if len(calls) != len(want) || calls[0] != want[0] || calls[1] != want[1] {
		t.Errorf("middleware calls = %v, want %v", calls, want)
	}
}

func TestRoute_GET_HEADTwinInheritsMeta(t *testing.T) {
	app := mustNew(t)
	var seen []string
	mw := func(next credo.Handler) credo.Handler {
		return func(ctx *credo.Context) error {
			if val, ok := ctx.Route().LookupMeta("scope"); ok {
				seen = append(seen, ctx.Request().Method+":"+val.(string))
			}
			return next(ctx)
		}
	}
	app.GET("/users", func(c *credo.Context) error { return nil }).
		SetMeta("scope", "list").
		Middleware(mw)

	serve(t, app, "GET", "/users")
	serve(t, app, "HEAD", "/users")

	want := []string{"GET:list", "HEAD:list"}
	if len(seen) != len(want) || seen[0] != want[0] || seen[1] != want[1] {
		t.Errorf("seen = %v, want %v", seen, want)
	}
}

func TestGroup_GET_HEADTwinInheritsMiddleware(t *testing.T) {
	app := mustNew(t)
	var calls []string
	mw := func(next credo.Handler) credo.Handler {
		return func(ctx *credo.Context) error {
			calls = append(calls, ctx.Request().Method)
			return next(ctx)
		}
	}
	api := app.Group("/api")
	api.GET("/users", func(c *credo.Context) error { return nil }).Middleware(mw)

	serve(t, app, "GET", "/api/users")
	serve(t, app, "HEAD", "/api/users")

	want := []string{"GET", "HEAD"}
	if len(calls) != len(want) || calls[0] != want[0] || calls[1] != want[1] {
		t.Errorf("middleware calls = %v, want %v", calls, want)
	}
}

func TestFile_HEADTwinInheritsMiddleware(t *testing.T) {
	app := mustNew(t)
	var calls []string
	mw := func(next credo.Handler) credo.Handler {
		return func(ctx *credo.Context) error {
			calls = append(calls, ctx.Request().Method)
			return next(ctx)
		}
	}
	app.File("/robots.txt", testFS(), "robots.txt").Middleware(mw)

	serve(t, app, "GET", "/robots.txt")
	serve(t, app, "HEAD", "/robots.txt")

	want := []string{"GET", "HEAD"}
	if len(calls) != len(want) || calls[0] != want[0] || calls[1] != want[1] {
		t.Errorf("middleware calls = %v, want %v", calls, want)
	}
}

func TestRoute_ExplicitHEADBlocksAutoTwin(t *testing.T) {
	// When an explicit HEAD route exists, the GET route's auto-HEAD-twin
	// is silently skipped (addHeadRoute returns nil). Middleware appended
	// to the GET route must NOT leak to the unrelated explicit HEAD route.
	app := mustNew(t)
	var (
		getMW         []string
		explicitCalls int
	)
	mw := func(next credo.Handler) credo.Handler {
		return func(ctx *credo.Context) error {
			getMW = append(getMW, ctx.Request().Method)
			return next(ctx)
		}
	}

	app.HEAD("/x", func(c *credo.Context) error {
		explicitCalls++
		return nil
	})
	app.GET("/x", func(c *credo.Context) error { return nil }).Middleware(mw)

	serve(t, app, "HEAD", "/x")
	if explicitCalls != 1 {
		t.Errorf("explicit HEAD calls = %d, want 1", explicitCalls)
	}
	if len(getMW) != 0 {
		t.Errorf("GET middleware should not run on explicit HEAD route, got %v", getMW)
	}

	serve(t, app, "GET", "/x")
	if len(getMW) != 1 || getMW[0] != "GET" {
		t.Errorf("GET middleware = %v, want [GET]", getMW)
	}
}

// --- SPA: HEAD requests must resolve sibling .html the same way GET does ---

func TestStatic_SPA_HEAD_SiblingHTMLForMissingFile(t *testing.T) {
	// HEAD must walk the same file-branch fallback as GET: missing path with
	// a sibling .html resolves to that sibling so HEAD responses for
	// prerendered routes still report the correct Content-Type / size.
	app := mustNew(t)
	app.Static("/", spaPrerenderFS(), credo.StaticConfig{SPA: true})

	w := serve(t, app, "HEAD", "/admin/users")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (HEAD sibling .html fallback)", w.Code)
	}
	if got := w.Body.Len(); got != 0 {
		t.Errorf("HEAD body length = %d, want 0", got)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html (sibling .html, not SPA shell fallthrough)", ct)
	}
	wantLength := strconv.Itoa(len("<html>users prerender</html>"))
	if got := w.Header().Get("Content-Length"); got != wantLength {
		t.Errorf("Content-Length = %q, want %q (sibling .html size, not SPA shell)", got, wantLength)
	}
}

func TestStatic_SPA_HEAD_SiblingHTMLForDirWithoutIndex(t *testing.T) {
	// Same contract for the directory branch: HEAD on /reports (a directory
	// without index.html but with a sibling reports.html) must serve the
	// sibling — not 404, not the SPA shell.
	app := mustNew(t)
	app.Static("/", spaPrerenderFS(), credo.StaticConfig{SPA: true})

	w := serve(t, app, "HEAD", "/reports")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (HEAD dir-branch sibling .html)", w.Code)
	}
	if got := w.Body.Len(); got != 0 {
		t.Errorf("HEAD body length = %d, want 0", got)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	wantLength := strconv.Itoa(len("<html>reports parent</html>"))
	if got := w.Header().Get("Content-Length"); got != wantLength {
		t.Errorf("Content-Length = %q, want %q (dir-branch sibling .html size, not SPA shell)", got, wantLength)
	}
}

func TestStatic_SPA_HEAD_DirWithoutIndexFallsToRoot(t *testing.T) {
	// Dir with no index AND no sibling .html falls through to the SPA shell;
	// HEAD must observe the same fallthrough as GET.
	app := mustNew(t)
	app.Static("/", spaPrerenderFS(), credo.StaticConfig{SPA: true})

	w := serve(t, app, "HEAD", "/plain")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (HEAD SPA root-index fallback)", w.Code)
	}
	if got := w.Body.Len(); got != 0 {
		t.Errorf("HEAD body length = %d, want 0", got)
	}
}

// --- SPA + Browse precedence: SPA wins for SPA-candidate requests ---

func TestStatic_SPABrowse_DirWithoutIndexServesSPAShell(t *testing.T) {
	// When both SPA and Browse are enabled, an SPA-candidate request
	// (GET, no dot in last segment) must serve the SPA shell rather than
	// expose a browseable file index for a route the SPA owns. This locks
	// the documented precedence so a future refactor can't silently flip
	// the order.
	app := mustNew(t)
	app.Static("/", spaPrerenderFS(), credo.StaticConfig{
		SPA:    true,
		Browse: true,
	})

	w := serve(t, app, "GET", "/plain")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "spa shell") {
		t.Errorf("body = %q, want SPA shell content (Browse listing must be suppressed)", body)
	}
	// A directory listing would mention the contained file name.
	if strings.Contains(body, "file.txt") {
		t.Errorf("body contains directory-listing entry %q; SPA must suppress Browse for SPA-candidate paths", "file.txt")
	}
}

func TestStatic_BrowseOnly_DirWithoutIndexServesListing(t *testing.T) {
	// Control case: with Browse on but SPA off, the same directory must
	// render a directory listing — proves the previous test's assertion
	// is locking the SPA-wins precedence rather than just observing that
	// listings happen to be off.
	app := mustNew(t)
	app.Static("/", spaPrerenderFS(), credo.StaticConfig{Browse: true})

	w := serve(t, app, "GET", "/plain")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "file.txt") {
		t.Errorf("body = %q, want directory listing containing %q", body, "file.txt")
	}
	if strings.Contains(body, "spa shell") {
		t.Errorf("body contains SPA shell content; Browse-only mode must list the directory")
	}
}
