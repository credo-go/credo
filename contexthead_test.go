package credo_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/middleware"
)

type headUser struct{ name string }

// spanKey stands for the key a tracing middleware stores its span under.
type spanKey struct{}

var (
	keyTenant = credo.NewKey[string]("tenant")
	keyBefore = credo.NewKey[string]("before")
	keyInner  = credo.NewKey[string]("inner")
)

// addSpan is a stdlib middleware that adds a span to the request's context,
// the way a tracing middleware does, after reading a value set before it.
func addSpan(sawBefore *string) credo.StdMiddleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if v, ok := r.Context().Value(keyBefore).(string); ok {
				*sawBefore = v
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), spanKey{}, "span-1")))
		})
	}
}

func setHeadUser(next credo.Handler) credo.Handler {
	return func(c *credo.Context) error {
		c.SetUser(&headUser{name: "alice"})
		return next(c)
	}
}

// TestContextHead_Acceptance is R4's acceptance test: the head carries a
// deadline and a span added by enclosing layers, a user set inside both
// scopes reaches the layers outside them, and stdlib code reads values with
// Value and passes on a request that still carries the user.
func TestContextHead_Acceptance(t *testing.T) {
	app := mustNew(t)
	var (
		sawBefore      string // what the span middleware read with Value
		innerSawTenant string // what a stdlib middleware after auth read with Value
		outerUser      string // the user, read by the outermost layer after next
		outerTenant    string
		handlerChecks  []string
	)
	outer := func(next credo.Handler) credo.Handler {
		return func(c *credo.Context) error {
			c.Set(keyBefore, "set-before")
			err := next(c)
			if u, ok := c.GetUser[*headUser](); ok {
				outerUser = u.name
			}
			outerTenant, _ = c.Get(keyTenant)
			if _, ok := c.Context().Deadline(); ok {
				handlerChecks = append(handlerChecks, "deadline leaked out of its scope")
			}
			if c.Context().Value(spanKey{}) != nil {
				handlerChecks = append(handlerChecks, "span leaked out of its scope")
			}
			return err
		}
	}
	afterAuth := credo.WrapStdMiddleware(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			innerSawTenant, _ = r.Context().Value(keyTenant).(string)
			type k struct{}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), k{}, 1)))
		})
	})
	tenant := func(next credo.Handler) credo.Handler {
		return func(c *credo.Context) error {
			c.Set(keyTenant, "acme")
			return next(c)
		}
	}
	app.GET("/x", func(c *credo.Context) error {
		for name, ctx := range map[string]context.Context{
			"c.Context()":           c.Context(),
			"c.Request().Context()": c.Request().Context(),
			"Std().Context()":       c.Request().Std().Context(),
		} {
			if _, ok := ctx.Deadline(); !ok {
				handlerChecks = append(handlerChecks, name+" has no deadline")
			}
			if ctx.Value(spanKey{}) != "span-1" {
				handlerChecks = append(handlerChecks, name+" has no span")
			}
		}
		if u, ok := c.GetUser[*headUser](); !ok || u.name != "alice" {
			handlerChecks = append(handlerChecks, "the handler after a stdlib middleware lost the user")
		}
		c.Set(keyInner, "from-handler")
		return c.Response().NoContent(http.StatusNoContent)
	}).Middleware(
		outer,
		credo.WrapStdMiddleware(addSpan(&sawBefore)),
		middleware.Timeout(middleware.TimeoutConfig{Timeout: time.Minute}),
		setHeadUser,
		tenant,
		afterAuth,
	)

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/x", nil))
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (body %q)", w.Code, w.Body.String())
	}
	for _, c := range handlerChecks {
		t.Error(c)
	}
	if sawBefore != "set-before" {
		t.Errorf("the stdlib middleware read %q with Value, want the value set before it", sawBefore)
	}
	if innerSawTenant != "acme" {
		t.Errorf("the stdlib middleware after auth read %q with Value, want acme", innerSawTenant)
	}
	if outerUser != "alice" {
		t.Errorf("the outer layer sees user %q, want the one set inside the scopes", outerUser)
	}
	if outerTenant != "acme" {
		t.Errorf("the outer layer sees tenant %q, want acme", outerTenant)
	}
}

