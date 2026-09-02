// Package static implements Credo's static file serving: path decoding and
// sanitization, index and SPA fallbacks, directory listings, Range-aware file
// delivery, and status-aware Cache-Control. The root package owns the public
// StaticConfig/StaticRoute surface and the route registration; it converts
// its config into this package's Config and adapts the request Context to
// the ResponseWriter/http.Request pair the server consumes.
package static

import (
	"errors"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"
)

var (
	// ErrNotFound reports a missing file or directory; the root package maps
	// it to its 404 error.
	ErrNotFound = errors.New("static: not found")

	// ErrBadRequest reports a malformed or unsafe path; the root package maps
	// it to its 400 error.
	ErrBadRequest = errors.New("static: bad request")
)

// Config mirrors credo.StaticConfig. See that type for field semantics.
type Config struct {
	Index        string
	Browse       bool
	SPA          bool
	Download     bool
	CacheControl func(CacheContext) string
}

// indexName returns the configured index file name, defaulting to "index.html".
func (cfg *Config) indexName() string {
	if cfg.Index != "" {
		return cfg.Index
	}
	return "index.html"
}

// CacheContext mirrors credo.StaticCacheContext field-for-field.
type CacheContext struct {
	RequestPath string
	FilePath    string
	FileName    string
	IsHTML      bool
}

// ResponseWriter is the subset of the root Response the server writes to.
type ResponseWriter interface {
	http.ResponseWriter
	Committed() bool
}

// exchange bundles the per-request inputs threaded through the serving helpers.
type exchange struct {
	w           ResponseWriter
	r           *http.Request
	requestPath string // client path before rewrites (CacheContext.RequestPath)
}

// Server serves files from an fs.FS under a route prefix.
type Server struct {
	fsys fs.FS
	cfg  Config
}

// NewServer returns a Server for fsys with cfg.
func NewServer(fsys fs.FS, cfg Config) *Server {
	return &Server{fsys: fsys, cfg: cfg}
}

// Serve resolves and writes filePath (the route-captured, still
// percent-encoded remainder; "" for the prefix itself). requestPath is the
// client path before internal rewrites.
func (s *Server) Serve(w ResponseWriter, r *http.Request, requestPath, filePath string) error {
	return servePath(exchange{w: w, r: r, requestPath: requestPath}, s.fsys, filePath, s.cfg)
}

// FileServer serves one named file from an fs.FS.
type FileServer struct {
	fsys fs.FS
	name string
	cfg  Config
}

// NewFileServer returns a FileServer for name inside fsys. The name is
// normalized once at construction.
func NewFileServer(fsys fs.FS, name string, cfg Config) *FileServer {
	cleanName := path.Clean("/" + name)[1:] // normalize, strip leading /
	if cleanName == "" {
		cleanName = "."
	}
	return &FileServer{fsys: fsys, name: cleanName, cfg: cfg}
}

