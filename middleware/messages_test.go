package middleware_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/middleware"
	"github.com/credo-go/credo/testutil"
)

func errorBody(t *testing.T, w *httptest.ResponseRecorder) credo.ErrorBody {
	t.Helper()
	var env credo.ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal %q: %v", w.Body.String(), err)
	}
	return env.Error
}

// TestContractGuard_RejectionMessages: each rejection renders its code's
// built-in message, and the value — the rejected content type, which is
// client input, or the header, query parameter or scope the route requires —
// is in the details, never in the message.
func TestContractGuard_RejectionMessages(t *testing.T) {
	cases := []struct {
		name    string
		cfg     middleware.ContractConfig
		set     func(*credo.Route)
		prep    func(*http.Request)
		status  int
		code    string
		message string
		details map[string]any
	}{
		{"content type required", middleware.ContractConfig{},
			func(r *credo.Route) { r.SetMeta(middleware.MetaAccept, []string{"application/json"}) },
			func(r *http.Request) { r.Header.Del("Content-Type") },
			415, "content_type_required", "Content-Type is required.", nil},
		{"content type not accepted", middleware.ContractConfig{},
			func(r *credo.Route) { r.SetMeta(middleware.MetaAccept, []string{"application/json"}) },
			func(r *http.Request) { r.Header.Set("Content-Type", "text/x-evil") },
			415, "content_type_not_accepted", "Content-Type is not accepted.", map[string]any{"content_type": "text/x-evil"}},
		{"missing header", middleware.ContractConfig{},
			func(r *credo.Route) { r.SetMeta(middleware.MetaRequireHeaders, []string{"X-Tenant"}) },
			func(*http.Request) {},
			400, "missing_required_header", "A required header is missing.", map[string]any{"header": "X-Tenant"}},
		{"missing query parameter", middleware.ContractConfig{},
			func(r *credo.Route) { r.SetMeta(middleware.MetaRequireQuery, []string{"page"}) },
			func(*http.Request) {},
			400, "missing_required_query_parameter", "A required query parameter is missing.", map[string]any{"query": "page"}},
		{"api version", middleware.ContractConfig{},
			func(r *credo.Route) { r.SetMeta(middleware.MetaAPIVersion, []string{"2"}) },
			func(r *http.Request) { r.Header.Set("X-API-Version", "9") },
			400, "unsupported_api_version", "Unsupported or missing API version.", nil},
		{"missing scope", middleware.ContractConfig{ScopeChecker: func(*credo.Context, string) bool { return false }},
			func(r *credo.Route) { r.SetMeta(middleware.MetaScope, []string{"reports:read"}) },
			func(*http.Request) {},
			403, "missing_required_scope", "A required scope is missing.", map[string]any{"scope": "reports:read"}},
		{"body over its limit", middleware.ContractConfig{},
			func(r *credo.Route) { r.SetMeta(middleware.MetaMaxBody, int64(1)) },
			func(*http.Request) {},
			413, "request_entity_too_large", "Request Entity Too Large", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app, g := contractGroup(t, tc.cfg)
			tc.set(g.POST("/x", contractOK))
			r := httptest.NewRequest(http.MethodPost, "/g/x", strings.NewReader("{}"))
			r.Header.Set("Content-Type", "application/json")
			tc.prep(r)
			w := httptest.NewRecorder()
			app.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status = %d, want %d; body %s", w.Code, tc.status, w.Body.String())
			}
			body := errorBody(t, w)
			if body.Code != tc.code || body.Message != tc.message {
				t.Fatalf("body = %+v, want code %q, message %q", body, tc.code, tc.message)
			}
			var details map[string]any
			if body.Details != nil {
				details, _ = body.Details.(map[string]any)
			}
			if !reflect.DeepEqual(details, tc.details) {
				t.Fatalf("details = %#v, want %#v", body.Details, tc.details)
			}
		})
	}
}

// TestTimeout_CodeThroughResolveMessageKey: the timeout carries no explicit
// key, so I18nConfig.ResolveMessageKey maps its code as it maps every other.
func TestTimeout_CodeThroughResolveMessageKey(t *testing.T) {
	app := mustNew(t)
	var refs []credo.MessageRef
	app.UseI18n(credo.I18nConfig{
		Default:  "en",
		Messages: credo.I18nMessages{"problem.request_timeout": "Took too long"},
		ResolveMessageKey: func(ref credo.MessageRef) string {
			refs = append(refs, ref)
			return "problem." + ref.Code
		},
	})
	app.GET("/", func(c *credo.Context) error {
		<-c.Request().Context().Done()
		return c.Request().Context().Err()
	}).Middleware(middleware.Timeout(middleware.TimeoutConfig{Timeout: 10 * time.Millisecond}))
	testutil.Start(t, app)

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", w.Code)
	}
	if body := errorBody(t, w); body.Code != "request_timeout" || body.Message != "Took too long" {
		t.Fatalf("body = %+v", body)
	}
	if len(refs) != 1 || refs[0] != (credo.MessageRef{Scope: credo.MessageScopeError, Code: "request_timeout"}) {
		t.Fatalf("resolver refs = %+v", refs)
	}
}
