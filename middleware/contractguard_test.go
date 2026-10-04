package middleware_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/middleware"
)

func contractOK(ctx *credo.Context) error {
	return ctx.Response().Text(http.StatusOK, "ok")
}

// contractGroup builds an app with a "/g" group guarded by ContractGuard.
// ContractGuard reads matched-route metadata, so it is registered at the group
// level (group/route middleware run after the route match; app-global
// middleware runs before it).
func contractGroup(t *testing.T, cfg ...middleware.ContractConfig) (*credo.App, *credo.Group) {
	t.Helper()
	app := mustNew(t)
	g := app.Group("/g")
	g.Middleware(middleware.ContractGuard(cfg...))
	return app, g
}

func TestContractGuard_Accept(t *testing.T) {
	tests := []struct {
		name        string
		accept      any
		contentType string
		want        int
	}{
		{"exact match", "application/json", "application/json", http.StatusOK},
		{"match ignores params", "application/json", "application/json; charset=utf-8", http.StatusOK},
		{"mismatch", "application/json", "text/plain", http.StatusUnsupportedMediaType},
		{"subtype wildcard", "image/*", "image/png", http.StatusOK},
		{"wildcard all", "*/*", "application/x-thing", http.StatusOK},
		{"empty content type passes", "application/json", "", http.StatusOK},
		{"slice match", []string{"application/json", "application/xml"}, "application/xml", http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app, g := contractGroup(t)
			g.POST("/x", contractOK).SetMeta(middleware.MetaAccept, tt.accept)

			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/g/x", strings.NewReader("{}"))
			if tt.contentType != "" {
				r.Header.Set("Content-Type", tt.contentType)
			}
			app.ServeHTTP(w, r)

			if w.Code != tt.want {
				t.Fatalf("status = %d, want %d", w.Code, tt.want)
			}
		})
	}
}

func TestContractGuard_MaxBody(t *testing.T) {
	t.Run("oversize content-length rejected eagerly", func(t *testing.T) {
		app, g := contractGroup(t)
		g.POST("/x", contractOK).SetMeta(middleware.MetaMaxBody, int64(10))

		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/g/x", strings.NewReader(strings.Repeat("x", 50)))
		app.ServeHTTP(w, r)

		if w.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413", w.Code)
		}
	})

	t.Run("int value coerced", func(t *testing.T) {
		app, g := contractGroup(t)
		g.POST("/x", contractOK).SetMeta(middleware.MetaMaxBody, 10) // int, not int64

		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/g/x", strings.NewReader(strings.Repeat("x", 50)))
		app.ServeHTTP(w, r)

		if w.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413", w.Code)
		}
	})

	t.Run("within limit passes (body readable)", func(t *testing.T) {
		app, g := contractGroup(t)
		g.POST("/x", func(ctx *credo.Context) error {
			if _, err := io.ReadAll(ctx.Request().Body); err != nil {
				return err
			}
			return ctx.Response().Text(http.StatusOK, "ok")
		}).SetMeta(middleware.MetaMaxBody, int64(64))

		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/g/x", strings.NewReader("small body"))
		app.ServeHTTP(w, r)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
	})

	t.Run("negative disables the per-route cap", func(t *testing.T) {
		app, g := contractGroup(t)
		g.POST("/x", contractOK).SetMeta(middleware.MetaMaxBody, int64(-1))

		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/g/x", strings.NewReader(strings.Repeat("x", 5000)))
		app.ServeHTTP(w, r)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
	})
}

func TestContractGuard_RequireHeaders(t *testing.T) {
	app, g := contractGroup(t)
	g.GET("/x", contractOK).SetMeta(middleware.MetaRequireHeaders, []string{"X-Tenant-Id"})

	t.Run("missing header rejected", func(t *testing.T) {
		w := httptest.NewRecorder()
		app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/g/x", nil))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", w.Code)
		}
	})

	t.Run("present header passes", func(t *testing.T) {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/g/x", nil)
		r.Header.Set("X-Tenant-Id", "acme")
		app.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
	})
}

func TestContractGuard_RequireQuery(t *testing.T) {
	app, g := contractGroup(t)
	g.GET("/list", contractOK).SetMeta(middleware.MetaRequireQuery, "page")

	t.Run("missing query rejected", func(t *testing.T) {
		w := httptest.NewRecorder()
		app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/g/list", nil))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", w.Code)
		}
	})

	t.Run("present query passes", func(t *testing.T) {
		w := httptest.NewRecorder()
		app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/g/list?page=1", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
	})
}

