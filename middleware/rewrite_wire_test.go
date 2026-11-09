package middleware_test

import (
	"fmt"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/middleware"
)

// rewriteApp returns an App whose PreRouting chain applies one rule and
// whose handlers report the route, the escaped path and the parameters they
// see after the rewrite.
func rewriteApp(t *testing.T, rule middleware.RewriteRule, routes ...string) *credo.App {
	t.Helper()
	app := mustNew(t)
	app.PreRouting(middleware.Rewrite(middleware.RewriteConfig{Rules: []middleware.RewriteRule{rule}}))
	for _, route := range routes {
		app.GET(route, func(c *credo.Context) error {
			req := c.Request()
			var params []string
			for name, value := range req.RouteParams() {
				params = append(params, name+"="+value)
			}
			slices.Sort(params)
			return c.Response().Text(200, fmt.Sprintf("%s %s raw=%q %s",
				route, req.URL.EscapedPath(), req.URL.RawPath, strings.Join(params, " ")))
		})
	}
	return app
}

func get(app *credo.App, target string) (int, string) {
	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", target, nil))
	return w.Code, w.Body.String()
}

// An encoded slash is data inside a segment: a single-segment pattern
// matches the segment that contains it, and the rewritten path keeps the
// escape, so the route parameter decodes once to "a/b".
func TestRewrite_EncodedSlashMatchesOneSegmentAndStaysEscaped(t *testing.T) {
	app := rewriteApp(t, middleware.RewriteRule{From: "/old/{name}", To: "/new/{name}"}, "/new/{name}")

	_, body := get(app, "/old/a%2Fb")
	if want := `/new/{name} /new/a%2Fb raw="/new/a%2Fb" name=a/b`; body != want {
		t.Errorf("body = %q, want %q", body, want)
	}
}

// A rule that matched across an encoded slash, when Rewrite matched the
// decoded path, no longer does: "/old/x%2Fy" is one segment.
func TestRewrite_RuleNoLongerMatchesAcrossEncodedSlash(t *testing.T) {
	app := rewriteApp(t, middleware.RewriteRule{From: "/old/{a}/{b}", To: "/new/{a}/{b}"},
		"/new/{a}/{b}", "/old/{rest...}")

	_, body := get(app, "/old/x%2Fy")
	if want := `/old/{rest...} /old/x%2Fy raw="/old/x%2Fy" rest=x/y`; body != want {
		t.Errorf("body = %q, want %q", body, want)
	}
}

// A catch-all keeps its slashes as separators and its escaped slashes as
// data, so the router sees after the rewrite the segments the client sent.
func TestRewrite_CatchAllKeepsSegments(t *testing.T) {
	app := rewriteApp(t, middleware.RewriteRule{From: "/old/{rest...}", To: "/new/{rest}"},
		"/new/{a}/{b}/{c}", "/new/{rest...}")

	_, body := get(app, "/old/x/y%2Fz/w")
	if want := `/new/{a}/{b}/{c} /new/x/y%2Fz/w raw="/new/x/y%2Fz/w" a=x b=y/z c=w`; body != want {
		t.Errorf("body = %q, want %q", body, want)
	}

	_, body = get(app, "/old/x/y%2Fz")
	if want := `/new/{rest...} /new/x/y%2Fz raw="/new/x/y%2Fz" rest=x/y/z`; body != want {
		t.Errorf("body = %q, want %q", body, want)
	}
}

// A captured value is decoded once, by the router: an encoded percent sign
// followed by "25" reaches the handler as "%25", not "%".
func TestRewrite_CaptureDecodedOnce(t *testing.T) {
	app := rewriteApp(t, middleware.RewriteRule{From: "/old/{n}", To: "/new/{n}"}, "/new/{n}")

	_, body := get(app, "/old/100%2525")
	if want := `/new/{n} /new/100%2525 raw="" n=100%25`; body != want {
		t.Errorf("body = %q, want %q", body, want)
	}
}

