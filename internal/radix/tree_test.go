package radix

import (
	"errors"
	"strings"
	"testing"
)

// The tree stores opaque payloads; tests use plain strings as values.
const dummyValue = "dummy"

func newTree() *Node[string] {
	return NewTree[string]()
}

func TestInsertAndFind_Static(t *testing.T) {
	tree := newTree()

	patterns := []string{
		"/",
		"/users",
		"/users/list",
		"/users/create",
		"/articles",
		"/articles/recent",
	}

	for _, p := range patterns {
		_, err := tree.InsertRoute(MGet, p, dummyValue)
		if err != nil {
			t.Fatalf("InsertRoute(%q): %v", p, err)
		}
	}

	for _, p := range patterns {
		rctx := &RouteContext{}
		if _, found := tree.FindRoute(rctx, MGet, p); !found {
			t.Errorf("FindRoute(%q) found no route", p)
		}
	}

	// Non-existent routes
	rctx := &RouteContext{}
	if _, found := tree.FindRoute(rctx, MGet, "/notfound"); found {
		t.Errorf("FindRoute(/notfound) should not match")
	}
}

func TestInsertAndFind_Params(t *testing.T) {
	tree := newTree()

	_, err := tree.InsertRoute(MGet, "/users/{id}", dummyValue)
	if err != nil {
		t.Fatalf("InsertRoute: %v", err)
	}

	_, err = tree.InsertRoute(MGet, "/users/{id}/posts/{postID}", dummyValue)
	if err != nil {
		t.Fatalf("InsertRoute: %v", err)
	}

	tests := []struct {
		path       string
		wantMatch  bool
		wantParams map[string]string
	}{
		{"/users/42", true, map[string]string{"id": "42"}},
		{"/users/abc", true, map[string]string{"id": "abc"}},
		{"/users/42/posts/7", true, map[string]string{"id": "42", "postID": "7"}},
		{"/users/", false, nil},
		{"/users", false, nil},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			rctx := &RouteContext{}
			_, found := tree.FindRoute(rctx, MGet, tt.path)

			if tt.wantMatch && !found {
				t.Fatalf("expected match for %q, got none", tt.path)
			}
			if !tt.wantMatch && found {
				t.Fatalf("expected no match for %q, got one", tt.path)
			}

			if tt.wantParams != nil {
				for key, want := range tt.wantParams {
					got := rctx.URLParam(key)
					if got != want {
						t.Errorf("param %q = %q, want %q", key, got, want)
					}
				}
			}
		})
	}
}

func TestInsertAndFind_Regex(t *testing.T) {
	tree := newTree()

	_, err := tree.InsertRoute(MGet, "/users/{id:[0-9]+}", dummyValue)
	if err != nil {
		t.Fatalf("InsertRoute: %v", err)
	}

	tests := []struct {
		path       string
		wantMatch  bool
		wantParams map[string]string
	}{
		{"/users/42", true, map[string]string{"id": "42"}},
		{"/users/0", true, map[string]string{"id": "0"}},
		{"/users/abc", false, nil},
		{"/users/", false, nil},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			rctx := &RouteContext{}
			_, found := tree.FindRoute(rctx, MGet, tt.path)

			if tt.wantMatch && !found {
				t.Fatalf("expected match for %q", tt.path)
			}
			if !tt.wantMatch && found {
				t.Fatalf("expected no match for %q", tt.path)
			}

			if tt.wantParams != nil {
				for key, want := range tt.wantParams {
					got := rctx.URLParam(key)
					if got != want {
						t.Errorf("param %q = %q, want %q", key, got, want)
					}
				}
			}
		})
	}
}

