package credo

import (
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"
)

// newStaticHandler returns a Credo handler that serves files from fsys.
// Used by the catch-all route registered by Static().
func newStaticHandler(fsys fs.FS, cfg StaticConfig) Handler {
	return func(ctx *Context) error {
		filePath := ctx.Request().RouteParam("_static")
		return serveStaticPath(ctx, fsys, filePath, cfg)
	}
}

// newStaticIndexHandler returns a Credo handler that serves the index file
// for the exact prefix match (e.g., GET /static without trailing slash).
func newStaticIndexHandler(fsys fs.FS, cfg StaticConfig) Handler {
	return func(ctx *Context) error {
		return serveStaticPath(ctx, fsys, "", cfg)
	}
}

// newFileHandler returns a Credo handler that serves a single named file.
func newFileHandler(fsys fs.FS, name string, cfg StaticConfig) Handler {
	cleanName := path.Clean("/" + name)[1:] // normalize, strip leading /
	if cleanName == "" {
		cleanName = "."
	}

	return func(ctx *Context) error {
		f, err := fsys.Open(cleanName)
		if err != nil {
			return ErrNotFound
		}
		defer f.Close()

		stat, err := f.Stat()
		if err != nil {
			return ErrNotFound
		}
		if stat.IsDir() {
			return ErrNotFound
		}

		cacheCtx := setStaticHeaders(ctx, cleanName, stat.Name(), cfg)
		return serveFile(ctx, f, stat, cacheCtx, cfg)
	}
}

// serveStaticPath is the core static serving logic shared by catch-all and
// index handlers.
func serveStaticPath(ctx *Context, fsys fs.FS, filePath string, cfg StaticConfig) error {
	decodedPath, err := decodeStaticPath(filePath)
	if err != nil {
		return err
	}

	cleanPath, err := sanitizeStaticPath(decodedPath)
	if err != nil {
		return err
	}

	f, openErr := fsys.Open(cleanPath)
	if openErr != nil {
		// File not found — try SvelteKit-style sibling .html (e.g., /admin/users
		// → admin/users.html) before falling back to SPA root index.
		if cfg.SPA && isSPACandidate(ctx, decodedPath) {
			siblingPath := cleanPath + ".html"
			if sibF, sibErr := fsys.Open(siblingPath); sibErr == nil {
				defer sibF.Close()
				if sibStat, sErr := sibF.Stat(); sErr == nil && !sibStat.IsDir() {
					cacheCtx := setStaticHeaders(ctx, siblingPath, sibStat.Name(), cfg)
					return serveFile(ctx, sibF, sibStat, cacheCtx, cfg)
				}
			}
			return serveIndex(ctx, fsys, cfg)
		}
		return ErrNotFound
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		return ErrNotFound
	}

	// Directory handling.
	if stat.IsDir() {
		// Try index file.
		indexName := cfg.indexName()
		indexPath := path.Join(cleanPath, indexName)
		if indexF, indexErr := fsys.Open(indexPath); indexErr == nil {
			defer indexF.Close()
			indexStat, statErr := indexF.Stat()
			if statErr != nil {
				return ErrNotFound
			}
			cacheCtx := setStaticHeaders(ctx, indexPath, indexStat.Name(), cfg)
			return serveFile(ctx, indexF, indexStat, cacheCtx, cfg)
		}

		// SPA mode: try sibling <path>.html before falling back to root index.
		// This supports SvelteKit static-adapter outputs where /reports.html
		// (parent route) coexists with /reports/ (child routes dir).
		if cfg.SPA && isSPACandidate(ctx, decodedPath) {
			siblingPath := strings.TrimSuffix(cleanPath, "/") + ".html"
			if sibF, sibErr := fsys.Open(siblingPath); sibErr == nil {
				defer sibF.Close()
				if sibStat, sErr := sibF.Stat(); sErr == nil && !sibStat.IsDir() {
					cacheCtx := setStaticHeaders(ctx, siblingPath, sibStat.Name(), cfg)
					return serveFile(ctx, sibF, sibStat, cacheCtx, cfg)
				}
			}
			return serveIndex(ctx, fsys, cfg)
		}

		// Directory listing.
		if cfg.Browse {
			return serveDirListing(ctx, fsys, cleanPath, decodedPath, cfg)
		}

		return ErrNotFound
	}

	cacheCtx := setStaticHeaders(ctx, cleanPath, stat.Name(), cfg)
	return serveFile(ctx, f, stat, cacheCtx, cfg)
}

// decodeStaticPath decodes a route-captured path before sanitization.
// Malformed escape sequences return ErrBadRequest.
func decodeStaticPath(p string) (string, error) {
	decoded, err := url.PathUnescape(p)
	if err != nil {
		return "", ErrBadRequest
	}
	return decoded, nil
}

// serveIndex serves the root index file for SPA fallback.
func serveIndex(ctx *Context, fsys fs.FS, cfg StaticConfig) error {
	indexName := cfg.indexName()
	f, err := fsys.Open(indexName)
	if err != nil {
		return ErrNotFound
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		return ErrNotFound
	}
	cacheCtx := setStaticHeaders(ctx, indexName, stat.Name(), cfg)
	return serveFile(ctx, f, stat, cacheCtx, cfg)
}

