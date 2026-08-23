package credo_test

import (
	jsonv1 "encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/validation"
)

// profileFixture exercises every axis of the JSON output profile in one
// payload: map ordering, nil slice/map rendering, omitempty semantics,
// []byte encoding, Duration and Time representation, and HTML escaping.
type profileFixture struct {
	Scores   map[string]int `json:"scores"`
	NilSlice []string       `json:"nil_slice"`
	NilMap   map[string]int `json:"nil_map"`
	ZeroInt  int            `json:"zero_int,omitempty"`
	EmptyStr string         `json:"empty_str,omitempty"`
	Blob     []byte         `json:"blob"`
	Timeout  time.Duration  `json:"timeout"`
	At       time.Time      `json:"at"`
	HTML     string         `json:"html"`
}

func newProfileFixture() profileFixture {
	return profileFixture{
		Scores:  map[string]int{"zeta": 3, "alpha": 1, "mu": 2},
		Blob:    []byte{1, 2, 3},
		Timeout: 5 * time.Second,
		At:      time.Date(2026, 8, 23, 10, 30, 0, 0, time.UTC),
		HTML:    "a<b>&c",
	}
}

// serveJSON runs one request through an app whose handler writes v with
// Response.JSON, returning the exact response body.
func serveJSON(t *testing.T, v any, opts ...credo.Option) string {
	t.Helper()
	app := mustNew(t, opts...)
	app.GET("/j", func(ctx *credo.Context) error {
		return ctx.Response().JSON(200, v)
	})
	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/j", nil))
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200 (body %q)", w.Code, w.Body.String())
	}
	return w.Body.String()
}

// TestResponseJSON_Profile is the golden-byte contract for the default
// encoding profile. Every element of the expected string is a deliberate
// decision documented on defaultJSONOptions.
func TestResponseJSON_Profile(t *testing.T) {
	got := serveJSON(t, newProfileFixture())

	want := `{"scores":{"alpha":1,"mu":2,"zeta":3},` + // Deterministic: sorted keys
		`"nil_slice":[],"nil_map":{},` + // v2: not null
		`"zero_int":0,` + // v2 omitempty: numbers are never JSON-empty
		// "empty_str" dropped: "" is JSON-empty
		`"blob":"AQID",` + // base64
		`"timeout":5000000000,` + // FormatDurationAsNano
		`"at":"2026-08-23T10:30:00Z",` + // RFC 3339
		`"html":"a<b>&c"}` // no HTML escaping

	if got != want {
		t.Errorf("Response.JSON bytes:\n got: %s\nwant: %s", got, want)
	}
	if strings.HasSuffix(got, "\n") {
		t.Error("Response.JSON must not append a trailing newline (that was an encoding/json v1 Encoder artefact)")
	}
}