func TestInsertAndFind_CatchAll(t *testing.T) {
	tree := newTree()

	_, err := tree.InsertRoute(MGet, "/files/{path...}", dummyValue)
	if err != nil {
		t.Fatalf("InsertRoute: %v", err)
	}

	tests := []struct {
		path       string
		wantMatch  bool
		wantParams map[string]string
	}{
		{"/files/a", true, map[string]string{"path": "a"}},
		{"/files/a/b/c", true, map[string]string{"path": "a/b/c"}},
		{"/files/a/b/c.txt", true, map[string]string{"path": "a/b/c.txt"}},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			rctx := &RouteContext{}
			_, found := tree.FindRoute(rctx, MGet, tt.path)

			if tt.wantMatch && !found {
				t.Fatalf("expected match for %q", tt.path)
			}
			if !tt.wantMatch && found {
				t.Fatalf("expected no match for %q", tt.path)
			}

			if tt.wantParams != nil {
				for key, want := range tt.wantParams {
					got := rctx.URLParam(key)
					if got != want {
						t.Errorf("param %q = %q, want %q", key, got, want)
					}
				}
			}
		})
	}
}

func TestInsertAndFind_MethodNotAllowed(t *testing.T) {
	tree := newTree()

	_, err := tree.InsertRoute(MGet, "/users", dummyValue)
	if err != nil {
		t.Fatalf("InsertRoute: %v", err)
	}

	rctx := &RouteContext{}
	if _, found := tree.FindRoute(rctx, MPost, "/users"); found {
		t.Fatal("expected no match for wrong method")
	}
	if !rctx.MethodNotAllowed {
		t.Error("expected MethodNotAllowed=true")
	}
}

func TestInsertAndFind_MultipleMethods(t *testing.T) {
	tree := newTree()

	if _, err := tree.InsertRoute(MGet, "/users", "GET"); err != nil {
		t.Fatalf("InsertRoute GET: %v", err)
	}
	if _, err := tree.InsertRoute(MPost, "/users", "POST"); err != nil {
		t.Fatalf("InsertRoute POST: %v", err)
	}

	rctx := &RouteContext{}
	v, found := tree.FindRoute(rctx, MGet, "/users")
	if !found || v != "GET" {
		t.Fatalf("GET /users = %q (found=%v), want \"GET\"", v, found)
	}

	rctx = &RouteContext{}
	v, found = tree.FindRoute(rctx, MPost, "/users")
	if !found || v != "POST" {
		t.Fatalf("POST /users = %q (found=%v), want \"POST\"", v, found)
	}
}

func TestInsertAndFind_MixedParamAndStatic(t *testing.T) {
	tree := newTree()

	// Static should take priority over param
	_, err := tree.InsertRoute(MGet, "/users/new", "static")
	if err != nil {
		t.Fatalf("InsertRoute: %v", err)
	}
	_, err = tree.InsertRoute(MGet, "/users/{id}", "param")
	if err != nil {
		t.Fatalf("InsertRoute: %v", err)
	}

	// /users/new should match static
	rctx := &RouteContext{}
	v, found := tree.FindRoute(rctx, MGet, "/users/new")
	if !found || v != "static" {
		t.Fatalf("/users/new = %q (found=%v), want \"static\"", v, found)
	}

	// /users/42 should match param
	rctx = &RouteContext{}
	v, found = tree.FindRoute(rctx, MGet, "/users/42")
	if !found || v != "param" {
		t.Fatalf("/users/42 = %q (found=%v), want \"param\"", v, found)
	}
	if rctx.URLParam("id") != "42" {
		t.Errorf("param id = %q, want %q", rctx.URLParam("id"), "42")
	}
}

func TestInsertRoute_Error(t *testing.T) {
	tree := newTree()

	_, err := tree.InsertRoute(MGet, "/users/{}", dummyValue)
	if err == nil {
		t.Fatal("expected error for empty parameter name")
	}

	_, err = tree.InsertRoute(MGet, "/users/{id:[invalid}", dummyValue)
	if err == nil {
		t.Fatal("expected error for invalid regex")
	}
}