func TestContractGuard_APIVersion(t *testing.T) {
	app, g := contractGroup(t)
	g.GET("/data", contractOK).SetMeta(middleware.MetaAPIVersion, []string{"1", "2"})
	g.GET("/v{version}/data", contractOK).SetMeta(middleware.MetaAPIVersion, []string{"1"})

	tests := []struct {
		name   string
		path   string
		header string
		want   int
	}{
		{"header allowed", "/g/data", "2", http.StatusOK},
		{"header not allowed", "/g/data", "3", http.StatusBadRequest},
		{"header missing", "/g/data", "", http.StatusBadRequest},
		{"path param allowed", "/g/v1/data", "", http.StatusOK},
		{"path param not allowed", "/g/v9/data", "", http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, tt.path, nil)
			if tt.header != "" {
				r.Header.Set("X-API-Version", tt.header)
			}
			app.ServeHTTP(w, r)
			if w.Code != tt.want {
				t.Fatalf("status = %d, want %d", w.Code, tt.want)
			}
		})
	}
}

func TestContractGuard_Scope(t *testing.T) {
	t.Run("no checker denies", func(t *testing.T) {
		app, g := contractGroup(t)
		g.GET("/x", contractOK).SetMeta(middleware.MetaScope, "admin")

		w := httptest.NewRecorder()
		app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/g/x", nil))
		if w.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", w.Code)
		}
	})

	t.Run("checker allows", func(t *testing.T) {
		app, g := contractGroup(t, middleware.ContractConfig{
			ScopeChecker: func(_ *credo.Context, s string) bool { return s == "admin" },
		})
		g.GET("/x", contractOK).SetMeta(middleware.MetaScope, "admin")

		w := httptest.NewRecorder()
		app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/g/x", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
	})

	t.Run("checker denies", func(t *testing.T) {
		app, g := contractGroup(t, middleware.ContractConfig{
			ScopeChecker: func(_ *credo.Context, _ string) bool { return false },
		})
		g.GET("/x", contractOK).SetMeta(middleware.MetaScope, []string{"a", "b"})

		w := httptest.NewRecorder()
		app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/g/x", nil))
		if w.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", w.Code)
		}
	})
}

func TestContractGuard_CustomCheck(t *testing.T) {
	app, g := contractGroup(t, middleware.ContractConfig{
		CustomChecks: []func(*credo.Context) error{
			func(ctx *credo.Context) error {
				if ctx.Request().Header.Get("X-Block") != "" {
					return credo.NewHTTPError(http.StatusTeapot).
						WithMessageKey("blocked by custom check")
				}
				return nil
			},
		},
	})
	g.GET("/x", contractOK)

	t.Run("custom check rejects", func(t *testing.T) {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/g/x", nil)
		r.Header.Set("X-Block", "1")
		app.ServeHTTP(w, r)
		if w.Code != http.StatusTeapot {
			t.Fatalf("status = %d, want 418", w.Code)
		}
	})

	t.Run("custom check passes", func(t *testing.T) {
		w := httptest.NewRecorder()
		app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/g/x", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
	})
}

func TestContractGuard_GroupMetaInheritance(t *testing.T) {
	app, g := contractGroup(t)
	g.SetMeta(middleware.MetaAccept, "application/json") // group-wide contract
	g.POST("/users", contractOK)

	t.Run("inherited contract enforced", func(t *testing.T) {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/g/users", strings.NewReader("{}"))
		r.Header.Set("Content-Type", "text/plain")
		app.ServeHTTP(w, r)
		if w.Code != http.StatusUnsupportedMediaType {
			t.Fatalf("status = %d, want 415", w.Code)
		}
	})

	t.Run("inherited contract satisfied", func(t *testing.T) {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/g/users", strings.NewReader("{}"))
		r.Header.Set("Content-Type", "application/json")
		app.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
	})
}

func TestContractGuard_NoMetaPasses(t *testing.T) {
	app, g := contractGroup(t)
	g.GET("/x", contractOK)

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/g/x", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
}

func TestContractGuard_Skipper(t *testing.T) {
	app, g := contractGroup(t, middleware.ContractConfig{
		Skipper: func(_ *credo.Context) bool { return true },
	})
	g.POST("/x", contractOK).SetMeta(middleware.MetaAccept, "application/json")

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/g/x", strings.NewReader("{}"))
	r.Header.Set("Content-Type", "text/plain") // would be 415 if not skipped
	app.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
}