// Path and RawPath stay consistent: a target that needs no escape leaves
// RawPath empty, and one that keeps an escaped reserved character or
// carries bytes that must be escaped round-trips through net/url.
func TestRewrite_PathAndRawPathConsistent(t *testing.T) {
	app := rewriteApp(t, middleware.RewriteRule{From: "/old/{n}", To: "/new/{n}"}, "/new/{n}")

	for _, tc := range []struct{ target, want string }{
		{"/old/42", `/new/{n} /new/42 raw="" n=42`},
		{"/old/caf%C3%A9", `/new/{n} /new/caf%C3%A9 raw="" n=café`},
		{"/old/a%3Bb", `/new/{n} /new/a%3Bb raw="/new/a%3Bb" n=a;b`},
		{"/old/a%20b", `/new/{n} /new/a%20b raw="" n=a b`},
	} {
		if _, body := get(app, tc.target); body != tc.want {
			t.Errorf("GET %s: body = %q, want %q", tc.target, body, tc.want)
		}
	}
}

// The target is a wire-form path: an escape in To stays data.
func TestRewrite_TargetIsWireForm(t *testing.T) {
	app := rewriteApp(t, middleware.RewriteRule{From: "/old", To: "/new/a%2Fb"}, "/new/{name}")

	_, body := get(app, "/old")
	if want := `/new/{name} /new/a%2Fb raw="/new/a%2Fb" name=a/b`; body != want {
		t.Errorf("body = %q, want %q", body, want)
	}
}

// Literal From text matches only its literal spelling, as in a route
// pattern: a "%" matches the client's "%25" and ";" does not match "%3B".
func TestRewrite_LiteralFromTextIsCanonical(t *testing.T) {
	app := rewriteApp(t, middleware.RewriteRule{From: "/100%/a;b", To: "/hit"}, "/hit", "/{rest...}")

	if _, body := get(app, "/100%25/a;b"); !strings.HasPrefix(body, "/hit ") {
		t.Errorf("GET /100%%25/a;b: body = %q, want the rewritten /hit", body)
	}
	if _, body := get(app, "/100%25/a%3Bb"); !strings.HasPrefix(body, "/{rest...} ") {
		t.Errorf("GET /100%%25/a%%3Bb: body = %q, want no rewrite", body)
	}
}

// A malformed escape in a target panics at construction, naming the rule,
// the target, where the escape is, and how to write one.
func TestRewrite_MalformedEscapeInTargetPanics(t *testing.T) {
	want := `credo: middleware.Rewrite rule 1: To "/new/100%/x?q=50%" has a malformed escape at byte 8; ` +
		`To is a wire-form path, so write each escape as %XX with two hex digits and a literal % as %25`
	defer func() {
		if r := recover(); fmt.Sprint(r) != want {
			t.Fatalf("panic = %v, want %q", r, want)
		}
	}()
	middleware.Rewrite(middleware.RewriteConfig{Rules: []middleware.RewriteRule{
		{From: "/a", To: "/b"},
		{From: "/old", To: "/new/100%/x?q=50%"},
	}})
}

// The query string of a target is not a path and is not checked.
func TestRewrite_TargetQueryNotChecked(t *testing.T) {
	middleware.Rewrite(middleware.RewriteConfig{Rules: []middleware.RewriteRule{{From: "/old", To: "/new?q=50%"}}})
}

// A Regexp group that splits a percent-escape yields a target with a
// malformed escape: the rewrite returns an error naming the rule, the
// target and the remedy, and leaves the request unchanged.
func TestRewrite_ExpandedTargetWithMalformedEscapeIsError(t *testing.T) {
	mw := middleware.Rewrite(middleware.RewriteConfig{Rules: []middleware.RewriteRule{{
		Regexp: regexp.MustCompile(`^/old/(?P<a>[^/]*%2)F$`),
		To:     "/new/{a}",
	}}})
	c := credo.NewContext(httptest.NewRecorder(), httptest.NewRequest("GET", "/old/x%2F?keep=1", nil))
	called := false
	err := mw(func(*credo.Context) error { called = true; return nil })(c)

	want := `credo: middleware.Rewrite rule 0: the rewritten path "/new/x%2" has a malformed escape (invalid URL escape "%2"); ` +
		`a capture group splits a percent-escape, so make each group capture whole %XX escapes`
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
	if called {
		t.Error("next was called after the rewrite failed")
	}
	if u := c.Request().URL; u.EscapedPath() != "/old/x%2F" || u.RawQuery != "keep=1" {
		t.Errorf("URL = %s?%s, want it unchanged", u.EscapedPath(), u.RawQuery)
	}
}

