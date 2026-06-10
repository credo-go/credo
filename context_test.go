package credo_test

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"log/slog"
	"mime/multipart"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/credo-go/credo"
)

func TestContext_SetLogger(t *testing.T) {
	logger, buf := newTestLogger(t)

	app := mustNew(t, credo.WithLogger(logger), credo.WithoutRequestID(), credo.WithoutAccessLog())

	var hasBefore, hasAfter []bool
	app.GlobalMiddleware(func(next credo.Handler) credo.Handler {
		return func(ctx *credo.Context) error {
			hasBefore = append(hasBefore, ctx.HasRequestLogger())
			ctx.SetLogger(ctx.Logger().With("tenant_id", "acme"))
			hasAfter = append(hasAfter, ctx.HasRequestLogger())
			return next(ctx)
		}
	})
	app.GET("/", func(ctx *credo.Context) error {
		ctx.Logger().Info("inside handler")
		return ctx.Response().NoContent(200)
	})

	// Two requests: the second proves the pooled context cleared the logger.
	for range 2 {
		app.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	}

	for i := range 2 {
		if hasBefore[i] {
			t.Errorf("request %d: HasRequestLogger before SetLogger = true, want false", i+1)
		}
		if !hasAfter[i] {
			t.Errorf("request %d: HasRequestLogger after SetLogger = false, want true", i+1)
		}
	}

	var entry map[string]any
	line, _, _ := bytes.Cut(buf.Bytes(), []byte("\n"))
	if err := json.Unmarshal(line, &entry); err != nil {
		t.Fatalf("parse log: %v\nraw: %s", err, buf.String())
	}
	if entry["tenant_id"] != "acme" {
		t.Errorf("tenant_id = %v, want acme (handler log should use the request-scoped logger)", entry["tenant_id"])
	}
}

func TestContext_AddLogAttrs(t *testing.T) {
	logger, buf := newTestLogger(t)

	// Built-in request ID tier stays on: AddLogAttrs derives from the
	// already-enriched logger, so request_id must survive.
	app := mustNew(t, credo.WithLogger(logger), credo.WithoutAccessLog())

	app.GlobalMiddleware(func(next credo.Handler) credo.Handler {
		return func(ctx *credo.Context) error {
			ctx.AddLogAttrs("tenant_id", "acme")
			return next(ctx)
		}
	})
	app.GET("/", func(ctx *credo.Context) error {
		ctx.Logger().Info("inside handler")
		return ctx.Response().NoContent(200)
	})

	app.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))

	var entry map[string]any
	line, _, _ := bytes.Cut(buf.Bytes(), []byte("\n"))
	if err := json.Unmarshal(line, &entry); err != nil {
		t.Fatalf("parse log: %v\nraw: %s", err, buf.String())
	}
	if entry["tenant_id"] != "acme" {
		t.Errorf("tenant_id = %v, want acme", entry["tenant_id"])
	}
	if id, _ := entry["request_id"].(string); id == "" {
		t.Errorf("request_id missing from handler log; AddLogAttrs must preserve request ID enrichment\nraw: %s", line)
	}
}

func TestContext_AddLogAttrs_NoArgsIsNoop(t *testing.T) {
	app := mustNew(t, credo.WithoutRequestID())

	app.GET("/", func(ctx *credo.Context) error {
		ctx.AddLogAttrs()
		if ctx.HasRequestLogger() {
			t.Error("HasRequestLogger after AddLogAttrs() = true, want false (no-op)")
		}
		ctx.AddLogAttrs("k", "v")
		if !ctx.HasRequestLogger() {
			t.Error("HasRequestLogger after AddLogAttrs(\"k\", \"v\") = false, want true")
		}
		return ctx.Response().NoContent(200)
	})

	app.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
}