// TestResponseJSON_ProfileOverrides proves WithJSONOptions overrides a single
// axis while the rest of the profile stays in force.
func TestResponseJSON_ProfileOverrides(t *testing.T) {
	tests := []struct {
		name    string
		opt     credo.Option
		want    string
		notWant string
	}{
		{
			name: "nil slice as null",
			opt:  credo.WithJSONOptions(jsonv2.FormatNilSliceAsNull(true)),
			want: `"nil_slice":null`,
		},
		{
			name: "HTML escaping",
			opt:  credo.WithJSONOptions(jsontext.EscapeForHTML(true)),
			want: `"html":"a\u003cb\u003e\u0026c"`,
		},
		{
			name:    "legacy omitempty drops the zero int",
			opt:     credo.WithJSONOptions(jsonv1.OmitEmptyWithLegacySemantics(true)),
			notWant: `"zero_int"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := serveJSON(t, newProfileFixture(), tt.opt)
			if tt.want != "" && !strings.Contains(got, tt.want) {
				t.Errorf("body missing %s:\n%s", tt.want, got)
			}
			if tt.notWant != "" && strings.Contains(got, tt.notWant) {
				t.Errorf("body should not contain %s:\n%s", tt.notWant, got)
			}
			// Untouched axes keep the framework profile.
			if !strings.Contains(got, `"scores":{"alpha":1,"mu":2,"zeta":3}`) {
				t.Errorf("deterministic map ordering lost:\n%s", got)
			}
			if !strings.Contains(got, `"timeout":5000000000`) {
				t.Errorf("nanosecond durations lost:\n%s", got)
			}
		})
	}
}

// TestResponseJSON_LegacyMode proves the escape hatch is a true legacy mode:
// with DefaultOptionsV1 the body matches what encoding/json v1 produces,
// except for the trailing newline its Encoder appended and json/v2 never
// writes. The expectation is computed with v1 rather than hard-coded.
func TestResponseJSON_LegacyMode(t *testing.T) {
	fixture := newProfileFixture()
	legacy, err := jsonv1.Marshal(fixture)
	if err != nil {
		t.Fatalf("v1 marshal: %v", err)
	}
	got := serveJSON(t, fixture, credo.WithJSONOptions(jsonv1.DefaultOptionsV1()))
	if got != string(legacy) {
		t.Errorf("legacy mode differs from encoding/json v1:\n got: %s\nwant: %s", got, legacy)
	}
}

// TestProblemDetails_AlwaysDeterministic locks the error-body contract: RFC
// 7807 responses sort map keys — here a validation error's params — even when
// the application profile disabled deterministic encoding, because clients
// and tests treat those bytes as a framework contract.
func TestProblemDetails_AlwaysDeterministic(t *testing.T) {
	app := mustNew(t, credo.WithJSONOptions(jsonv2.Deterministic(false)))
	app.GET("/boom", func(*credo.Context) error {
		return validation.Errors{{
			Field:   "age",
			Code:    "between",
			Message: "must be between 1 and 3",
			Params:  map[string]any{"zeta": 3, "alpha": 1, "mu": 2},
		}}
	})

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/boom", nil))
	if got := w.Body.String(); !strings.Contains(got, `"alpha":1,"mu":2,"zeta":3`) {
		t.Errorf("problem details must sort map keys regardless of the app profile:\n%s", got)
	}
}

// TestRender_InheritsProfile covers the Render fallback path: with no
// SuccessRenderer installed it delegates to Response.JSON and must therefore
// carry the same profile.
func TestRender_InheritsProfile(t *testing.T) {
	app := mustNew(t)
	app.GET("/r", func(ctx *credo.Context) error {
		return ctx.Render(200, newProfileFixture())
	})
	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/r", nil))

	got := w.Body.String()
	for _, want := range []string{`"scores":{"alpha":1,"mu":2,"zeta":3}`, `"nil_slice":[]`, `"timeout":5000000000`} {
		if !strings.Contains(got, want) {
			t.Errorf("Render fallback missing %s:\n%s", want, got)
		}
	}
}

// TestRender_SuccessRendererOwnsEncoding documents the full-control escape:
// a SuccessRenderer that commits the response itself owns the bytes, so the
// framework profile applies only to bodies the renderer returns.
func TestRender_SuccessRendererOwnsEncoding(t *testing.T) {
	app := mustNew(t)
	app.SetSuccessRenderer(func(c *credo.Context, info credo.RenderInfo) any {
		_ = c.Response().Text(info.Status, "rendered")
		return nil
	})
	app.GET("/r", func(ctx *credo.Context) error {
		return ctx.Render(200, newProfileFixture())
	})
	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/r", nil))

	if got := w.Body.String(); got != "rendered" {
		t.Errorf("body = %q, want the renderer's own output", got)
	}
}

func BenchmarkResponseJSON(b *testing.B) {
	app, err := credo.New()
	if err != nil {
		b.Fatal(err)
	}
	fixture := newProfileFixture()
	app.GET("/j", func(ctx *credo.Context) error {
		return ctx.Response().JSON(200, fixture)
	})
	req := httptest.NewRequest("GET", "/j", nil)

	b.ReportAllocs()
	for b.Loop() {
		w := httptest.NewRecorder()
		app.ServeHTTP(w, req)
	}
}