func TestInsertRoute_DuplicateMethodPattern(t *testing.T) {
	tree := newTree()

	if _, err := tree.InsertRoute(MGet, "/users", "first"); err != nil {
		t.Fatalf("InsertRoute first: %v", err)
	}

	// A duplicate explicit registration must return a typed *DuplicateRouteError
	// (the mechanism layer), not panic — the framework policy layer (mux) turns
	// it into a fail-loud, two-location panic. The error must carry the prior
	// payload so that policy layer can name the original registration site.
	_, err := tree.InsertRoute(MGet, "/users", "second")
	if err == nil {
		t.Fatal("expected duplicate route error")
	}

	dup, ok := errors.AsType[*DuplicateRouteError[string]](err)
	if !ok {
		t.Fatalf("error = %T (%v), want *DuplicateRouteError[string]", err, err)
	}
	if dup.Method != MGet {
		t.Errorf("Method = %v, want MGet", dup.Method)
	}
	if dup.Pattern != "/users" {
		t.Errorf("Pattern = %q, want %q", dup.Pattern, "/users")
	}
	if dup.ExistingPattern != "/users" {
		t.Errorf("ExistingPattern = %q, want %q", dup.ExistingPattern, "/users")
	}
	if dup.Existing != "first" {
		t.Errorf("Existing = %q, want the prior payload %q", dup.Existing, "first")
	}
}

func TestInsertRoute_ConflictingParamNames(t *testing.T) {
	tree := newTree()

	if _, err := tree.InsertRoute(MGet, "/users/{id}", dummyValue); err != nil {
		t.Fatalf("InsertRoute first: %v", err)
	}
	_, err := tree.InsertRoute(MGet, "/users/{name}", dummyValue)
	if err == nil {
		t.Fatal("expected conflict error for different param names")
	}

	for _, want := range []string{
		`conflicting path parameter "name" with existing "id"`,
		`existing route "/users/{id}"`,
		`new route "/users/{name}"`,
		"dynamic segments at the same path level must use the same parameter name",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want substring %q", err.Error(), want)
		}
	}
}

func TestInsertRoute_ConflictingParamNamesIncludesDescendantRoute(t *testing.T) {
	tree := newTree()

	if _, err := tree.InsertRoute(MGet, "/users/{id}/timeline", dummyValue); err != nil {
		t.Fatalf("InsertRoute first: %v", err)
	}
	_, err := tree.InsertRoute(MGet, "/users/{name}", dummyValue)
	if err == nil {
		t.Fatal("expected conflict error for different param names")
	}

	for _, want := range []string{
		`existing route "/users/{id}/timeline"`,
		`new route "/users/{name}"`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want substring %q", err.Error(), want)
		}
	}
}

func TestInsertRoute_ConflictingRegexSiblings(t *testing.T) {
	tree := newTree()

	if _, err := tree.InsertRoute(MGet, "/users/{id:[0-9]+}", dummyValue); err != nil {
		t.Fatalf("InsertRoute first: %v", err)
	}
	if _, err := tree.InsertRoute(MGet, "/users/{slug:[a-z]+}", dummyValue); err == nil {
		t.Fatal("expected conflict error for multiple regexp siblings")
	}
}

func TestInsertRoute_RegexSameMatcherDifferentParamName(t *testing.T) {
	tree := newTree()

	if _, err := tree.InsertRoute(MGet, "/users/{id:[0-9]+}", dummyValue); err != nil {
		t.Fatalf("InsertRoute first: %v", err)
	}
	if _, err := tree.InsertRoute(MGet, "/users/{name:[0-9]+}", dummyValue); err == nil {
		t.Fatal("expected conflict error for different regexp param names")
	}
}

func TestInsertRoute_RegexSharedMatcherSameParam(t *testing.T) {
	tree := newTree()

	if _, err := tree.InsertRoute(MGet, "/zip/{zip:[0-9]{5}}", dummyValue); err != nil {
		t.Fatalf("InsertRoute first: %v", err)
	}
	if _, err := tree.InsertRoute(MGet, "/zip/{zip:[0-9]{5}}/ext", dummyValue); err != nil {
		t.Fatalf("InsertRoute second: %v", err)
	}
}