// A {name:regex} parameter matches as the router's does: one segment, never
// across a raw slash, even when the constraint would match the slash; an
// escaped slash inside the segment is data.
func TestRewrite_RegexParamIsOneSegment(t *testing.T) {
	app := rewriteApp(t, middleware.RewriteRule{From: "/p/{rest:.+}", To: "/q/{rest}"}, "/q/{rest}", "/p/{x...}")

	if _, body := get(app, "/p/a/b"); body != `/p/{x...} /p/a/b raw="" x=a/b` {
		t.Errorf("GET /p/a/b: body = %q, want no rewrite", body)
	}
	if _, body := get(app, "/p/a%2Fb"); body != `/q/{rest} /q/a%2Fb raw="/q/a%2Fb" rest=a/b` {
		t.Errorf("GET /p/a%%2Fb: body = %q, want the rewrite with the escape kept", body)
	}
}

// A constraint is checked on the whole decoded value, as the router checks
// it: "%31" is "1" and satisfies [0-9]+, while "12x" does not.
func TestRewrite_ConstraintOnDecodedValue(t *testing.T) {
	app := rewriteApp(t, middleware.RewriteRule{From: "/users/{id:[0-9]+}", To: "/api/users/{id}"},
		"/api/users/{id}", "/users/{id}")

	if _, body := get(app, "/users/%31%32"); body != `/api/users/{id} /api/users/12 raw="" id=12` {
		t.Errorf("GET /users/%%31%%32: body = %q, want the rewrite", body)
	}
	if _, body := get(app, "/users/12x"); body != `/users/{id} /users/12x raw="" id=12x` {
		t.Errorf("GET /users/12x: body = %q, want no rewrite", body)
	}
	if _, body := get(app, "/users/a%3B1"); body != `/users/{id} /users/a%3B1 raw="/users/a%3B1" id=a;1` {
		t.Errorf("GET /users/a%%3B1: body = %q, want no rewrite", body)
	}
}

// A parameter ends at the byte that follows it in the pattern, as in the
// router: "/d/{a}-{b}" splits "x-y-z" at its first "-".
func TestRewrite_ParamEndsAtTailByte(t *testing.T) {
	app := rewriteApp(t, middleware.RewriteRule{From: "/d/{a}-{b}", To: "/e/{a}/{b}"}, "/e/{a}/{b}")

	if _, body := get(app, "/d/x-y-z"); body != `/e/{a}/{b} /e/x/y-z raw="" a=x b=y-z` {
		t.Errorf("body = %q", body)
	}
}

// A custom Regexp rule is the caller's regexp over the canonical path and
// may span segments.
func TestRewrite_CustomRegexpSpansSegments(t *testing.T) {
	app := rewriteApp(t, middleware.RewriteRule{Regexp: regexp.MustCompile(`^/p/(?P<rest>.+)$`), To: "/q/{rest}"},
		"/q/{rest...}")

	if _, body := get(app, "/p/a/b%2Fc"); body != `/q/{rest...} /q/a/b%2Fc raw="/q/a/b%2Fc" rest=a/b/c` {
		t.Errorf("body = %q", body)
	}
}

// Text after a catch-all could never match, since the catch-all takes the
// rest of the path: the rule panics when built, naming the remedy.
func TestRewrite_TextAfterCatchAllPanics(t *testing.T) {
	want := `credo: middleware.Rewrite rule 0: pattern: text "/x" after the catch-all {p...} in "/a/{p...}/x": ` +
		`a catch-all takes the rest of the path, so end the pattern with it`
	defer func() {
		if r := recover(); fmt.Sprint(r) != want {
			t.Fatalf("panic = %v, want %q", r, want)
		}
	}()
	middleware.Rewrite(middleware.RewriteConfig{Rules: []middleware.RewriteRule{{From: "/a/{p...}/x", To: "/b"}}})
}