// serveFile writes a file to the response using http.ServeContent when
// possible (supports Range requests, If-Modified-Since, Content-Type).
// Falls back to io.Copy for non-seekable fs.File implementations.
func serveFile(ctx *Context, f fs.File, stat fs.FileInfo, cacheCtx StaticCacheContext, cfg StaticConfig) error {
	ctx.Response().Header().Set("X-Content-Type-Options", "nosniff")

	if rs, ok := f.(io.ReadSeeker); ok {
		w := &staticCacheResponseWriter{
			Response: ctx.Response(),
			cacheCtx: cacheCtx,
			cfg:      cfg,
		}
		http.ServeContent(
			w,
			ctx.Request().Request,
			stat.Name(),
			stat.ModTime(),
			rs,
		)
		return nil
	}

	// Fallback for non-seekable fs.File (no Range support).
	ct := detectContentType(stat.Name())
	ctx.Response().Header().Set("Content-Type", ct)
	applyStaticCacheControl(ctx.Response(), cacheCtx, cfg, http.StatusOK)
	ctx.Response().WriteHeader(http.StatusOK)
	_, err := io.Copy(ctx.Response(), f)
	return err
}

// setStaticHeaders sets immediate static headers and returns cache context for
// status-aware Cache-Control application at write time.
func setStaticHeaders(ctx *Context, filePath, fileName string, cfg StaticConfig) StaticCacheContext {
	cacheCtx := newStaticCacheContext(ctx, filePath, fileName, cfg)
	if cfg.Download {
		// mime.FormatMediaType handles RFC 2231/6266 encoding: quotes and
		// escapes special characters, and emits filename*=utf-8''… for
		// non-ASCII names — a naive fmt.Sprintf would let a quote in the
		// file name break out of the parameter.
		ctx.Response().Header().Set("Content-Disposition",
			mime.FormatMediaType("attachment", map[string]string{"filename": fileName}))
	}
	return cacheCtx
}

type staticCacheResponseWriter struct {
	*Response
	cacheCtx StaticCacheContext
	cfg      StaticConfig
}

func (w *staticCacheResponseWriter) WriteHeader(code int) {
	applyStaticCacheControl(w.Response, w.cacheCtx, w.cfg, code)
	w.Response.WriteHeader(code)
}

func (w *staticCacheResponseWriter) Write(b []byte) (int, error) {
	if !w.Committed() {
		w.WriteHeader(http.StatusOK)
	}
	return w.Response.Write(b)
}

func applyStaticCacheControl(r *Response, cacheCtx StaticCacheContext, cfg StaticConfig, code int) {
	if code >= http.StatusBadRequest {
		return
	}
	if cc := cacheControlValue(cacheCtx, cfg); cc != "" {
		r.Header().Set("Cache-Control", cc)
	}
}

func newStaticCacheContext(ctx *Context, filePath, fileName string, cfg StaticConfig) StaticCacheContext {
	return StaticCacheContext{
		RequestPath: ctx.OriginalPath(),
		FilePath:    filePath,
		FileName:    fileName,
		IsHTML:      isHTMLCacheTarget(fileName, cfg),
	}
}

func cacheControlValue(cacheCtx StaticCacheContext, cfg StaticConfig) string {
	if cfg.CacheControl == nil {
		return ""
	}
	return cfg.CacheControl(cacheCtx)
}

func isHTMLCacheTarget(fileName string, cfg StaticConfig) bool {
	ext := strings.ToLower(path.Ext(fileName))
	if ext == ".html" || ext == ".htm" {
		return true
	}
	return fileName == cfg.indexName()
}

// sanitizeStaticPath cleans and validates a decoded URL path segment for
// filesystem access. Returns ErrBadRequest for null bytes, backslashes, and
// explicit parent-directory segments.
func sanitizeStaticPath(p string) (string, error) {
	// Reject null bytes.
	if strings.ContainsRune(p, 0) {
		return "", ErrBadRequest
	}
	// Reject backslashes (not valid in URL paths, potential traversal).
	if strings.ContainsRune(p, '\\') {
		return "", ErrBadRequest
	}
	if hasParentDirSegment(p) {
		return "", ErrBadRequest
	}

	// path.Clean is platform-independent (unlike filepath.Clean).
	cleaned := path.Clean("/" + p)
	// Strip leading slash for fs.FS (which expects relative paths).
	cleaned = strings.TrimPrefix(cleaned, "/")
	if cleaned == "" {
		cleaned = "."
	}
	return cleaned, nil
}

func hasParentDirSegment(p string) bool {
	for segment := range strings.SplitSeq(p, "/") {
		if segment == ".." {
			return true
		}
	}
	return false
}

// isSPACandidate checks whether the request qualifies for SPA fallback:
//   - Method must be GET or HEAD
//   - Last path segment must not contain a dot (no file extension)
func isSPACandidate(ctx *Context, filePath string) bool {
	method := ctx.Request().Method
	if method != http.MethodGet && method != http.MethodHead {
		return false
	}
	return !hasFileExtension(filePath)
}

// hasFileExtension returns true if the last segment of the path contains a dot,
// indicating a file extension (e.g., "app.js", "style.css", "logo.png").
func hasFileExtension(p string) bool {
	// Find the last path segment.
	lastSlash := strings.LastIndexByte(p, '/')
	lastSegment := p
	if lastSlash >= 0 {
		lastSegment = p[lastSlash+1:]
	}
	return strings.ContainsRune(lastSegment, '.')
}

// detectContentType returns the MIME type based on file extension.
// Falls back to "application/octet-stream" for unknown extensions.
func detectContentType(name string) string {
	ext := path.Ext(name)
	if ext == "" {
		return "application/octet-stream"
	}
	// Use the standard library's MIME type detection.
	ct := mime.TypeByExtension(ext)
	if ct == "" {
		return "application/octet-stream"
	}
	return ct
}
