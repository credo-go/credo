package credo_test

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/middleware"
)

// The cleanup tests parse with a 1 KiB memory threshold and upload a 16 KiB
// file, so the file part is always spilled to a temporary file.
const (
	uploadSpillThreshold = 1 << 10
	uploadFileSize       = 16 << 10
)

// uploadInput is the bind target of the cleanup tests.
type uploadInput struct {
	Count int                   `form:"count"`
	File  *multipart.FileHeader `form:"file"`
}

// redirectTempDir points os.TempDir at a fresh directory for the test, so the
// multipart temporary files a request leaves behind can be counted.
func redirectTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(key, dir)
	}
	return dir
}

func multipartTempFiles(t *testing.T, dir string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "multipart-*"))
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// postUpload sends a multipart form with one text field and one file part
// larger than uploadSpillThreshold, and returns the response status.
func postUpload(t *testing.T, url, count string) int {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if err := mw.WriteField("count", count); err != nil {
		t.Fatal(err)
	}
	fw, err := mw.CreateFormFile("file", "upload.bin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = fw.Write(bytes.Repeat([]byte("x"), uploadFileSize)); err != nil {
		t.Fatal(err)
	}
	if err = mw.Close(); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Post(url, mw.FormDataContentType(), &body)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// TestMultipartTempFiles_RemovedBehindRequestCopies: net/http removes the
// temporary files of a multipart form only for the request it created. A form
// parsed on a copy of that request — SetUser, middleware.Timeout, a wrapped
// stdlib middleware and Mount each install one — is invisible to it, so the
// framework removes those files itself, on every way out of the handler.
func TestMultipartTempFiles_RemovedBehindRequestCopies(t *testing.T) {
	type principal struct{ name string }
	type ctxKey struct{}

	setUser := func(next credo.Handler) credo.Handler {
		return func(ctx *credo.Context) error {
			ctx.SetUser(principal{name: "alice"})
			return next(ctx)
		}
	}
	timeout := middleware.Timeout(middleware.TimeoutConfig{Timeout: time.Minute})
	// A stdlib middleware that hands its own copy of the request down the chain.
	stdCopy := credo.WrapStdMiddleware(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, true)))
		})
	})

	tests := []struct {
		name  string
		count string // the form's count field; "many" fails decoding
		want  int
		// setup registers the upload endpoint at /upload. Handlers call
		// observe once the form is parsed.
		setup func(app *credo.App, observe func())
	}{
		{
			name: "plain route", count: "1", want: http.StatusOK,
			setup: func(app *credo.App, observe func()) {
				app.POST("/upload", bindUpload(observe))
			},
		},
		{
			name: "bind behind SetUser", count: "1", want: http.StatusOK,
			setup: func(app *credo.App, observe func()) {
				app.POST("/upload", bindUpload(observe)).Middleware(setUser)
			},
		},
		{
			name: "direct parse behind SetUser", count: "1", want: http.StatusOK,
			setup: func(app *credo.App, observe func()) {
				app.POST("/upload", parseUpload(observe)).Middleware(setUser)
			},
		},
		{
			name: "bind behind Timeout", count: "1", want: http.StatusOK,
			setup: func(app *credo.App, observe func()) {
				app.POST("/upload", bindUpload(observe)).Middleware(timeout)
			},
		},
		{
			name: "direct parse behind Timeout", count: "1", want: http.StatusOK,
			setup: func(app *credo.App, observe func()) {
				app.POST("/upload", parseUpload(observe)).Middleware(timeout)
			},
		},
		{
			name: "bind behind a stdlib middleware", count: "1", want: http.StatusOK,
			setup: func(app *credo.App, observe func()) {
				app.POST("/upload", bindUpload(observe)).Middleware(stdCopy)
			},
		},
		{
			name: "direct parse behind a stdlib middleware", count: "1", want: http.StatusOK,
			setup: func(app *credo.App, observe func()) {
				app.POST("/upload", parseUpload(observe)).Middleware(stdCopy)
			},
		},
		{
			name: "direct parse behind SetUser inside Timeout", count: "1", want: http.StatusOK,
			setup: func(app *credo.App, observe func()) {
				app.POST("/upload", parseUpload(observe)).Middleware(timeout, setUser)
			},
		},
		{
			name: "mounted stdlib handler", count: "1", want: http.StatusOK,
			setup: func(app *credo.App, observe func()) {
				app.Mount("/upload", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if err := r.ParseMultipartForm(uploadSpillThreshold); err != nil {
						http.Error(w, err.Error(), http.StatusBadRequest)
						return
					}
					observe()
					_, _ = w.Write([]byte("ok"))
				}))
			},
		},
		{
			name: "stdlib handler mounted under a parametric prefix", count: "1", want: http.StatusOK,
			setup: func(app *credo.App, observe func()) {
				app.Mount("/{area}", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if err := r.ParseMultipartForm(uploadSpillThreshold); err != nil {
						http.Error(w, err.Error(), http.StatusBadRequest)
						return
					}
					observe()
					_, _ = w.Write([]byte(r.PathValue("area")))
				}))
			},
		},
		{
			name: "mounted stdlib handler panics", count: "1", want: http.StatusInternalServerError,
			setup: func(app *credo.App, observe func()) {
				app.Mount("/upload", http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
					if err := r.ParseMultipartForm(uploadSpillThreshold); err != nil {
						panic(err)
					}
					observe()
					panic("boom")
				}))
			},
		},
		{
			name: "handler panics after binding", count: "1", want: http.StatusInternalServerError,
			setup: func(app *credo.App, observe func()) {
				app.POST("/upload", func(ctx *credo.Context) error {
					var in uploadInput
					if err := ctx.Request().BindBody(&in); err != nil {
						return err
					}
					observe()
					panic("boom")
				}).Middleware(setUser)
			},
		},
		{
			name: "bind fails after the form is parsed", count: "many", want: http.StatusBadRequest,
			setup: func(app *credo.App, observe func()) {
				app.POST("/upload", func(ctx *credo.Context) error {
					var in uploadInput
					err := ctx.Request().BindBody(&in)
					observe()
					if err == nil {
						return ctx.Response().Text(http.StatusOK, "bound")
					}
					return err
				}).Middleware(setUser)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := redirectTempDir(t)
			logger, _ := newTestLogger(t) // keep the expected panic records off stderr
			app := mustNew(t, credo.WithLogger(logger))
			credo.SetMultipartMaxMemory(app, uploadSpillThreshold)

			// spilled is the number of temporary files the handler saw while
			// the form was live; without it a pass would prove nothing.
			var spilled atomic.Int32
			tt.setup(app, func() { spilled.Store(int32(len(multipartTempFiles(t, dir)))) })

			srv := httptest.NewServer(app)
			defer srv.Close()

			if got := postUpload(t, srv.URL+"/upload", tt.count); got != tt.want {
				t.Fatalf("status = %d, want %d", got, tt.want)
			}
			if spilled.Load() == 0 {
				t.Fatal("the upload was not spilled to a temporary file; the test proves nothing")
			}
			if left := multipartTempFiles(t, dir); len(left) != 0 {
				t.Fatalf("%d multipart temporary file(s) left behind: %s", len(left), strings.Join(left, ", "))
			}
		})
	}
}

// bindUpload binds the form with BindBody.
func bindUpload(observe func()) credo.Handler {
	return func(ctx *credo.Context) error {
		var in uploadInput
		if err := ctx.Request().BindBody(&in); err != nil {
			return err
		}
		observe()
		return ctx.Response().Text(http.StatusOK, "ok")
	}
}

// parseUpload parses the form on the current request itself, the way a
// handler that calls ParseMultipartForm or FormFile does.
func parseUpload(observe func()) credo.Handler {
	return func(ctx *credo.Context) error {
		if err := ctx.Request().ParseMultipartForm(uploadSpillThreshold); err != nil {
			return err
		}
		observe()
		return ctx.Response().Text(http.StatusOK, "ok")
	}
}