func TestContextHead_StdCopiesOnlyWhenBehind(t *testing.T) {
	app := mustNew(t)
	app.GET("/x", func(c *credo.Context) error {
		embedded := c.Request().Request
		if got := c.Request().Std(); got != embedded {
			t.Error("Std copied a request that already carries the head")
		}
		c.Set(keyTenant, "acme")
		first := c.Request().Std()
		if first == embedded {
			t.Error("Std did not copy after a Set")
		}
		if first != c.Request().Request {
			t.Error("Std did not install its copy as the embedded request")
		}
		if first.Context().Value(keyTenant) != "acme" {
			t.Error("Std's copy does not carry the head")
		}
		if again := c.Request().Std(); again != first {
			t.Error("a second Std without a new Set copied again")
		}
		if c.Request().Request.Context() != c.Context() {
			t.Error("the installed request lags the head")
		}
		return c.Response().NoContent(http.StatusNoContent)
	})
	app.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
}

func TestContextHead_EmbeddedRequestLagsAfterSet(t *testing.T) {
	app := mustNew(t)
	app.GET("/x", func(c *credo.Context) error {
		c.Set(keyTenant, "acme")
		if c.Request().Request.Context().Value(keyTenant) != nil {
			t.Error("Set copied the request")
		}
		if v, _ := c.Request().Context().Value(keyTenant).(string); v != "acme" {
			t.Errorf("Request.Context() = %q, want the head", v)
		}
		return nil
	})
	app.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
}

type withinKey struct{}

func TestContextHead_Within(t *testing.T) {
	for _, panics := range []bool{false, true} {
		name := "returns"
		if panics {
			name = "panics"
		}
		t.Run(name, func(t *testing.T) {
			logger, _ := newTestLogger(t)
			app := mustNew(t, credo.WithLogger(logger))
			var problems []string
			app.GET("/x", func(c *credo.Context) error {
				outerCtx := c.Context()
				outerReq := c.Request().Request
				func() {
					defer func() { _ = recover() }()
					scoped := context.WithValue(c.Context(), withinKey{}, "scoped")
					_ = c.Within(scoped, func(c *credo.Context) error {
						if c.Context().Value(withinKey{}) != "scoped" ||
							c.Request().Context().Value(withinKey{}) != "scoped" ||
							c.Request().Request.Context().Value(withinKey{}) != "scoped" {
							problems = append(problems, "the inner chain does not read the scope's context")
						}
						c.Set(keyInner, "set-inside")
						if panics {
							panic("boom")
						}
						return nil
					})
				}()
				if c.Context().Value(withinKey{}) != nil {
					problems = append(problems, "the scope's own value outlived it")
				}
				if v, _ := c.Get(keyInner); v != "set-inside" {
					problems = append(problems, "a value set inside was not reapplied: "+v)
				}
				if c.Request().Request != outerReq {
					problems = append(problems, "the outer embedded request was not restored")
				}
				if outerCtx.Value(keyInner) != nil {
					problems = append(problems, "a context taken before the scope changed")
				}
				return c.Response().NoContent(http.StatusNoContent)
			})
			app.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
			for _, p := range problems {
				t.Error(p)
			}
		})
	}
}

func TestContextHead_WithinNilPanics(t *testing.T) {
	c := credo.NewContext(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	defer func() {
		r := recover()
		if s, _ := r.(string); !strings.Contains(s, "Context.Within: nil context.Context") {
			t.Fatalf("panic = %v, want the nil-context message", r)
		}
	}()
	_ = c.Within(nil, func(*credo.Context) error { return nil }) //nolint:staticcheck // the nil is the case under test
}

// TestContextHead_ForeignRequestReported: assigning c.Request().Request a
// request with a new context does not move the head, and in debug mode it is
// reported once per route.
func TestContextHead_ForeignRequestReported(t *testing.T) {
	logger, buf := newTestLogger(t)
	app := mustNew(t, credo.WithLogger(logger), credo.WithDebug())
	var moved bool
	h := func(c *credo.Context) error {
		r := c.Request().Request
		c.Request().Request = r.WithContext(context.WithValue(r.Context(), withinKey{}, "old idiom"))
		moved = c.Context().Value(withinKey{}) != nil
		return c.Response().NoContent(http.StatusNoContent)
	}
	app.GET("/a", h)
	app.GET("/b", h)
	for _, path := range []string{"/a", "/a", "/b"} {
		app.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
	}
	if moved {
		t.Error("assigning the embedded request moved the head")
	}
	const msg = "c.Request().Request was replaced"
	if got := strings.Count(buf.String(), msg); got != 2 {
		t.Fatalf("reported %d times, want once per route (2):\n%s", got, buf.String())
	}
	for _, want := range []string{"GET /a", "GET /b", "c.Within(ctx, next)", "c.Request().Std()"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("report does not name %q:\n%s", want, buf.String())
		}
	}
}