func TestFindEndpoint(t *testing.T) {
	tree := newTree()
	inserts := []struct {
		method  MethodTyp
		pattern string
		value   string
	}{
		{MGet, "/users", "users-get"},
		{MPost, "/users", "users-post"},
		{MGet, "/users/{id}", "user-by-id"},
		{MGet, "/files/{path...}", "files"},
		{MGet, "/n/{id:[0-9]+}", "num"},
	}
	for _, in := range inserts {
		if _, err := tree.InsertRoute(in.method, in.pattern, in.value); err != nil {
			t.Fatalf("InsertRoute(%v, %q): %v", in.method, in.pattern, err)
		}
	}

	// FindEndpoint reaches an endpoint only when the exact pattern already
	// resolves to an existing node; any pattern that an InsertRoute would build
	// by splitting a node, extending a prefix, or adding a sibling param/regexp
	// child reports no endpoint, because none can pre-exist at that location.
	tests := []struct {
		name      string
		method    MethodTyp
		pattern   string
		wantFound bool
		wantValue string
	}{
		{"exact static hit", MGet, "/users", true, "users-get"},
		{"same node, other method", MPost, "/users", true, "users-post"},
		{"method absent at existing node", MDelete, "/users", false, ""},
		{"missing pattern", MGet, "/nope", false, ""},
		{"shorter prefix would split", MGet, "/use", false, ""},
		{"longer pattern would extend", MGet, "/usersx", false, ""},
		{"param child hit", MGet, "/users/{id}", true, "user-by-id"},
		{"param key mismatch", MGet, "/users/{slug}", false, ""},
		{"catch-all hit", MGet, "/files/{path...}", true, "files"},
		{"catch-all key mismatch", MGet, "/files/{other...}", false, ""},
		{"regexp hit", MGet, "/n/{id:[0-9]+}", true, "num"},
		{"regexp matcher mismatch", MGet, "/n/{id:[a-z]+}", false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ep, ok := tree.FindEndpoint(tt.method, tt.pattern)
			if ok != tt.wantFound {
				t.Fatalf("FindEndpoint(%v, %q) found = %v, want %v", tt.method, tt.pattern, ok, tt.wantFound)
			}
			if !tt.wantFound {
				if ep != nil {
					t.Errorf("FindEndpoint(%v, %q) = %v, want nil endpoint when not found", tt.method, tt.pattern, ep)
				}
				return
			}
			if ep == nil {
				t.Fatalf("FindEndpoint(%v, %q) endpoint = nil, want non-nil", tt.method, tt.pattern)
			}
			if ep.Value != tt.wantValue {
				t.Errorf("FindEndpoint(%v, %q) value = %q, want %q", tt.method, tt.pattern, ep.Value, tt.wantValue)
			}
			if ep.AutoGenerated {
				t.Errorf("FindEndpoint(%v, %q) AutoGenerated = true, want false for an explicit route", tt.method, tt.pattern)
			}
		})
	}
}

// TestFindEndpoint_AutoGenerated is the load-bearing distinction for Mount's
// preflight: an auto-generated endpoint (e.g. a HEAD twin) is reported with
// AutoGenerated set, so mux.wouldConflict treats it as overwritable rather than
// a conflict — mirroring setEndpoint, which lets a new registration replace it.
func TestFindEndpoint_AutoGenerated(t *testing.T) {
	tree := newTree()
	if _, err := tree.InsertRoute(MHead, "/p", "head-auto", true); err != nil {
		t.Fatalf("InsertRoute auto: %v", err)
	}

	ep, ok := tree.FindEndpoint(MHead, "/p")
	if !ok {
		t.Fatal("FindEndpoint(MHead, /p) found = false, want true")
	}
	if !ep.AutoGenerated {
		t.Error("AutoGenerated = false, want true for an auto-generated endpoint")
	}
	if ep.Value != "head-auto" {
		t.Errorf("Value = %q, want %q", ep.Value, "head-auto")
	}
}

