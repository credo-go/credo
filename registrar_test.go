package credo

import (
	"testing"
)

// fakeRegistrar records registration calls so Group and Route can be
// exercised without an App.
type fakeRegistrar struct {
	frozen   bool
	routes   []string // "METHOD pattern"
	names    map[string]*Route
	frozenAt []string
}

func newFakeRegistrar() *fakeRegistrar {
	return &fakeRegistrar{names: map[string]*Route{}}
}

func (f *fakeRegistrar) addRoute(method, pattern string, h Handler, g *Group) *Route {
	f.routes = append(f.routes, method+" "+pattern)
	return &Route{method: method, pattern: pattern, handler: h, parent: g, registrar: f}
}

func (f *fakeRegistrar) addGetRoute(pattern string, h Handler, g *Group) *Route {
	return f.addRoute("GET", pattern, h, g)
}

func (f *fakeRegistrar) checkFrozen(what string) {
	f.frozenAt = append(f.frozenAt, what)
	if f.frozen {
		panic("frozen: " + what)
	}
}

func (f *fakeRegistrar) registerName(name string, route *Route) {
	if existing, ok := f.names[name]; ok && existing != route {
		panic("duplicate " + name)
	}
	f.names[name] = route
}

func (f *fakeRegistrar) deregisterName(name string) { delete(f.names, name) }

func TestRouteRegistrar_GroupAndRouteWorkWithoutApp(t *testing.T) {
	reg := newFakeRegistrar()
	root := &Group{registrar: reg}
	api := root.Group("/api")
	handler := func(*Context) error { return nil }

	route := api.Group("/v1").GET("/users/{id}", handler).Name("users.show")
	api.POST("/users", handler)

	if want := []string{"GET /api/v1/users/{id}", "POST /api/users"}; len(reg.routes) != 2 ||
		reg.routes[0] != want[0] || reg.routes[1] != want[1] {
		t.Fatalf("routes = %v, want %v", reg.routes, want)
	}
	if reg.names["users.show"] != route {
		t.Fatalf("named route not registered through the registrar: %v", reg.names)
	}

	route.Name("users.detail")
	if _, stale := reg.names["users.show"]; stale || reg.names["users.detail"] != route {
		t.Fatalf("rename did not swap the index: %v", reg.names)
	}
	route.Name("")
	if len(reg.names) != 0 {
		t.Fatalf("Name(\"\") should deregister: %v", reg.names)
	}

	reg.frozen = true
	defer func() {
		if recovered := recover(); recovered != "frozen: Group.Middleware" {
			t.Fatalf("recovered = %v, want the freeze guard from the registrar", recovered)
		}
	}()
	api.Middleware(func(next Handler) Handler { return next })
}

func TestRouteRegistrar_AppSatisfiesSeam(t *testing.T) {
	var _ routeRegistrar = (*App)(nil)
}