func TestContext_QueryParam(t *testing.T) {
	app := mustNew(t)
	app.GET("/search", func(ctx *credo.Context) error {
		q := ctx.Request().QueryParam("q")
		page := ctx.Request().QueryParam("page")
		return ctx.Response().Text(200, "q="+q+" page="+page)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/search?q=hello&page=2", nil)
	app.ServeHTTP(w, r)

	if w.Body.String() != "q=hello page=2" {
		t.Errorf("body = %q, want 'q=hello page=2'", w.Body.String())
	}
}

func TestContext_BindBody_EmptyBody(t *testing.T) {
	app := mustNew(t)
	app.POST("/test", func(ctx *credo.Context) error {
		var v struct{}
		return ctx.Request().BindBody(&v)
	})

	// nil body — should return 400
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/test", nil)
	app.ServeHTTP(w, r)

	if w.Code != 400 {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestContext_BindBody_InvalidJSON(t *testing.T) {
	app := mustNew(t)
	app.POST("/test", func(ctx *credo.Context) error {
		var v struct{ Name string }
		return ctx.Request().BindBody(&v)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/test", strings.NewReader("not json"))
	app.ServeHTTP(w, r)

	if w.Code != 400 {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestContext_BindQuery_Basic(t *testing.T) {
	app := mustNew(t)
	app.GET("/test", func(ctx *credo.Context) error {
		var q struct {
			Page   int    `query:"page"`
			Sort   string `query:"sort"`
			Active bool   `query:"active"`
		}
		if err := ctx.Request().BindQuery(&q); err != nil {
			return err
		}
		return ctx.Response().Text(200, fmt.Sprintf("page=%d sort=%s active=%v", q.Page, q.Sort, q.Active))
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test?page=2&sort=name&active=true", nil)
	app.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
	want := "page=2 sort=name active=true"
	if w.Body.String() != want {
		t.Errorf("body = %q, want %q", w.Body.String(), want)
	}
}

func TestContext_BindQuery_TypeConversionError(t *testing.T) {
	app := mustNew(t)
	app.GET("/test", func(ctx *credo.Context) error {
		var q struct {
			Page int `query:"page"`
		}
		return ctx.Request().BindQuery(&q)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test?page=abc", nil)
	app.ServeHTTP(w, r)

	if w.Code != 400 {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestContext_BindQuery_MissingOptional(t *testing.T) {
	app := mustNew(t)
	app.GET("/test", func(ctx *credo.Context) error {
		var q struct {
			Page int    `query:"page"`
			Sort string `query:"sort"`
		}
		if err := ctx.Request().BindQuery(&q); err != nil {
			return err
		}
		// Missing params → zero values
		return ctx.Response().Text(200, fmt.Sprintf("page=%d sort=%q", q.Page, q.Sort))
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test?page=5", nil)
	app.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
	want := `page=5 sort=""`
	if w.Body.String() != want {
		t.Errorf("body = %q, want %q", w.Body.String(), want)
	}
}

func TestContext_BindQuery_WithValidation(t *testing.T) {
	app := mustNew(t)
	app.GET("/test", func(ctx *credo.Context) error {
		var q validatedQuery
		if err := ctx.Request().BindQuery(&q); err != nil {
			return err
		}
		return ctx.Response().JSON(200, q)
	})

	// Valid input
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test?name=Alice", nil)
	app.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Errorf("valid: status = %d, want 200", w.Code)
	}

	// Invalid input (empty name)
	w = httptest.NewRecorder()
	r = httptest.NewRequest("GET", "/test?name=", nil)
	app.ServeHTTP(w, r)

	if w.Code != 422 {
		t.Errorf("invalid: status = %d, want 422", w.Code)
	}
}

func TestContext_BindQuery_StringSlice(t *testing.T) {
	app := mustNew(t)
	app.GET("/test", func(ctx *credo.Context) error {
		var q struct {
			Tags []string `query:"tag"`
		}
		if err := ctx.Request().BindQuery(&q); err != nil {
			return err
		}
		return ctx.Response().Text(200, strings.Join(q.Tags, ","))
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test?tag=a&tag=b&tag=c", nil)
	app.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if w.Body.String() != "a,b,c" {
		t.Errorf("body = %q, want %q", w.Body.String(), "a,b,c")
	}
}

func TestContext_BindQuery_NoTag(t *testing.T) {
	app := mustNew(t)
	app.GET("/test", func(ctx *credo.Context) error {
		var q struct {
			Page    int `query:"page"`
			Skipped int // no query tag — should be ignored
		}
		if err := ctx.Request().BindQuery(&q); err != nil {
			return err
		}
		return ctx.Response().Text(200, fmt.Sprintf("page=%d skipped=%d", q.Page, q.Skipped))
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test?page=3&Skipped=99", nil)
	app.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
	want := "page=3 skipped=0"
	if w.Body.String() != want {
		t.Errorf("body = %q, want %q", w.Body.String(), want)
	}
}

func TestContext_BindQuery_NilTarget(t *testing.T) {
	app := mustNew(t)
	app.GET("/test", func(ctx *credo.Context) error {
		type Q struct {
			Page int `query:"page"`
		}
		var q *Q // nil pointer
		return ctx.Request().BindQuery(q)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test?page=1", nil)
	app.ServeHTTP(w, r)

	if w.Code != 400 {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestContext_BindQuery_EmptyQuery(t *testing.T) {
	app := mustNew(t)
	app.GET("/test", func(ctx *credo.Context) error {
		var q struct {
			Page int    `query:"page"`
			Sort string `query:"sort"`
		}
		if err := ctx.Request().BindQuery(&q); err != nil {
			return err
		}
		return ctx.Response().Text(200, fmt.Sprintf("page=%d sort=%q", q.Page, q.Sort))
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test", nil)
	app.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
	want := `page=0 sort=""`
	if w.Body.String() != want {
		t.Errorf("body = %q, want %q", w.Body.String(), want)
	}
}

func TestContext_BindBody_WithValidation(t *testing.T) {
	app := mustNew(t)

	app.POST("/test", func(ctx *credo.Context) error {
		var v validatedStruct
		if err := ctx.Request().BindBody(&v); err != nil {
			return err
		}
		return ctx.Response().JSON(200, v)
	})

	// Valid input
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/test", strings.NewReader(`{"name":"Alice"}`))
	app.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}

	// Invalid input (empty name)
	w = httptest.NewRecorder()
	r = httptest.NewRequest("POST", "/test", strings.NewReader(`{"name":""}`))
	app.ServeHTTP(w, r)

	if w.Code != 422 {
		t.Errorf("status = %d, want 422", w.Code)
	}
}

// validatedStruct implements Validatable for testing.
type validatedStruct struct {
	Name string `json:"name"`
}

func (v *validatedStruct) Validate() error {
	if v.Name == "" {
		return credo.NewHTTPError(422, "name is required")
	}
	return nil
}

// validatedQuery implements Validatable for BindQuery testing.
type validatedQuery struct {
	Name string `query:"name"`
}

func (q *validatedQuery) Validate() error {
	if q.Name == "" {
		return credo.NewHTTPError(422, "name is required")
	}
	return nil
}

// formInput is used for form and multipart BindBody tests.
type formInput struct {
	Name   string `form:"name"`
	Age    int    `form:"age"`
	Active bool   `form:"active"`
}

// validatedFormInput implements Validatable for form BindBody tests.
type validatedFormInput struct {
	Name string `form:"name"`
}

func (f *validatedFormInput) Validate() error {
	if f.Name == "" {
		return credo.NewHTTPError(422, "name is required")
	}
	return nil
}

// xmlInput is used for XML BindBody tests.
type xmlInput struct {
	XMLName xml.Name `xml:"item"`
	Name    string   `xml:"name"`
}

// validatedXMLInput implements Validatable for XML BindBody tests.
type validatedXMLInput struct {
	XMLName xml.Name `xml:"item"`
	Name    string   `xml:"name"`
}

func (x *validatedXMLInput) Validate() error {
	if x.Name == "" {
		return credo.NewHTTPError(422, "name is required")
	}
	return nil
}

// multipartFileInput is used for multipart file binding tests.
type multipartFileInput struct {
	Name   string                  `form:"name"`
	Avatar *multipart.FileHeader   `form:"avatar"`
	Photos []*multipart.FileHeader `form:"photos"`
}

func TestBindBody_JSON_ExplicitContentType(t *testing.T) {
	app := mustNew(t)
	app.POST("/test", func(ctx *credo.Context) error {
		var v struct {
			Name string `json:"name"`
		}
		if err := ctx.Request().BindBody(&v); err != nil {
			return err
		}
		return ctx.Response().Text(200, v.Name)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/test", strings.NewReader(`{"name":"Alice"}`))
	r.Header.Set("Content-Type", "application/json")
	app.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if w.Body.String() != "Alice" {
		t.Errorf("body = %q, want %q", w.Body.String(), "Alice")
	}
}

func TestBindBody_JSON_NoContentType(t *testing.T) {
	app := mustNew(t)
	app.POST("/test", func(ctx *credo.Context) error {
		var v struct {
			Name string `json:"name"`
		}
		if err := ctx.Request().BindBody(&v); err != nil {
			return err
		}
		return ctx.Response().Text(200, v.Name)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/test", strings.NewReader(`{"name":"Bob"}`))
	// No Content-Type header — should default to JSON
	app.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if w.Body.String() != "Bob" {
		t.Errorf("body = %q, want %q", w.Body.String(), "Bob")
	}
}

func TestBindBody_JSON_WithCharset(t *testing.T) {
	app := mustNew(t)
	app.POST("/test", func(ctx *credo.Context) error {
		var v struct {
			Name string `json:"name"`
		}
		if err := ctx.Request().BindBody(&v); err != nil {
			return err
		}
		return ctx.Response().Text(200, v.Name)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/test", strings.NewReader(`{"name":"Charlie"}`))
	r.Header.Set("Content-Type", "application/json; charset=utf-8")
	app.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if w.Body.String() != "Charlie" {
		t.Errorf("body = %q, want %q", w.Body.String(), "Charlie")
	}
}

func TestBindBody_XML(t *testing.T) {
	app := mustNew(t)
	app.POST("/test", func(ctx *credo.Context) error {
		var v xmlInput
		if err := ctx.Request().BindBody(&v); err != nil {
			return err
		}
		return ctx.Response().Text(200, v.Name)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/test", strings.NewReader(`<item><name>Alice</name></item>`))
	r.Header.Set("Content-Type", "application/xml")
	app.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if w.Body.String() != "Alice" {
		t.Errorf("body = %q, want %q", w.Body.String(), "Alice")
	}
}

func TestBindBody_XML_TextXML(t *testing.T) {
	app := mustNew(t)
	app.POST("/test", func(ctx *credo.Context) error {
		var v xmlInput
		if err := ctx.Request().BindBody(&v); err != nil {
			return err
		}
		return ctx.Response().Text(200, v.Name)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/test", strings.NewReader(`<item><name>Bob</name></item>`))
	r.Header.Set("Content-Type", "text/xml")
	app.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if w.Body.String() != "Bob" {
		t.Errorf("body = %q, want %q", w.Body.String(), "Bob")
	}
}

func TestBindBody_XML_Invalid(t *testing.T) {
	app := mustNew(t)
	app.POST("/test", func(ctx *credo.Context) error {
		var v xmlInput
		return ctx.Request().BindBody(&v)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/test", strings.NewReader(`not xml at all`))
	r.Header.Set("Content-Type", "application/xml")
	app.ServeHTTP(w, r)

	if w.Code != 400 {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestBindBody_XML_WithValidation(t *testing.T) {
	app := mustNew(t)
	app.POST("/test", func(ctx *credo.Context) error {
		var v validatedXMLInput
		if err := ctx.Request().BindBody(&v); err != nil {
			return err
		}
		return ctx.Response().Text(200, v.Name)
	})

	// Valid
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/test", strings.NewReader(`<item><name>Alice</name></item>`))
	r.Header.Set("Content-Type", "application/xml")
	app.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Errorf("valid: status = %d, want 200", w.Code)
	}

	// Invalid (empty name)
	w = httptest.NewRecorder()
	r = httptest.NewRequest("POST", "/test", strings.NewReader(`<item><name></name></item>`))
	r.Header.Set("Content-Type", "application/xml")
	app.ServeHTTP(w, r)

	if w.Code != 422 {
		t.Errorf("invalid: status = %d, want 422", w.Code)
	}
}

func TestBindBody_FormURLEncoded(t *testing.T) {
	app := mustNew(t)
	app.POST("/test", func(ctx *credo.Context) error {
		var f formInput
		if err := ctx.Request().BindBody(&f); err != nil {
			return err
		}
		return ctx.Response().Text(200, fmt.Sprintf("name=%s age=%d active=%v", f.Name, f.Age, f.Active))
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/test", strings.NewReader("name=Alice&age=30&active=true"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	app.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
	want := "name=Alice age=30 active=true"
	if w.Body.String() != want {
		t.Errorf("body = %q, want %q", w.Body.String(), want)
	}
}

func TestBindBody_FormURLEncoded_TypeError(t *testing.T) {
	app := mustNew(t)
	app.POST("/test", func(ctx *credo.Context) error {
		var f formInput
		return ctx.Request().BindBody(&f)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/test", strings.NewReader("name=Alice&age=notanumber"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	app.ServeHTTP(w, r)

	if w.Code != 400 {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestBindBody_FormURLEncoded_WithValidation(t *testing.T) {
	app := mustNew(t)
	app.POST("/test", func(ctx *credo.Context) error {
		var f validatedFormInput
		if err := ctx.Request().BindBody(&f); err != nil {
			return err
		}
		return ctx.Response().Text(200, f.Name)
	})

	// Valid
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/test", strings.NewReader("name=Alice"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	app.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Errorf("valid: status = %d, want 200", w.Code)
	}

	// Invalid (empty name)
	w = httptest.NewRecorder()
	r = httptest.NewRequest("POST", "/test", strings.NewReader("name="))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	app.ServeHTTP(w, r)

	if w.Code != 422 {
		t.Errorf("invalid: status = %d, want 422", w.Code)
	}
}

func TestBindBody_Multipart(t *testing.T) {
	app := mustNew(t)
	app.POST("/test", func(ctx *credo.Context) error {
		var f formInput
		if err := ctx.Request().BindBody(&f); err != nil {
			return err
		}
		return ctx.Response().Text(200, fmt.Sprintf("name=%s age=%d active=%v", f.Name, f.Age, f.Active))
	})

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("name", "Alice")
	mw.WriteField("age", "25")
	mw.WriteField("active", "true")
	mw.Close()

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/test", &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	app.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
	want := "name=Alice age=25 active=true"
	if w.Body.String() != want {
		t.Errorf("body = %q, want %q", w.Body.String(), want)
	}
}

func TestBindBody_Multipart_WithValidation(t *testing.T) {
	app := mustNew(t)
	app.POST("/test", func(ctx *credo.Context) error {
		var f validatedFormInput
		if err := ctx.Request().BindBody(&f); err != nil {
			return err
		}
		return ctx.Response().Text(200, f.Name)
	})

	// Valid
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("name", "Alice")
	mw.Close()

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/test", &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	app.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Errorf("valid: status = %d, want 200", w.Code)
	}

	// Invalid (empty name)
	buf.Reset()
	mw = multipart.NewWriter(&buf)
	mw.WriteField("name", "")
	mw.Close()

	w = httptest.NewRecorder()
	r = httptest.NewRequest("POST", "/test", &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	app.ServeHTTP(w, r)

	if w.Code != 422 {
		t.Errorf("invalid: status = %d, want 422", w.Code)
	}
}

func TestBindBody_Multipart_FileUpload(t *testing.T) {
	app := mustNew(t)
	app.POST("/test", func(ctx *credo.Context) error {
		var f multipartFileInput
		if err := ctx.Request().BindBody(&f); err != nil {
			return err
		}
		if f.Avatar == nil {
			return ctx.Response().Text(400, "no avatar")
		}
		return ctx.Response().Text(200, fmt.Sprintf("name=%s file=%s", f.Name, f.Avatar.Filename))
	})

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("name", "Alice")
	fw, _ := mw.CreateFormFile("avatar", "photo.jpg")
	fw.Write([]byte("fake image data"))
	mw.Close()

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/test", &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	app.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
	want := "name=Alice file=photo.jpg"
	if w.Body.String() != want {
		t.Errorf("body = %q, want %q", w.Body.String(), want)
	}
}

func TestBindBody_Multipart_MultipleFiles(t *testing.T) {
	app := mustNew(t)
	app.POST("/test", func(ctx *credo.Context) error {
		var f multipartFileInput
		if err := ctx.Request().BindBody(&f); err != nil {
			return err
		}
		names := make([]string, len(f.Photos))
		for i, p := range f.Photos {
			names[i] = p.Filename
		}
		return ctx.Response().Text(200, strings.Join(names, ","))
	})

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("name", "Alice")
	fw1, _ := mw.CreateFormFile("photos", "a.jpg")
	fw1.Write([]byte("data1"))
	fw2, _ := mw.CreateFormFile("photos", "b.jpg")
	fw2.Write([]byte("data2"))
	mw.Close()

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/test", &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	app.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if w.Body.String() != "a.jpg,b.jpg" {
		t.Errorf("body = %q, want %q", w.Body.String(), "a.jpg,b.jpg")
	}
}

func TestBindBody_UnsupportedContentType(t *testing.T) {
	app := mustNew(t)
	app.POST("/test", func(ctx *credo.Context) error {
		var v struct{ Name string }
		return ctx.Request().BindBody(&v)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/test", strings.NewReader("some data"))
	r.Header.Set("Content-Type", "application/msgpack")
	app.ServeHTTP(w, r)

	if w.Code != 415 {
		t.Errorf("status = %d, want 415", w.Code)
	}
}

func TestContext_XML(t *testing.T) {
	app := mustNew(t)
	type Item struct {
		Name string `xml:"name"`
	}
	app.GET("/xml", func(ctx *credo.Context) error {
		return ctx.Response().XML(200, Item{Name: "test"})
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/xml", nil)
	app.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
	ct := w.Header().Get("Content-Type")
	if ct != "application/xml; charset=utf-8" {
		t.Errorf("Content-Type = %q, want xml", ct)
	}
	if !strings.Contains(w.Body.String(), "<name>test</name>") {
		t.Errorf("body = %q, want to contain '<name>test</name>'", w.Body.String())
	}
}

func TestContext_Redirect(t *testing.T) {
	app := mustNew(t)
	app.GET("/old", func(ctx *credo.Context) error {
		return ctx.Response().Redirect(302, "/new")
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/old", nil)
	app.ServeHTTP(w, r)

	if w.Code != 302 {
		t.Errorf("status = %d, want 302", w.Code)
	}
	loc := w.Header().Get("Location")
	if loc != "/new" {
		t.Errorf("Location = %q, want '/new'", loc)
	}
}

func TestContext_RouteAccess(t *testing.T) {
	app := mustNew(t)
	app.GET("/test", func(ctx *credo.Context) error {
		route := ctx.Route()
		if route == nil {
			return ctx.Response().Text(500, "no route")
		}
		return ctx.Response().Text(200, "method="+route.GetMethod()+" pattern="+route.GetPattern())
	}).Name("test")

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test", nil)
	app.ServeHTTP(w, r)

	body := w.Body.String()
	if !strings.Contains(body, "method=GET") {
		t.Errorf("body = %q, want to contain 'method=GET'", body)
	}
	if !strings.Contains(body, "pattern=/test") {
		t.Errorf("body = %q, want to contain 'pattern=/test'", body)
	}
}

func TestContext_HasRoute(t *testing.T) {
	app := mustNew(t)

	var matchedHasRoute, unmatchedHasRoute bool

	app.GET("/exists", func(ctx *credo.Context) error {
		matchedHasRoute = ctx.HasRoute()
		return ctx.Response().Text(200, "ok")
	})

	app.StatusHandler(404, func(ctx *credo.Context) error {
		unmatchedHasRoute = ctx.HasRoute()
		return credo.ErrNotFound
	})

	// Matched route: HasRoute should be true.
	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/exists", nil))
	if !matchedHasRoute {
		t.Error("HasRoute() = false on matched route, want true")
	}

	// Unmatched route (404): HasRoute should be false.
	w = httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/nope", nil))
	if unmatchedHasRoute {
		t.Error("HasRoute() = true on 404 handler, want false")
	}
}

func TestContext_Context(t *testing.T) {
	app := mustNew(t)
	app.GET("/test", func(ctx *credo.Context) error {
		// Context() returns the underlying request's context.Context.
		rc := ctx.Context()
		if rc == nil {
			return ctx.Response().Text(500, "nil request context")
		}
		// Done() returns a usable channel and Err() is nil for a live request.
		_ = rc.Done()
		if err := rc.Err(); err != nil {
			return ctx.Response().Text(500, "unexpected error")
		}
		return ctx.Response().Text(200, "ok")
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test", nil)
	app.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
}

// --- P2-3: Unsupported field type test ---

func TestBindQuery_UnsupportedFieldType(t *testing.T) {
	type query struct {
		T complex128 `query:"t"`
	}

	app := mustNew(t)
	app.GET("/test", func(ctx *credo.Context) error {
		var q query
		if err := ctx.Request().BindQuery(&q); err != nil {
			return err
		}
		return ctx.Response().Text(200, "ok")
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test?t=hello", nil)
	app.ServeHTTP(w, r)

	if w.Code != 400 {
		t.Errorf("status = %d, want 400 for unsupported field type", w.Code)
	}
}

func TestBindQuery_NumericSlices(t *testing.T) {
	type query struct {
		IDs    []int     `query:"id"`
		Scores []float64 `query:"score"`
	}

	var got query
	app := mustNew(t)
	app.GET("/test", func(ctx *credo.Context) error {
		if err := ctx.Request().BindQuery(&got); err != nil {
			return err
		}
		return ctx.Response().Text(200, "ok")
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test?id=1&id=2&id=3&score=1.5&score=2.5", nil)
	app.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	if len(got.IDs) != 3 || got.IDs[0] != 1 || got.IDs[2] != 3 {
		t.Errorf("IDs = %v, want [1 2 3]", got.IDs)
	}
	if len(got.Scores) != 2 || got.Scores[1] != 2.5 {
		t.Errorf("Scores = %v, want [1.5 2.5]", got.Scores)
	}
}

func TestBindQuery_NumericSlice_InvalidElement(t *testing.T) {
	type query struct {
		IDs []int `query:"id"`
	}

	app := mustNew(t)
	app.GET("/test", func(ctx *credo.Context) error {
		var q query
		if err := ctx.Request().BindQuery(&q); err != nil {
			return err
		}
		return ctx.Response().Text(200, "ok")
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test?id=1&id=abc", nil)
	app.ServeHTTP(w, r)

	if w.Code != 400 {
		t.Errorf("status = %d, want 400 for invalid slice element", w.Code)
	}
}

func TestBindQuery_TextUnmarshaler(t *testing.T) {
	type query struct {
		Since time.Time  `query:"since"`
		Until *time.Time `query:"until"`
	}

	var got query
	app := mustNew(t)
	app.GET("/test", func(ctx *credo.Context) error {
		if err := ctx.Request().BindQuery(&got); err != nil {
			return err
		}
		return ctx.Response().Text(200, "ok")
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test?since=2026-01-02T15:04:05Z&until=2026-06-10T00:00:00Z", nil)
	app.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	if got.Since.Year() != 2026 || got.Since.Month() != time.January {
		t.Errorf("Since = %v, want 2026-01-02", got.Since)
	}
	if got.Until == nil || got.Until.Month() != time.June {
		t.Errorf("Until = %v, want 2026-06-10", got.Until)
	}

	// Invalid RFC 3339 value → 400.
	w = httptest.NewRecorder()
	r = httptest.NewRequest("GET", "/test?since=not-a-time", nil)
	app.ServeHTTP(w, r)
	if w.Code != 400 {
		t.Errorf("status = %d, want 400 for invalid time value", w.Code)
	}
}

// --- P2-3: Numeric overflow tests ---

func TestBindQuery_IntOverflow(t *testing.T) {
	type query struct {
		Val int8 `query:"val"`
	}

	tests := []struct {
		name     string
		query    string
		wantCode int
	}{
		{"int8 in range", "val=127", 200},
		{"int8 min", "val=-128", 200},
		{"int8 overflow", "val=200", 400},
		{"int8 underflow", "val=-200", 400},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := mustNew(t)
			app.GET("/test", func(ctx *credo.Context) error {
				var q query
				if err := ctx.Request().BindQuery(&q); err != nil {
					return err
				}
				return ctx.Response().Text(200, "ok")
			})

			w := httptest.NewRecorder()
			r := httptest.NewRequest("GET", "/test?"+tt.query, nil)
			app.ServeHTTP(w, r)

			if w.Code != tt.wantCode {
				t.Errorf("status = %d, want %d", w.Code, tt.wantCode)
			}
		})
	}
}

func TestBindQuery_UintOverflow(t *testing.T) {
	type query struct {
		Val uint8 `query:"val"`
	}

	app := mustNew(t)
	app.GET("/test", func(ctx *credo.Context) error {
		var q query
		if err := ctx.Request().BindQuery(&q); err != nil {
			return err
		}
		return ctx.Response().Text(200, "ok")
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test?val=300", nil)
	app.ServeHTTP(w, r)

	if w.Code != 400 {
		t.Errorf("status = %d, want 400 for uint8 overflow", w.Code)
	}
}

func TestBindQuery_Float32Overflow(t *testing.T) {
	type query struct {
		Val float32 `query:"val"`
	}

	app := mustNew(t)
	app.GET("/test", func(ctx *credo.Context) error {
		var q query
		if err := ctx.Request().BindQuery(&q); err != nil {
			return err
		}
		return ctx.Response().Text(200, "ok")
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test?val=3.5e39", nil)
	app.ServeHTTP(w, r)

	if w.Code != 400 {
		t.Errorf("status = %d, want 400 for float32 overflow", w.Code)
	}
}

// --- Pointer field support tests ---

func fmtPtr[T any](v *T) string {
	if v == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%v", *v)
}

func TestBindQuery_PointerFields(t *testing.T) {
	type query struct {
		Name   *string  `query:"name"`
		Page   *int     `query:"page"`
		Score  *float64 `query:"score"`
		Active *bool    `query:"active"`
	}

	tests := []struct {
		name     string
		url      string
		wantCode int
		wantBody string
	}{
		{
			name:     "all present",
			url:      "/test?name=Alice&page=2&score=9.5&active=true",
			wantCode: 200,
			wantBody: "name=Alice page=2 score=9.5 active=true",
		},
		{
			name:     "all absent",
			url:      "/test",
			wantCode: 200,
			wantBody: "name=<nil> page=<nil> score=<nil> active=<nil>",
		},
		{
			name:     "partial",
			url:      "/test?name=Bob&active=false",
			wantCode: 200,
			wantBody: "name=Bob page=<nil> score=<nil> active=false",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := mustNew(t)
			app.GET("/test", func(ctx *credo.Context) error {
				var q query
				if err := ctx.Request().BindQuery(&q); err != nil {
					return err
				}
				body := fmt.Sprintf("name=%s page=%s score=%s active=%s",
					fmtPtr(q.Name), fmtPtr(q.Page), fmtPtr(q.Score), fmtPtr(q.Active))
				return ctx.Response().Text(200, body)
			})

			w := httptest.NewRecorder()
			r := httptest.NewRequest("GET", tt.url, nil)
			app.ServeHTTP(w, r)

			if w.Code != tt.wantCode {
				t.Errorf("status = %d, want %d", w.Code, tt.wantCode)
			}
			if w.Body.String() != tt.wantBody {
				t.Errorf("body = %q, want %q", w.Body.String(), tt.wantBody)
			}
		})
	}
}

func TestBindQuery_PointerFieldTypeError(t *testing.T) {
	type query struct {
		Page *int `query:"page"`
	}

	app := mustNew(t)
	app.GET("/test", func(ctx *credo.Context) error {
		var q query
		return ctx.Request().BindQuery(&q)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test?page=abc", nil)
	app.ServeHTTP(w, r)

	if w.Code != 400 {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestBindQuery_PointerEmbeddedStruct(t *testing.T) {
	type Pagination struct {
		Page int    `query:"page"`
		Sort string `query:"sort"`
	}
	type query struct {
		*Pagination
		Name string `query:"name"`
	}

	tests := []struct {
		name     string
		url      string
		wantCode int
		wantBody string
	}{
		{
			name:     "embedded fields present",
			url:      "/test?name=Alice&page=3&sort=asc",
			wantCode: 200,
			wantBody: "name=Alice page=3 sort=asc",
		},
		{
			name:     "only outer field",
			url:      "/test?name=Bob",
			wantCode: 200,
			wantBody: "name=Bob page=0 sort=",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := mustNew(t)
			app.GET("/test", func(ctx *credo.Context) error {
				var q query
				if err := ctx.Request().BindQuery(&q); err != nil {
					return err
				}
				page, sort := 0, ""
				if q.Pagination != nil {
					page = q.Pagination.Page
					sort = q.Pagination.Sort
				}
				return ctx.Response().Text(200, fmt.Sprintf("name=%s page=%d sort=%s", q.Name, page, sort))
			})

			w := httptest.NewRecorder()
			r := httptest.NewRequest("GET", tt.url, nil)
			app.ServeHTTP(w, r)

			if w.Code != tt.wantCode {
				t.Errorf("status = %d, want %d", w.Code, tt.wantCode)
			}
			if w.Body.String() != tt.wantBody {
				t.Errorf("body = %q, want %q", w.Body.String(), tt.wantBody)
			}
		})
	}
}

func TestBindBody_FormURLEncoded_PointerFields(t *testing.T) {
	type form struct {
		Name   *string `form:"name"`
		Age    *int    `form:"age"`
		Active *bool   `form:"active"`
	}

	app := mustNew(t)
	app.POST("/test", func(ctx *credo.Context) error {
		var f form
		if err := ctx.Request().BindBody(&f); err != nil {
			return err
		}
		return ctx.Response().Text(200, fmt.Sprintf("name=%s age=%s active=%s",
			fmtPtr(f.Name), fmtPtr(f.Age), fmtPtr(f.Active)))
	})

	// All present
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/test", strings.NewReader("name=Alice&age=30&active=true"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	app.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
	want := "name=Alice age=30 active=true"
	if w.Body.String() != want {
		t.Errorf("body = %q, want %q", w.Body.String(), want)
	}

	// Partial (only name)
	w = httptest.NewRecorder()
	r = httptest.NewRequest("POST", "/test", strings.NewReader("name=Bob"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	app.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Errorf("partial: status = %d, want 200", w.Code)
	}
	want = "name=Bob age=<nil> active=<nil>"
	if w.Body.String() != want {
		t.Errorf("partial: body = %q, want %q", w.Body.String(), want)
	}
}

func TestBindQuery_PointerNarrowInt(t *testing.T) {
	type query struct {
		Val *int8 `query:"val"`
	}

	tests := []struct {
		name     string
		url      string
		wantCode int
	}{
		{"valid", "/test?val=127", 200},
		{"overflow", "/test?val=200", 400},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := mustNew(t)
			app.GET("/test", func(ctx *credo.Context) error {
				var q query
				if err := ctx.Request().BindQuery(&q); err != nil {
					return err
				}
				return ctx.Response().Text(200, fmtPtr(q.Val))
			})

			w := httptest.NewRecorder()
			r := httptest.NewRequest("GET", tt.url, nil)
			app.ServeHTTP(w, r)

			if w.Code != tt.wantCode {
				t.Errorf("status = %d, want %d", w.Code, tt.wantCode)
			}
		})
	}
}

func TestLogger_FallsBackToAppLogger(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	app, err := credo.New(credo.WithLogger(logger))
	if err != nil {
		t.Fatal(err)
	}

	var captured *slog.Logger
	app.GET("/test", func(ctx *credo.Context) error {
		captured = ctx.Logger()
		return ctx.Response().NoContent(204)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test", nil)
	app.ServeHTTP(w, r)

	if captured == nil {
		t.Fatal("Logger() returned nil")
	}
	// Verify it's the app logger by writing to it and checking the buffer.
	captured.Info("test message")
	if !strings.Contains(buf.String(), "test message") {
		t.Errorf("expected app logger to receive message, got: %q", buf.String())
	}
}

// --- OriginalPath tests ---

func TestOriginalPath_NoRewrite(t *testing.T) {
	app := mustNew(t)
	var got string
	app.GET("/test", func(c *credo.Context) error {
		got = c.OriginalPath()
		return c.Response().Text(200, "ok")
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test", nil)
	app.ServeHTTP(w, r)

	if got != "/test" {
		t.Errorf("OriginalPath() = %q, want %q", got, "/test")
	}
}

func TestOriginalPath_PreservedAfterURLMutation(t *testing.T) {
	app := mustNew(t)
	var original, current string
	app.GET("/new-path", func(c *credo.Context) error {
		original = c.OriginalPath()
		current = c.Request().URL.Path
		return c.Response().Text(200, "ok")
	})

	// Simulate a middleware that mutates URL.Path before dispatch
	app.GlobalMiddleware(func(next credo.Handler) credo.Handler {
		return func(c *credo.Context) error {
			c.Request().URL.Path = "/new-path"
			return next(c)
		}
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/original-path", nil)
	app.ServeHTTP(w, r)

	if original != "/original-path" {
		t.Errorf("OriginalPath() = %q, want %q", original, "/original-path")
	}
	if current != "/new-path" {
		t.Errorf("URL.Path = %q, want %q", current, "/new-path")
	}
}

// --- Rewrite validation tests ---

func TestRewrite_Validation(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		wantErr string
	}{
		{"empty path", "", "must start with '/'"},
		{"no leading slash", "no-slash", "must start with '/'"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := mustNew(t)
			var rewriteErr error
			app.GET("/trigger", func(c *credo.Context) error {
				rewriteErr = c.Rewrite(tt.path)
				// Don't return the rewrite error — return nil so we can inspect it.
				return c.Response().Text(200, "ok")
			})

			w := httptest.NewRecorder()
			r := httptest.NewRequest("GET", "/trigger", nil)
			app.ServeHTTP(w, r)

			if rewriteErr == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(rewriteErr.Error(), tt.wantErr) {
				t.Errorf("error = %q, want to contain %q", rewriteErr.Error(), tt.wantErr)
			}
		})
	}
}

func TestRewrite_CommittedGuard(t *testing.T) {
	app := mustNew(t)
	var rewriteErr error
	app.GET("/trigger", func(c *credo.Context) error {
		// Write response first (commits it)
		_ = c.Response().Text(200, "already committed")
		// Now try to rewrite
		rewriteErr = c.Rewrite("/target")
		return nil
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/trigger", nil)
	app.ServeHTTP(w, r)

	if rewriteErr == nil {
		t.Fatal("expected error for committed response, got nil")
	}
	if !strings.Contains(rewriteErr.Error(), "committed") {
		t.Errorf("error = %q, want to contain 'committed'", rewriteErr.Error())
	}
}

// --- Debug-mode bind warning tests ---

func TestBindBody_NoValidatable_DebugWarning(t *testing.T) {
	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	app, err := credo.New(credo.WithLogger(logger), credo.WithDebug())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	type plain struct {
		Name string `json:"name"`
	}
	app.POST("/test", func(ctx *credo.Context) error {
		var p plain
		if err := ctx.Request().BindBody(&p); err != nil {
			return err
		}
		return ctx.Response().JSON(200, p)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/test", strings.NewReader(`{"name":"Alice"}`))
	app.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	output := buf.String()
	if !strings.Contains(output, "does not implement Validatable") {
		t.Errorf("expected debug warning in log, got: %s", output)
	}
	if !strings.Contains(output, "credo_test.plain") {
		t.Errorf("expected type name in warning, got: %s", output)
	}
}

func TestBindBody_NoValidatable_NoDebug_NoWarning(t *testing.T) {
	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	app, err := credo.New(credo.WithLogger(logger)) // no WithDebug
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	type plain struct {
		Name string `json:"name"`
	}
	app.POST("/test", func(ctx *credo.Context) error {
		var p plain
		if err := ctx.Request().BindBody(&p); err != nil {
			return err
		}
		return ctx.Response().JSON(200, p)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/test", strings.NewReader(`{"name":"Alice"}`))
	app.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	output := buf.String()
	if strings.Contains(output, "does not implement Validatable") {
		t.Errorf("unexpected debug warning without WithDebug: %s", output)
	}
}

func TestBindQuery_NoValidatable_DebugWarning(t *testing.T) {
	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	app, err := credo.New(credo.WithLogger(logger), credo.WithDebug())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	type filter struct {
		Page int `query:"page"`
	}
	app.GET("/test", func(ctx *credo.Context) error {
		var f filter
		if err := ctx.Request().BindQuery(&f); err != nil {
			return err
		}
		return ctx.Response().JSON(200, f)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test?page=1", nil)
	app.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	output := buf.String()
	if !strings.Contains(output, "does not implement Validatable") {
		t.Errorf("expected debug warning in log, got: %s", output)
	}
}

func TestBindBody_NoValidatable_ServerDebugConfig_Warning(t *testing.T) {
	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	app, err := credo.New(
		credo.WithLogger(logger),
		credo.WithRawConfig(newServerConfigRC(map[string]any{"debug": true})),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	type plain struct {
		Name string `json:"name"`
	}
	app.POST("/test", func(ctx *credo.Context) error {
		var p plain
		if err := ctx.Request().BindBody(&p); err != nil {
			return err
		}
		return ctx.Response().JSON(200, p)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/test", strings.NewReader(`{"name":"Alice"}`))
	app.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	output := buf.String()
	if !strings.Contains(output, "does not implement Validatable") {
		t.Errorf("expected debug warning from server.debug config, got: %s", output)
	}
}