// Serve writes the file. Directories and missing files report ErrNotFound.
func (f *FileServer) Serve(w ResponseWriter, r *http.Request, requestPath string) error {
	ex := exchange{w: w, r: r, requestPath: requestPath}

	file, err := f.fsys.Open(f.name)
	if err != nil {
		return ErrNotFound
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil || stat.IsDir() {
		return ErrNotFound
	}

	cacheCtx := setHeaders(ex, f.name, stat.Name(), f.cfg)
	return serveFile(ex, file, stat, cacheCtx, f.cfg)
}

// servePath is the core static serving logic shared by the catch-all and
// index handlers.
func servePath(ex exchange, fsys fs.FS, filePath string, cfg Config) error {
	decodedPath, err := decodePath(filePath)
	if err != nil {
		return err
	}

	cleanPath, err := sanitizePath(decodedPath)
	if err != nil {
		return err
	}

	f, openErr := fsys.Open(cleanPath)
	if openErr != nil {
		// File not found — try SvelteKit-style sibling .html (e.g., /admin/users
		// → admin/users.html) before falling back to SPA root index.
		if cfg.SPA && isSPACandidate(ex.r, decodedPath) {
			siblingPath := cleanPath + ".html"
			if sibF, sibErr := fsys.Open(siblingPath); sibErr == nil {
				defer sibF.Close()
				if sibStat, sErr := sibF.Stat(); sErr == nil && !sibStat.IsDir() {
					cacheCtx := setHeaders(ex, siblingPath, sibStat.Name(), cfg)
					return serveFile(ex, sibF, sibStat, cacheCtx, cfg)
				}
			}
			return serveIndex(ex, fsys, cfg)
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
			cacheCtx := setHeaders(ex, indexPath, indexStat.Name(), cfg)
			return serveFile(ex, indexF, indexStat, cacheCtx, cfg)
		}

		// SPA mode: try sibling <path>.html before falling back to root index.
		// This supports SvelteKit static-adapter outputs where /reports.html
		// (parent route) coexists with /reports/ (child routes dir).
		if cfg.SPA && isSPACandidate(ex.r, decodedPath) {
			siblingPath := strings.TrimSuffix(cleanPath, "/") + ".html"
			if sibF, sibErr := fsys.Open(siblingPath); sibErr == nil {
				defer sibF.Close()
				if sibStat, sErr := sibF.Stat(); sErr == nil && !sibStat.IsDir() {
					cacheCtx := setHeaders(ex, siblingPath, sibStat.Name(), cfg)
					return serveFile(ex, sibF, sibStat, cacheCtx, cfg)
				}
			}
			return serveIndex(ex, fsys, cfg)
		}

		// Directory listing.
		if cfg.Browse {
			return serveDirListing(ex, fsys, cleanPath, decodedPath, cfg)
		}

		return ErrNotFound
	}

	cacheCtx := setHeaders(ex, cleanPath, stat.Name(), cfg)
	return serveFile(ex, f, stat, cacheCtx, cfg)
}

// decodePath decodes a route-captured path before sanitization. Malformed
// escape sequences return ErrBadRequest.
func decodePath(p string) (string, error) {
	decoded, err := url.PathUnescape(p)
	if err != nil {
		return "", ErrBadRequest
	}
	return decoded, nil
}

// serveIndex serves the root index file for SPA fallback.
func serveIndex(ex exchange, fsys fs.FS, cfg Config) error {
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
	cacheCtx := setHeaders(ex, indexName, stat.Name(), cfg)
	return serveFile(ex, f, stat, cacheCtx, cfg)
}

// serveFile writes a file to the response using http.ServeContent when
// possible (supports Range requests, If-Modified-Since, Content-Type).
// Falls back to io.Copy for non-seekable fs.File implementations.
func serveFile(ex exchange, f fs.File, stat fs.FileInfo, cacheCtx CacheContext, cfg Config) error {
	ex.w.Header().Set("X-Content-Type-Options", "nosniff")

	if rs, ok := f.(io.ReadSeeker); ok {
		w := &cacheResponseWriter{
			ResponseWriter: ex.w,
			cacheCtx:       cacheCtx,
			cfg:            cfg,
		}
		http.ServeContent(w, ex.r, stat.Name(), stat.ModTime(), rs)
		return nil
	}

	// Fallback for non-seekable fs.File (no Range support).
	ex.w.Header().Set("Content-Type", detectContentType(stat.Name()))
	applyCacheControl(ex.w, cacheCtx, cfg, http.StatusOK)
	ex.w.WriteHeader(http.StatusOK)
	_, err := io.Copy(ex.w, f)
	return err
}

// setHeaders sets immediate static headers and returns the cache context for
// status-aware Cache-Control application at write time.
func setHeaders(ex exchange, filePath, fileName string, cfg Config) CacheContext {
	cacheCtx := newCacheContext(ex.requestPath, filePath, fileName, cfg)
	if cfg.Download {
		// mime.FormatMediaType handles RFC 2231/6266 encoding: quotes and
		// escapes special characters, and emits filename*=utf-8''… for
		// non-ASCII names — a naive fmt.Sprintf would let a quote in the
		// file name break out of the parameter.
		ex.w.Header().Set("Content-Disposition",
			mime.FormatMediaType("attachment", map[string]string{"filename": fileName}))
	}
	return cacheCtx
}

// cacheResponseWriter applies Cache-Control at WriteHeader time so error
// statuses produced by http.ServeContent (412, 416, ...) never carry it.
type cacheResponseWriter struct {
	ResponseWriter
	cacheCtx CacheContext
	cfg      Config
}

func (w *cacheResponseWriter) WriteHeader(code int) {
	applyCacheControl(w.ResponseWriter, w.cacheCtx, w.cfg, code)
	w.ResponseWriter.WriteHeader(code)
}

func (w *cacheResponseWriter) Write(b []byte) (int, error) {
	if !w.Committed() {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap exposes the wrapped writer to http.ResponseController.
func (w *cacheResponseWriter) Unwrap() http.ResponseWriter {
	if u, ok := w.ResponseWriter.(interface{ Unwrap() http.ResponseWriter }); ok {
		return u.Unwrap()
	}
	return w.ResponseWriter
}

func applyCacheControl(w http.ResponseWriter, cacheCtx CacheContext, cfg Config, code int) {
	if code >= http.StatusBadRequest {
		return
	}
	if cfg.CacheControl == nil {
		return
	}
	if cc := cfg.CacheControl(cacheCtx); cc != "" {
		w.Header().Set("Cache-Control", cc)
	}
}

func newCacheContext(requestPath, filePath, fileName string, cfg Config) CacheContext {
	return CacheContext{
		RequestPath: requestPath,
		FilePath:    filePath,
		FileName:    fileName,
		IsHTML:      isHTMLCacheTarget(fileName, cfg),
	}
}

func isHTMLCacheTarget(fileName string, cfg Config) bool {
	ext := strings.ToLower(path.Ext(fileName))
	if ext == ".html" || ext == ".htm" {
		return true
	}
	return fileName == cfg.indexName()
}

// sanitizePath cleans and validates a decoded URL path segment for
// filesystem access. Returns ErrBadRequest for null bytes, backslashes, and
// explicit parent-directory segments.
func sanitizePath(p string) (string, error) {
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
func isSPACandidate(r *http.Request, filePath string) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	return !hasFileExtension(filePath)
}

// hasFileExtension returns true if the last segment of the path contains a dot,
// indicating a file extension (e.g., "app.js", "style.css", "logo.png").
func hasFileExtension(p string) bool {
	lastSegment := p
	if _, after, ok := strings.CutLast(p, "/"); ok {
		lastSegment = after
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
	ct := mime.TypeByExtension(ext)
	if ct == "" {
		return "application/octet-stream"
	}
	return ct
}