// TestContractGuard_RequireContentType covers the body-present signal for the
// RequireContentType switch: Content-Length > 0 or unknown (-1, chunked /
// HTTP/2) means "has body"; 0 or no body passes; a present-but-empty header
// counts as absent; the switch only arms a declared MetaAccept contract.
func TestContractGuard_RequireContentType(t *testing.T) {
	type tc struct {
		name    string
		require bool
		method  string
		body    io.Reader
		chunked bool
		header  *string // nil = no header; "" = present but empty
		accept  bool    // route declares MetaAccept
		want    int
	}
	empty := ""
	tests := []tc{
		{"POST body no header", true, http.MethodPost, strings.NewReader("{}"), false, nil, true, http.StatusUnsupportedMediaType},
		{"POST NoBody no header", true, http.MethodPost, http.NoBody, false, nil, true, http.StatusOK},
		{"GET no header", true, http.MethodGet, nil, false, nil, true, http.StatusOK},
		{"chunked POST no header", true, http.MethodPost, strings.NewReader("{}"), true, nil, true, http.StatusUnsupportedMediaType},
		{"POST body empty header", true, http.MethodPost, strings.NewReader("{}"), false, &empty, true, http.StatusUnsupportedMediaType},
		{"no MetaAccept contract", true, http.MethodPost, strings.NewReader("{}"), false, nil, false, http.StatusOK},
		{"default off: POST body no header", false, http.MethodPost, strings.NewReader("{}"), false, nil, true, http.StatusOK},
		{"default off: chunked no header", false, http.MethodPost, strings.NewReader("{}"), true, nil, true, http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := middleware.DefaultContractConfig()
			cfg.RequireContentType = tt.require
			app, g := contractGroup(t, cfg)
			var route *credo.Route
			if tt.method == http.MethodGet {
				route = g.GET("/x", contractOK)
			} else {
				route = g.POST("/x", contractOK)
			}
			if tt.accept {
				route.SetMeta(middleware.MetaAccept, "application/json")
			}

			w := httptest.NewRecorder()
			r := httptest.NewRequest(tt.method, "/g/x", tt.body)
			if tt.chunked {
				r.ContentLength = -1
				r.TransferEncoding = []string{"chunked"}
			}
			if tt.header != nil {
				r.Header.Set("Content-Type", *tt.header)
			}
			app.ServeHTTP(w, r)

			if w.Code != tt.want {
				t.Fatalf("status = %d, want %d (body %q)", w.Code, tt.want, w.Body.String())
			}
		})
	}
}

// Named types are what an application reaches for when it models scopes as
// constants. None of them is in the guard's accepted set.
type (
	scopeName  string
	scopeNames []string
)

// errorRecords returns the Error-level records written to a JSON log buffer.
func errorRecords(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	sc := bufio.NewScanner(bytes.NewReader(buf.Bytes()))
	for sc.Scan() {
		var rec map[string]any
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			t.Fatalf("log line is not JSON: %v\n%s", err, sc.Bytes())
		}
		if rec["level"] == "ERROR" {
			out = append(out, rec)
		}
	}
	return out
}