// TestContextHead_UserSetBeforePanicReachesObservers: a user set inside a
// scope before the handler panics is still the request's user after recovery.
func TestContextHead_UserSetBeforePanicReachesObservers(t *testing.T) {
	logger, _ := newTestLogger(t)
	app := mustNew(t, credo.WithLogger(logger))
	var observed string
	app.UseAccessLog(credo.AccessLogConfig{
		Logger: slog.New(slog.DiscardHandler),
		ResultFilter: func(c *credo.Context, _ credo.AccessLogEntry) bool {
			if u, ok := c.GetUser[*headUser](); ok {
				observed = u.name
			}
			return false
		},
	})
	app.GET("/x", func(*credo.Context) error { panic("boom") }).
		Middleware(middleware.Timeout(middleware.TimeoutConfig{Timeout: time.Minute}), setHeadUser)

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/x", nil))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	if observed != "alice" {
		t.Fatalf("the access record observed user %q, want alice", observed)
	}
}

func TestContextHead_ContextTakenBeforeKeepsItsValue(t *testing.T) {
	c := credo.NewContext(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	c.Set(keyTenant, "first")
	before := c.Context()
	for i := range 1000 {
		c.Set(keyTenant, "v"+string(rune('a'+i%26)))
	}
	if got := before.Value(keyTenant); got != "first" {
		t.Fatalf("a context taken before the later Sets reads %v, want first", got)
	}
}

// TestContextHead_RetryRunsOverFirstAttempt: a Credo middleware that calls
// next again runs the second attempt over the values the first one set.
func TestContextHead_RetryRunsOverFirstAttempt(t *testing.T) {
	app := mustNew(t)
	attempts := 0
	var secondSaw string
	retry := func(next credo.Handler) credo.Handler {
		return func(c *credo.Context) error {
			if err := next(c); err == nil {
				return nil
			}
			return next(c)
		}
	}
	app.GET("/x", func(c *credo.Context) error {
		attempts++
		if attempts == 1 {
			c.Set(keyTenant, "first-attempt")
			return io.ErrUnexpectedEOF
		}
		secondSaw, _ = c.Get(keyTenant)
		return c.Response().NoContent(http.StatusNoContent)
	}).Middleware(retry)
	app.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
	if secondSaw != "first-attempt" {
		t.Fatalf("the second attempt saw %q, want the first attempt's value", secondSaw)
	}
}

// TestContextHead_UnrelatedContextHidesValues: a stdlib middleware that passes
// on a context not derived from the request's hides the values set before it.
func TestContextHead_UnrelatedContextHidesValues(t *testing.T) {
	app := mustNew(t)
	cut := credo.WrapStdMiddleware(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(context.Background()))
		})
	})
	var tenantSeen, userSeen bool
	var afterTenant string
	outer := func(next credo.Handler) credo.Handler {
		return func(c *credo.Context) error {
			c.Set(keyTenant, "acme")
			err := next(c)
			afterTenant, _ = c.Get(keyTenant)
			return err
		}
	}
	app.GET("/x", func(c *credo.Context) error {
		_, tenantSeen = c.Get(keyTenant)
		_, userSeen = c.GetUser[*headUser]()
		return c.Response().NoContent(http.StatusNoContent)
	}).Middleware(outer, setHeadUser, cut)
	app.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
	if tenantSeen || userSeen {
		t.Fatalf("after a context.Background() the handler saw tenant=%v user=%v, want neither", tenantSeen, userSeen)
	}
	if afterTenant != "acme" {
		t.Fatalf("after the stdlib middleware returned, the outer layer reads %q, want its own value", afterTenant)
	}
}