func TestRouteContext_Reset(t *testing.T) {
	rctx := &RouteContext{}
	rctx.Params.Add("id", "42")
	rctx.RouteMethod = "GET"
	rctx.RoutePath = "/test"
	rctx.MethodNotAllowed = true

	rctx.Reset()

	if len(rctx.Params.Keys) != 0 {
		t.Error("expected empty Keys after Reset")
	}
	if rctx.RouteMethod != "" {
		t.Error("expected empty RouteMethod after Reset")
	}
	if rctx.MethodNotAllowed {
		t.Error("expected MethodNotAllowed=false after Reset")
	}
}

func BenchmarkFindRoute_Static(b *testing.B) {
	tree := newTree()
	tree.InsertRoute(MGet, "/users/list", dummyValue)

	b.ReportAllocs()

	for b.Loop() {
		rctx := &RouteContext{}
		tree.FindRoute(rctx, MGet, "/users/list")
	}
}

func BenchmarkFindRoute_Param(b *testing.B) {
	tree := newTree()
	tree.InsertRoute(MGet, "/users/{id}", dummyValue)

	b.ReportAllocs()

	for b.Loop() {
		rctx := &RouteContext{}
		tree.FindRoute(rctx, MGet, "/users/42")
	}
}

func TestNewNode_EmptyPrefix(t *testing.T) {
	// Must not panic on empty prefix
	n := newNode[string](NtStatic, "")
	if n.Label != 0 {
		t.Errorf("Label = %d, want 0 for empty prefix", n.Label)
	}
	if n.Prefix != "" {
		t.Errorf("Prefix = %q, want empty", n.Prefix)
	}
}

func TestNewNode_NormalPrefix(t *testing.T) {
	n := newNode[string](NtStatic, "/users")
	if n.Label != '/' {
		t.Errorf("Label = %q, want '/'", n.Label)
	}
	if n.Prefix != "/users" {
		t.Errorf("Prefix = %q, want %q", n.Prefix, "/users")
	}
}

func TestFindRoute_RegexWithTailByte(t *testing.T) {
	tree := newTree()
	_, err := tree.InsertRoute(MGet, "/articles/{slug:[a-z-]+}.html", dummyValue)
	if err != nil {
		t.Fatalf("InsertRoute: %v", err)
	}

	tests := []struct {
		path      string
		wantMatch bool
		wantSlug  string
	}{
		{"/articles/hello-world.html", true, "hello-world"},
		{"/articles/test.html", true, "test"},
		{"/articles/UPPER.html", false, ""},       // [a-z-]+ only
		{"/articles/hello-world.json", false, ""}, // wrong suffix
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			rctx := &RouteContext{}
			_, found := tree.FindRoute(rctx, MGet, tt.path)

			if tt.wantMatch && !found {
				t.Fatalf("expected match for %q", tt.path)
			}
			if !tt.wantMatch && found {
				t.Fatalf("expected no match for %q, got one", tt.path)
			}

			if tt.wantMatch {
				if got := rctx.URLParam("slug"); got != tt.wantSlug {
					t.Errorf("param slug = %q, want %q", got, tt.wantSlug)
				}
			}
		})
	}
}

func TestFindRoute_RegexGreedyBoundary(t *testing.T) {
	tree := newTree()
	_, err := tree.InsertRoute(MGet, "/page/{name:[a-z.]+}/view", dummyValue)
	if err != nil {
		t.Fatalf("InsertRoute: %v", err)
	}

	rctx := &RouteContext{}
	if _, found := tree.FindRoute(rctx, MGet, "/page/test.page/view"); !found {
		t.Fatal("expected match for /page/test.page/view")
	}
	if got := rctx.URLParam("name"); got != "test.page" {
		t.Errorf("param name = %q, want %q", got, "test.page")
	}
}

func BenchmarkFindRoute_Regex(b *testing.B) {
	tree := newTree()
	tree.InsertRoute(MGet, "/users/{id:[0-9]+}", dummyValue)

	b.ReportAllocs()

	for b.Loop() {
		rctx := &RouteContext{}
		tree.FindRoute(rctx, MGet, "/users/42")
	}
}
