package radix

import (
	"errors"
	"slices"
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

// Parameter names are endpoint-owned: the tree identifies a route by its
// method and name-stripped shape, so "/users/{id}" and "/users/{name}" are one
// route (a duplicate for the same method) while "/users/{id}" and
// "/users/{name}/timeline" are two routes that share a dynamic node and each
// name its capture independently.
func TestInsertRoute_SameShapeDifferentNamesIsDuplicate(t *testing.T) {
	tree := newTree()

	if _, err := tree.InsertRoute(MGet, "/users/{id}", "first"); err != nil {
		t.Fatalf("InsertRoute first: %v", err)
	}
	_, err := tree.InsertRoute(MGet, "/users/{name}", "second")
	if err == nil {
		t.Fatal("expected duplicate route error for the same method and shape")
	}

	dup, ok := errors.AsType[*DuplicateRouteError[string]](err)
	if !ok {
		t.Fatalf("error = %T (%v), want *DuplicateRouteError[string]", err, err)
	}
	if dup.Pattern != "/users/{name}" {
		t.Errorf("Pattern = %q, want %q", dup.Pattern, "/users/{name}")
	}
	if dup.ExistingPattern != "/users/{id}" {
		t.Errorf("ExistingPattern = %q, want %q", dup.ExistingPattern, "/users/{id}")
	}
	if dup.Existing != "first" {
		t.Errorf("Existing = %q, want %q", dup.Existing, "first")
	}
	if !strings.Contains(err.Error(), `already registered as "/users/{id}"`) {
		t.Errorf("error = %q, want it to name the existing spelling", err.Error())
	}

	// The failed registration must not have disturbed the existing endpoint.
	rctx := &RouteContext{}
	v, found := tree.FindRoute(rctx, MGet, "/users/7")
	if !found || v != "first" {
		t.Fatalf("FindRoute after duplicate = %q (found=%v), want \"first\"", v, found)
	}
	if got := rctx.URLParam("id"); got != "7" {
		t.Errorf("param id = %q, want %q", got, "7")
	}
}

func TestInsertRoute_SharedSegmentEndpointOwnedNames(t *testing.T) {
	tree := newTree()

	inserts := []struct {
		method  MethodTyp
		pattern string
		value   string
	}{
		{MGet, "/users/{id}/timeline", "timeline"},
		{MGet, "/users/{name}", "show"},
		{MDelete, "/users/{user_id}", "delete"}, // same shape as show, other method
		{MGet, "/users/{uid}/posts/{post}", "post"},
	}
	for _, in := range inserts {
		if _, err := tree.InsertRoute(in.method, in.pattern, in.value); err != nil {
			t.Fatalf("InsertRoute(%v, %q): %v", in.method, in.pattern, err)
		}
	}

	tests := []struct {
		method     MethodTyp
		path       string
		wantValue  string
		wantKeys   []string
		wantValues []string
	}{
		{MGet, "/users/7/timeline", "timeline", []string{"id"}, []string{"7"}},
		{MGet, "/users/7", "show", []string{"name"}, []string{"7"}},
		{MDelete, "/users/7", "delete", []string{"user_id"}, []string{"7"}},
		{MGet, "/users/7/posts/9", "post", []string{"uid", "post"}, []string{"7", "9"}},
	}
	for _, tt := range tests {
		t.Run(methodName(tt.method)+" "+tt.path, func(t *testing.T) {
			rctx := &RouteContext{}
			v, found := tree.FindRoute(rctx, tt.method, tt.path)
			if !found || v != tt.wantValue {
				t.Fatalf("FindRoute = %q (found=%v), want %q", v, found, tt.wantValue)
			}
			if !slices.Equal(rctx.Params.Keys, tt.wantKeys) {
				t.Errorf("Keys = %q, want %q", rctx.Params.Keys, tt.wantKeys)
			}
			if !slices.Equal(rctx.Params.Values, tt.wantValues) {
				t.Errorf("Values = %q, want %q", rctx.Params.Values, tt.wantValues)
			}
		})
	}

	// A sibling endpoint's name is never visible to a handler.
	rctx := &RouteContext{}
	if _, found := tree.FindRoute(rctx, MGet, "/users/7"); !found {
		t.Fatal("expected /users/7 to match")
	}
	if got := rctx.URLParam("id"); got != "" {
		t.Errorf("param id = %q on the show endpoint, want empty (owned by the timeline endpoint)", got)
	}
}

func TestInsertRoute_EndpointParamKeysInCaptureOrder(t *testing.T) {
	tree := newTree()

	inserts := []struct {
		pattern  string
		wantKeys []string
	}{
		{"/static", nil},
		{"/a/{x}/b/{y:[0-9]+}/c/{rest...}", []string{"x", "y", "rest"}},
		{"/a/{one}/b/{two:[0-9]+}", []string{"one", "two"}},
	}
	for _, in := range inserts {
		if _, err := tree.InsertRoute(MGet, in.pattern, in.pattern); err != nil {
			t.Fatalf("InsertRoute(%q): %v", in.pattern, err)
		}
	}
	for _, in := range inserts {
		ep, ok := tree.FindEndpoint(MGet, in.pattern)
		if !ok {
			t.Fatalf("FindEndpoint(%q) not found", in.pattern)
		}
		if !slices.Equal(ep.ParamKeys, in.wantKeys) {
			t.Errorf("ParamKeys(%q) = %q, want %q", in.pattern, ep.ParamKeys, in.wantKeys)
		}
	}

	rctx := &RouteContext{}
	v, found := tree.FindRoute(rctx, MGet, "/a/1/b/22/c/x/y/z")
	if !found || v != "/a/{x}/b/{y:[0-9]+}/c/{rest...}" {
		t.Fatalf("FindRoute = %q (found=%v)", v, found)
	}
	if !slices.Equal(rctx.Params.Keys, []string{"x", "y", "rest"}) {
		t.Errorf("Keys = %q", rctx.Params.Keys)
	}
	if !slices.Equal(rctx.Params.Values, []string{"1", "22", "x/y/z"}) {
		t.Errorf("Values = %q", rctx.Params.Values)
	}

	rctx = &RouteContext{}
	if _, found := tree.FindRoute(rctx, MGet, "/a/1/b/22"); !found {
		t.Fatal("expected /a/1/b/22 to match")
	}
	if !slices.Equal(rctx.Params.Keys, []string{"one", "two"}) {
		t.Errorf("Keys = %q, want the shorter endpoint's names", rctx.Params.Keys)
	}
	if !slices.Equal(rctx.Params.Values, []string{"1", "22"}) {
		t.Errorf("Values = %q", rctx.Params.Values)
	}
}

// Backtracking must leave no stray capture behind: when a regexp branch
// captures a value but fails deeper, the eventual param-branch match names
// exactly its own captures.
func TestFindRoute_BacktrackingKeepsCapturesAligned(t *testing.T) {
	tree := newTree()

	if _, err := tree.InsertRoute(MGet, "/x/{num:[0-9]+}/only-num", "num"); err != nil {
		t.Fatalf("InsertRoute regexp: %v", err)
	}
	if _, err := tree.InsertRoute(MGet, "/x/{any}/{tail}", "any"); err != nil {
		t.Fatalf("InsertRoute param: %v", err)
	}

	rctx := &RouteContext{}
	v, found := tree.FindRoute(rctx, MGet, "/x/42/other")
	if !found || v != "any" {
		t.Fatalf("FindRoute = %q (found=%v), want \"any\"", v, found)
	}
	if !slices.Equal(rctx.Params.Keys, []string{"any", "tail"}) {
		t.Errorf("Keys = %q, want [any tail]", rctx.Params.Keys)
	}
	if !slices.Equal(rctx.Params.Values, []string{"42", "other"}) {
		t.Errorf("Values = %q, want [42 other]", rctx.Params.Values)
	}

	rctx = &RouteContext{}
	v, found = tree.FindRoute(rctx, MGet, "/x/42/only-num")
	if !found || v != "num" {
		t.Fatalf("FindRoute = %q (found=%v), want \"num\"", v, found)
	}
	if !slices.Equal(rctx.Params.Keys, []string{"num"}) || !slices.Equal(rctx.Params.Values, []string{"42"}) {
		t.Errorf("Keys/Values = %q/%q, want [num]/[42]", rctx.Params.Keys, rctx.Params.Values)
	}
}

// A 405 names nothing: no endpoint matched, so no keys are appended and the
// caller's key-driven copy sees no parameters.
func TestFindRoute_MethodNotAllowedAppendsNoKeys(t *testing.T) {
	tree := newTree()
	if _, err := tree.InsertRoute(MGet, "/users/{id}", dummyValue); err != nil {
		t.Fatalf("InsertRoute: %v", err)
	}
	if _, err := tree.InsertRoute(MGet, "/files/{path...}", dummyValue); err != nil {
		t.Fatalf("InsertRoute: %v", err)
	}

	for _, path := range []string{"/users/7", "/files/a/b"} {
		rctx := &RouteContext{}
		if _, found := tree.FindRoute(rctx, MPost, path); found {
			t.Fatalf("FindRoute(POST %s) matched, want 405", path)
		}
		if !rctx.MethodNotAllowed {
			t.Errorf("MethodNotAllowed = false for %s", path)
		}
		if len(rctx.Params.Keys) != 0 {
			t.Errorf("Keys = %q for %s, want none", rctx.Params.Keys, path)
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
	// Same method and shape: a duplicate, not a name conflict.
	_, err := tree.InsertRoute(MGet, "/users/{name:[0-9]+}", dummyValue)
	if _, ok := errors.AsType[*DuplicateRouteError[string]](err); !ok {
		t.Fatalf("error = %T (%v), want *DuplicateRouteError[string]", err, err)
	}
	// A different method or a longer shape shares the regexp node under its
	// own name.
	if _, err := tree.InsertRoute(MPost, "/users/{name:[0-9]+}", "post"); err != nil {
		t.Fatalf("InsertRoute other method: %v", err)
	}
	if _, err := tree.InsertRoute(MGet, "/users/{uid:[0-9]+}/ext", "ext"); err != nil {
		t.Fatalf("InsertRoute longer: %v", err)
	}

	rctx := &RouteContext{}
	if v, found := tree.FindRoute(rctx, MPost, "/users/42"); !found || v != "post" {
		t.Fatalf("FindRoute POST = %q (found=%v), want \"post\"", v, found)
	}
	if got := rctx.URLParam("name"); got != "42" {
		t.Errorf("param name = %q, want %q", got, "42")
	}
	rctx = &RouteContext{}
	if v, found := tree.FindRoute(rctx, MGet, "/users/42/ext"); !found || v != "ext" {
		t.Fatalf("FindRoute /ext = %q (found=%v), want \"ext\"", v, found)
	}
	if got := rctx.URLParam("uid"); got != "42" {
		t.Errorf("param uid = %q, want %q", got, "42")
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

	// FindEndpoint reaches an endpoint only when the pattern's shape already
	// resolves to an existing node; any pattern that an InsertRoute would build
	// by splitting a node, extending a prefix, or adding a sibling regexp child
	// reports no endpoint, because none can pre-exist at that location.
	// Parameter names do not take part: they belong to endpoints.
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
		{"param other name, same shape", MGet, "/users/{slug}", true, "user-by-id"},
		{"catch-all hit", MGet, "/files/{path...}", true, "files"},
		{"catch-all other name, same shape", MGet, "/files/{other...}", true, "files"},
		{"regexp hit", MGet, "/n/{id:[0-9]+}", true, "num"},
		{"regexp other name, same matcher", MGet, "/n/{num:[0-9]+}", true, "num"},
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