// TestContractGuard_UnrecognizedMetaFailsClosed pins the fail-closed rule: a
// declared contract whose value the guard cannot read rejects the request
// instead of being skipped. The request carries no header, query parameter or
// body and the scope checker denies everything, so the handler could only be
// reached through a skipped contract.
func TestContractGuard_UnrecognizedMetaFailsClosed(t *testing.T) {
	type value struct {
		name   string
		value  any
		logged string // how the logged cause names the value
	}
	listValues := []value{
		{"named string", scopeName("admin"), "type middleware_test.scopeName"},
		{"named string slice", scopeNames{"admin"}, "type middleware_test.scopeNames"},
		{"slice of named strings", []scopeName{"admin"}, "type []middleware_test.scopeName"},
		{"int", 1, "type int"},
		{"struct", struct{ Name string }{"admin"}, "type struct { Name string }"},
		{"any slice with a non-string element", []any{"admin", 1}, "type []interface {}"},
		{"untyped nil", nil, "is nil"},
	}
	sizeValues := []value{
		{"uint", uint(10), "type uint"},
		{"float64", float64(10), "type float64"},
		{"string", "1MB", "type string"},
		{"untyped nil", nil, "is nil"},
	}
	contracts := []struct {
		key    string
		values []value
	}{
		{middleware.MetaAccept, listValues},
		{middleware.MetaRequireHeaders, listValues},
		{middleware.MetaRequireQuery, listValues},
		{middleware.MetaAPIVersion, listValues},
		{middleware.MetaScope, listValues},
		{middleware.MetaMaxBody, sizeValues},
	}
	for _, c := range contracts {
		for _, v := range c.values {
			t.Run(c.key+"/"+v.name, func(t *testing.T) {
				logger, logs := newTestLogger(t)
				app := mustNew(t, credo.WithLogger(logger))
				g := app.Group("/g")
				g.Middleware(middleware.ContractGuard(middleware.ContractConfig{
					ScopeChecker: func(*credo.Context, string) bool { return false },
				}))
				called := false
				g.POST("/x", func(ctx *credo.Context) error {
					called = true
					return contractOK(ctx)
				}).SetMeta(c.key, v.value)

				w := httptest.NewRecorder()
				app.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/g/x", nil))

				if w.Code != http.StatusInternalServerError {
					t.Fatalf("status = %d, want 500 (body %q)", w.Code, w.Body.String())
				}
				if called {
					t.Fatal("handler ran behind an unrecognized contract value")
				}
				var body struct {
					Success bool `json:"success"`
					Error   struct {
						Code    string `json:"code"`
						Message string `json:"message"`
					} `json:"error"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
					t.Fatalf("body is not JSON: %v", err)
				}
				if body.Success || body.Error.Code != "internal_server_error" ||
					body.Error.Message != "Internal Server Error" {
					t.Fatalf("body = %s, want the generic 500 envelope", w.Body.String())
				}
				// The response says nothing about the route's contracts.
				if got := w.Body.String(); strings.Contains(got, c.key) || strings.Contains(got, "contract") {
					t.Fatalf("body %q leaks the contract", got)
				}

				recs := errorRecords(t, logs)
				if len(recs) != 1 {
					t.Fatalf("got %d error records, want 1:\n%s", len(recs), logs.String())
				}
				cause, _ := recs[0]["error"].(string)
				for _, want := range []string{"contractguard", "POST /g/x", `"` + c.key + `"`, v.logged} {
					if !strings.Contains(cause, want) {
						t.Errorf("logged cause %q does not name %q", cause, want)
					}
				}
			})
		}
	}
}

// TestContractGuard_LiftingAnInheritedScope: a route lifts a group's scope
// requirement with an explicit empty list. A nil value is not a contract
// value — it used to switch the inherited requirement off silently.
func TestContractGuard_LiftingAnInheritedScope(t *testing.T) {
	tests := []struct {
		name string
		set  func(*credo.Route)
		want int
	}{
		{"inherited requirement applies", func(*credo.Route) {}, http.StatusForbidden},
		{"empty list lifts it", func(r *credo.Route) {
			r.SetMeta(middleware.MetaScope, []string{})
		}, http.StatusOK},
		{"nil does not", func(r *credo.Route) {
			r.SetMeta(middleware.MetaScope, nil)
		}, http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app, g := contractGroup(t, middleware.ContractConfig{
				ScopeChecker: func(*credo.Context, string) bool { return false },
			})
			g.SetMeta(middleware.MetaScope, "admin")
			tt.set(g.GET("/x", contractOK))

			w := httptest.NewRecorder()
			app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/g/x", nil))
			if w.Code != tt.want {
				t.Fatalf("status = %d, want %d", w.Code, tt.want)
			}
		})
	}
}

// TestContractGuard_RecognizedMetaTypes covers the accepted shapes the other
// tests do not: a []any of strings is read like a []string, and an int32 byte
// count like an int64.
func TestContractGuard_RecognizedMetaTypes(t *testing.T) {
	t.Run("any slice of strings", func(t *testing.T) {
		var asked []string
		app, g := contractGroup(t, middleware.ContractConfig{
			ScopeChecker: func(_ *credo.Context, s string) bool {
				asked = append(asked, s)
				return true
			},
		})
		g.GET("/x", contractOK).SetMeta(middleware.MetaScope, []any{"read", "write"})

		w := httptest.NewRecorder()
		app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/g/x", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		if want := []string{"read", "write"}; !slices.Equal(asked, want) {
			t.Fatalf("checked scopes = %v, want %v", asked, want)
		}
	})

	t.Run("int32 body cap", func(t *testing.T) {
		app, g := contractGroup(t)
		g.POST("/x", contractOK).SetMeta(middleware.MetaMaxBody, int32(10))

		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/g/x", strings.NewReader(strings.Repeat("x", 50)))
		app.ServeHTTP(w, r)
		if w.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413", w.Code)
		}
	})
}