// TestContextHead_ZeroKeyPanics: every method that takes a key panics for a
// zero Key, naming the method.
func TestContextHead_ZeroKeyPanics(t *testing.T) {
	var zero credo.Key[string]
	app := mustNew(t)
	route := app.GET("/x", func(*credo.Context) error { return nil })
	group := app.Group("/g")
	static := app.Static("/s", testFS())
	c := credo.NewContext(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	cases := map[string]func(){
		"Context.Set":            func() { c.Set(zero, "v") },
		"Context.Get":            func() { c.Get(zero) },
		"Route.SetMeta":          func() { route.SetMeta(zero, "v") },
		"Route.RemoveMeta":       func() { route.RemoveMeta(zero) },
		"Route.GetMeta":          func() { route.GetMeta(zero) },
		"Group.SetMeta":          func() { group.SetMeta(zero, "v") },
		"Group.RemoveMeta":       func() { group.RemoveMeta(zero) },
		"App.SetMeta":            func() { app.SetMeta(zero, "v") },
		"App.RemoveMeta":         func() { app.RemoveMeta(zero) },
		"StaticRoute.SetMeta":    func() { static.SetMeta(zero, "v") },
		"StaticRoute.RemoveMeta": func() { static.RemoveMeta(zero) },
		"RouteInfo.GetMeta":      func() { credo.RouteInfo{}.GetMeta(zero) },
	}
	for method, call := range cases {
		t.Run(method, func(t *testing.T) {
			defer func() {
				s, _ := recover().(string)
				want := "credo: " + method + ": the key is a zero credo.Key[string]; declare it with credo.NewKey"
				if s != want {
					t.Fatalf("panic = %q, want %q", s, want)
				}
			}()
			call()
		})
	}
}

func TestKey_IdentityAndName(t *testing.T) {
	a := credo.NewKey[string]("same")
	b := credo.NewKey[string]("same")
	if a == b {
		t.Fatal("two NewKey calls with one name are one key")
	}
	if a.String() != "same" {
		t.Fatalf("String() = %q, want the name", a.String())
	}
	c := credo.NewContext(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	c.Set(a, "from-a")
	if _, ok := c.Get(b); ok {
		t.Fatal("a key with the same name read another key's value")
	}
	if got, _ := c.Context().Value(a).(string); got != "from-a" {
		t.Fatalf("Value(key) = %q, want the value Set stored", got)
	}
}

// TestContextHead_MountReadsParentValues: a mounted Credo App reads what was
// set before the mount, the user included; what it sets stays in its own
// Context.
func TestContextHead_MountReadsParentValues(t *testing.T) {
	child := mustNew(t)
	var childUser, childTenant string
	child.GET("/x", func(c *credo.Context) error {
		if u, ok := c.GetUser[*headUser](); ok {
			childUser = u.name
		}
		childTenant, _ = c.Get(keyTenant)
		c.Set(keyInner, "child")
		return c.Response().NoContent(http.StatusNoContent)
	})

	parent := mustNew(t)
	var parentInner string
	var parentSawInner bool
	parent.UseAccessLog(credo.AccessLogConfig{
		Logger: slog.New(slog.DiscardHandler),
		ResultFilter: func(c *credo.Context, _ credo.AccessLogEntry) bool {
			parentInner, parentSawInner = c.Get(keyInner)
			return false
		},
	})
	parent.PreRouting(setHeadUser, func(next credo.Handler) credo.Handler {
		return func(c *credo.Context) error {
			c.Set(keyTenant, "acme")
			return next(c)
		}
	})
	parent.Mount("/child", child)

	w := httptest.NewRecorder()
	parent.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/child/x", nil))
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", w.Code)
	}
	if childUser != "alice" || childTenant != "acme" {
		t.Fatalf("the mounted App read user=%q tenant=%q, want alice and acme", childUser, childTenant)
	}
	if parentSawInner {
		t.Fatalf("the parent's access record saw the child's value %q", parentInner)
	}
}
