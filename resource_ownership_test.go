package credo_test

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/testutil"
)

// ownRes is a resource with a lifecycle that names itself as its identity,
// so a value that embeds it holds the same resource.
type ownRes struct {
	j    *journal
	name string
}

func newOwnRes(j *journal, name string) *ownRes { return &ownRes{j: j, name: name} }

func (r *ownRes) ResourceIdentity() any { return r }

func (r *ownRes) Start(context.Context) error {
	r.j.add("start:" + r.name)
	return nil
}

func (*ownRes) Ready(context.Context) error { return nil }

func (r *ownRes) Shutdown(context.Context) error {
	r.j.add("stop:" + r.name)
	return nil
}

func (*ownRes) Query() {}

// ownWrap embeds the resource and keeps no state of its own.
type ownWrap struct{ *ownRes }

// ownQuerier is an adapter's view of the resource: it shows Query alone.
type ownQuerier interface{ Query() }

// ownLifecycle shows every capability of the resource.
type ownLifecycle interface {
	credo.Component
	credo.Starter
	credo.Readier
	Query()
}

// ownUser consumes a view and records its own start and stop, so the order
// against the resource shows.
type ownUser struct{ j *journal }

func (u *ownUser) Start(context.Context) error {
	u.j.add("start:user")
	return nil
}

func (u *ownUser) Shutdown(context.Context) error {
	u.j.add("stop:user")
	return nil
}

// ownServe registers, starts the App with health installed, runs each of
// after, reads the /ready entries and shuts the App down.
func ownServe(t *testing.T, register func(app *credo.App), after ...func(app *credo.App)) []string {
	t.Helper()
	app := mustNew(t)
	register(app)
	app.UseHealth()
	startComponents(t, app)
	for _, fn := range after {
		fn(app)
	}
	names := readyNames(getReady(t, app))
	if err := shutdownApp(t, app, 2*time.Second); err != nil {
		t.Fatalf("Shutdown() = %v", err)
	}
	return names
}

// ownEvents fails the test unless the journal holds want, in order.
func ownEvents(t *testing.T, j *journal, want ...string) {
	t.Helper()
	if got := j.list(); !slices.Equal(got, want) {
		t.Errorf("events = %q, want %q", got, want)
	}
}

// ownStartFailure fails the test unless err is a failed start with an
// entry named name whose error contains want.
func ownStartFailure(t *testing.T, err error, name, want string) {
	t.Helper()
	entry := lifecycleEntry(t, err, name)
	if entry.Phase != credo.PhaseStart || entry.Err == nil || !strings.Contains(entry.Err.Error(), want) {
		t.Fatalf("entry = %+v\nwant a start failure containing %q", entry, want)
	}
}

// ownOrders are the two registration orders of an owner and its view.
var ownOrders = []string{"owner first", "view first"}

// TestOwnership_WrapperBesideItsHandle: a wrapper that embeds a bound handle,
// declared a view — bound with ProvideValue or built by a constructor,
// before the handle or after it — leaves the handle one start, one readiness
// entry under the handle's name and one teardown, after a consumer of the
// view. Undeclared, the second of the two is refused, naming the first as
// the owner and both remedies; a constructor's wrapper fails the start.
func TestOwnership_WrapperBesideItsHandle(t *testing.T) {
	for _, form := range []string{"ProvideValue", "constructor"} {
		for _, order := range ownOrders {
			t.Run("declared, "+form+", "+order, func(t *testing.T) {
				j := &journal{}
				r := newOwnRes(j, "pool")
				owner := func(app *credo.App) { app.ProvideValue(r, credo.ComponentName("pool")) }
				view := func(app *credo.App) {
					if form == "ProvideValue" {
						app.ProvideValue(ownWrap{r}, credo.View())
						return
					}
					app.Provide(func(r *ownRes) ownWrap { return ownWrap{r} }, credo.View())
				}
				ready := ownServe(t, func(app *credo.App) {
					if order == "owner first" {
						owner(app)
						view(app)
					} else {
						view(app)
						owner(app)
					}
					app.Provide(func(ownWrap) *ownUser { return &ownUser{j: j} })
				})
				if !slices.Equal(ready, []string{"pool"}) {
					t.Errorf("/ready entries = %q, want the owner's one entry", ready)
				}
				ownEvents(t, j, "start:pool", "start:user", "stop:user", "stop:pool")
			})
		}
	}
	t.Run("undeclared, handle first", func(t *testing.T) {
		app := mustNew(t)
		r := newOwnRes(&journal{}, "pool")
		app.ProvideValue(r)
		registerWant(t, mustPanicMessage(t, func() { app.ProvideValue(ownWrap{r}) }),
			"credo: App.ProvideValue[credo_test.ownWrap]: credo_test.ownWrap holds the resource that the "+
				"ProvideValue binding of *credo_test.ownRes owns, and a resource has one owner; declare "+
				"credo_test.ownWrap a view: give credo.View() to its registration, or, when credo_test.ownWrap "+
				"releases state of its own, hold the *credo_test.ownRes in a named field of credo_test.ownWrap, so "+
				"that it is a resource of its own")
	})
	t.Run("undeclared, wrapper first", func(t *testing.T) {
		app := mustNew(t)
		r := newOwnRes(&journal{}, "pool")
		app.ProvideValue(ownWrap{r})
		registerWant(t, mustPanicMessage(t, func() { app.ProvideValue(r) }),
			"credo: App.ProvideValue[*credo_test.ownRes]: *credo_test.ownRes holds the resource that the "+
				"ProvideValue binding of credo_test.ownWrap owns, and a resource has one owner; declare "+
				"*credo_test.ownRes a view: give credo.View() to its registration, or, when credo_test.ownWrap "+
				"releases state of its own, hold the *credo_test.ownRes in a named field of credo_test.ownWrap, so "+
				"that it is a resource of its own")
	})
	for _, order := range ownOrders {
		t.Run("undeclared constructor, "+order, func(t *testing.T) {
			app := mustNew(t)
			r := newOwnRes(&journal{}, "pool")
			wrap := func() { app.Provide(func(r *ownRes) ownWrap { return ownWrap{r} }) }
			if order == "owner first" {
				app.ProvideValue(r)
				wrap()
			} else {
				wrap()
				app.ProvideValue(r)
			}
			ownStartFailure(t, app.Start(t.Context()), "credo_test.ownWrap", "credo_test.ownWrap holds the "+
				"resource that the ProvideValue binding of *credo_test.ownRes owns, and a resource has one owner; "+
				"declare credo_test.ownWrap a view: give credo.View() to its registration, or, when "+
				"credo_test.ownWrap releases state of its own, hold the *credo_test.ownRes in a named field of "+
				"credo_test.ownWrap, so that it is a resource of its own")
		})
	}
}

// TestOwnership_OnePointerTwoBindings: one pointer with a lifecycle under a
// second, interface binding, declared with app.Alias or credo.View() — the
// view before the owner or after it — is started, readied and shut down
// once. Undeclared, the second binding is refused at the call, naming the
// first as the owner: the interface after the pointer is told of both
// app.Alias and credo.View().
func TestOwnership_OnePointerTwoBindings(t *testing.T) {
	for _, tc := range []struct {
		name     string
		register func(app *credo.App, r *ownRes)
	}{
		{"Alias", func(app *credo.App, r *ownRes) {
			app.ProvideValue(r)
			app.Alias[ownLifecycle, *ownRes]()
		}},
		{"View, owner first", func(app *credo.App, r *ownRes) {
			app.ProvideValue(r)
			app.ProvideValue[ownLifecycle](r, credo.View())
		}},
		{"View, view first", func(app *credo.App, r *ownRes) {
			app.ProvideValue[ownLifecycle](r, credo.View())
			app.ProvideValue(r)
		}},
	} {
		t.Run("declared, "+tc.name, func(t *testing.T) {
			j := &journal{}
			r := newOwnRes(j, "pool")
			ready := ownServe(t, func(app *credo.App) {
				tc.register(app, r)
				app.Provide(func(ownLifecycle) *ownUser { return &ownUser{j: j} })
			})
			if !slices.Equal(ready, []string{"*credo_test.ownRes"}) {
				t.Errorf("/ready entries = %q, want the owner's one entry", ready)
			}
			ownEvents(t, j, "start:pool", "start:user", "stop:user", "stop:pool")
		})
	}
	t.Run("undeclared, pointer first", func(t *testing.T) {
		app := mustNew(t)
		r := newOwnRes(&journal{}, "pool")
		app.ProvideValue(r)
		registerWant(t, mustPanicMessage(t, func() { app.ProvideValue[ownLifecycle](r) }),
			"credo: App.ProvideValue[credo_test.ownLifecycle]: credo_test.ownLifecycle holds the resource that "+
				"the ProvideValue binding of *credo_test.ownRes owns, and a resource has one owner; declare "+
				"credo_test.ownLifecycle a view: bind it with app.Alias[credo_test.ownLifecycle, "+
				"*credo_test.ownRes](), or give credo.View() to its registration")
	})
	t.Run("undeclared, interface first", func(t *testing.T) {
		app := mustNew(t)
		r := newOwnRes(&journal{}, "pool")
		app.ProvideValue[ownLifecycle](r)
		registerWant(t, mustPanicMessage(t, func() { app.ProvideValue(r) }),
			"credo: App.ProvideValue[*credo_test.ownRes]: *credo_test.ownRes holds the resource that the "+
				"ProvideValue binding of credo_test.ownLifecycle owns, and a resource has one owner; declare "+
				"*credo_test.ownRes a view: give credo.View() to its registration")
	})
}

// TestOwnership_AdapterConstructorView: an interface view returned by an
// adapter's constructor, declared with credo.View() before the owner or
// after it, is started with its owner: a consumer of the view starts after
// the resource and stops before it. Undeclared, nothing plans the adapter,
// so it is refused when it is built — at its first Resolve, or in the start
// walk when a component consumes it.
func TestOwnership_AdapterConstructorView(t *testing.T) {
	adapter := func(app *credo.App, opts ...credo.RegistrationOption) {
		app.Provide(func(r *ownRes) ownQuerier { return r }, opts...)
	}
	for _, order := range ownOrders {
		t.Run("declared, "+order, func(t *testing.T) {
			j := &journal{}
			r := newOwnRes(j, "pool")
			ready := ownServe(t, func(app *credo.App) {
				if order == "owner first" {
					app.ProvideValue(r)
					adapter(app, credo.View())
				} else {
					adapter(app, credo.View())
					app.ProvideValue(r)
				}
				app.Provide(func(ownQuerier) *ownUser { return &ownUser{j: j} })
			})
			if !slices.Equal(ready, []string{"*credo_test.ownRes"}) {
				t.Errorf("/ready entries = %q, want the owner's one entry", ready)
			}
			ownEvents(t, j, "start:pool", "start:user", "stop:user", "stop:pool")
		})
	}
	const want = "credo_test.ownQuerier holds the resource that the ProvideValue binding of *credo_test.ownRes " +
		"owns, and a resource has one owner; declare credo_test.ownQuerier a view: bind it with " +
		"app.Alias[credo_test.ownQuerier, *credo_test.ownRes](), or give credo.View() to its registration"
	for _, order := range ownOrders {
		t.Run("undeclared, "+order+", refused at Resolve", func(t *testing.T) {
			j := &journal{}
			app := mustNew(t)
			if order == "owner first" {
				app.ProvideValue(newOwnRes(j, "pool"))
				adapter(app)
			} else {
				adapter(app)
				app.ProvideValue(newOwnRes(j, "pool"))
			}
			startComponents(t, app)
			if _, err := app.Resolve[ownQuerier](); err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("Resolve() = %v\nwant it to contain %q", err, want)
			}
			if err := shutdownApp(t, app, 2*time.Second); err != nil {
				t.Fatalf("Shutdown() = %v", err)
			}
			ownEvents(t, j, "start:pool", "stop:pool")
		})
	}
	t.Run("undeclared, consumed by a component", func(t *testing.T) {
		app := mustNew(t)
		app.ProvideValue(newOwnRes(&journal{}, "pool"))
		adapter(app)
		app.Provide(func(ownQuerier) *ownUser { return &ownUser{j: &journal{}} })
		if err := app.Start(t.Context()); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("Start() = %v\nwant it to contain %q", err, want)
		}
	})
}

// ownEmbedder is a worker that embeds the resource: it holds the resource
// and its Start, Ready and Shutdown are the resource's.
type ownEmbedder struct{ *ownRes }

func (*ownEmbedder) Run(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

// ownHolder is a worker that holds the resource in a named field.
type ownHolder struct {
	res  *ownRes
	ran  chan struct{}
	once sync.Once
}

func (w *ownHolder) Run(ctx context.Context) error {
	w.once.Do(func() { close(w.ran) })
	<-ctx.Done()
	return nil
}

// TestOwnership_WorkerConstructorValue: the value a worker's constructor or
// a Manage constructor builds is not a binding and cannot be a view. One
// that embeds a bound resource fails the start, registered before the
// binding or after it, naming the named field; one that returns the
// binding's own value names the remedy that drops the second registration.
// A worker that holds the resource in a named field leaves it its owner's.
func TestOwnership_WorkerConstructorValue(t *testing.T) {
	for _, order := range []string{"binding first", "constructor first"} {
		register := func(app *credo.App, r *ownRes, second func()) {
			if order == "binding first" {
				app.ProvideValue(r)
				second()
				return
			}
			second()
			app.ProvideValue(r)
		}
		t.Run("a worker that embeds the resource, "+order, func(t *testing.T) {
			app := mustNew(t)
			register(app, newOwnRes(&journal{}, "pool"), func() {
				app.Workers().Continuous("relay", func(r *ownRes) *ownEmbedder { return &ownEmbedder{r} })
			})
			ownStartFailure(t, app.Start(t.Context()), "worker-value:relay", `the value `+
				`Workers.Continuous("relay") builds, a *credo_test.ownEmbedder, holds the resource that the `+
				"ProvideValue binding of *credo_test.ownRes owns, and a resource has one owner; hold the "+
				"*credo_test.ownRes in a named field of *credo_test.ownEmbedder, so that it is a resource of its own")
		})
		t.Run("a worker that holds the resource in a named field, "+order, func(t *testing.T) {
			j := &journal{}
			var w *ownHolder
			ready := ownServe(t, func(app *credo.App) {
				register(app, newOwnRes(j, "pool"), func() {
					app.Workers().Continuous("relay", func(r *ownRes) *ownHolder {
						w = &ownHolder{res: r, ran: make(chan struct{})}
						return w
					})
				})
			})
			if !slices.Equal(ready, []string{"*credo_test.ownRes"}) {
				t.Errorf("/ready entries = %q, want the owner's one entry", ready)
			}
			ownEvents(t, j, "start:pool", "stop:pool")
		})
		t.Run("a Manage constructor that wraps the resource, "+order, func(t *testing.T) {
			app := mustNew(t)
			register(app, newOwnRes(&journal{}, "pool"), func() {
				app.Manage(func(r *ownRes) ownWrap { return ownWrap{r} }, credo.ComponentName("wrap"))
			})
			ownStartFailure(t, app.Start(t.Context()), "wrap", "the value the constructor handed to "+
				"Manage(wrap) builds, a credo_test.ownWrap, holds the resource that the ProvideValue binding of "+
				"*credo_test.ownRes owns, and a resource has one owner; hold the *credo_test.ownRes in a named "+
				"field of credo_test.ownWrap, so that it is a resource of its own")
		})
		t.Run("a Manage constructor that returns the binding's value, "+order, func(t *testing.T) {
			app := mustNew(t)
			register(app, newOwnRes(&journal{}, "pool"), func() {
				app.Manage(func(r *ownRes) *ownRes { return r }, credo.ComponentName("again"))
			})
			ownStartFailure(t, app.Start(t.Context()), "again", "the value the constructor handed to "+
				"Manage(again) builds, a *credo_test.ownRes, holds the resource that the ProvideValue binding of "+
				"*credo_test.ownRes owns, and a resource has one owner; drop the Manage call, since that binding "+
				"already owns the value")
		})
	}
}

// TestOwnership_ParentManagesChild: a child App handed to its parent's
// Manage is a resource of its own beside a resource both containers hold —
// the parent owns the pool and the child borrows it — so the pool is
// started and shut down once, whichever comes first. The child App handed
// to Manage twice, or bound beside the Manage call, is refused at the
// second call, naming the first as the owner.
func TestOwnership_ParentManagesChild(t *testing.T) {
	for _, order := range []string{"pool first", "child first"} {
		t.Run(order, func(t *testing.T) {
			j := &journal{}
			pool := newOwnRes(j, "pool")
			child := mustNew(t)
			child.ProvideValue(pool, credo.Borrowed())
			ready := ownServe(t, func(parent *credo.App) {
				if order == "pool first" {
					parent.ProvideValue(pool)
				}
				parent.Mount("/c", child)
				parent.Manage(child, credo.ComponentName("child"))
				if order == "child first" {
					parent.ProvideValue(pool)
				}
			})
			if !slices.Equal(ready, []string{"*credo_test.ownRes"}) {
				t.Errorf("/ready entries = %q, want the pool's one entry", ready)
			}
			ownEvents(t, j, "start:pool", "stop:pool")
		})
	}
	t.Run("handed to Manage twice", func(t *testing.T) {
		child := mustNew(t)
		parent := mustNew(t)
		parent.Manage(child)
		registerWant(t, mustPanicMessage(t, func() { parent.Manage(child, credo.ComponentName("again")) }),
			"credo: App.Manage[*credo.App]: Manage(again) holds the resource that the value handed to Manage as "+
				"*credo.App owns, and a resource has one owner; hand each resource to Manage once")
	})
	t.Run("bound, then handed to Manage", func(t *testing.T) {
		child := mustNew(t)
		parent := mustNew(t)
		parent.ProvideValue(child)
		registerWant(t, mustPanicMessage(t, func() { parent.Manage(child) }),
			"credo: App.Manage[*credo.App]: Manage(*credo.App) holds the resource that the ProvideValue binding "+
				"of *credo.App owns, and a resource has one owner; drop the Manage call, since that binding already "+
				"owns it")
	})
	t.Run("handed to Manage, then bound", func(t *testing.T) {
		child := mustNew(t)
		parent := mustNew(t)
		parent.Manage(child)
		registerWant(t, mustPanicMessage(t, func() { parent.ProvideValue(child) }),
			"credo: App.ProvideValue[*credo.App]: *credo.App holds the resource that the value handed to Manage "+
				"as *credo.App owns, and a resource has one owner; declare *credo.App a view: give credo.View() to "+
				"its registration")
	})
}

// ownPlain has no lifecycle and shows three interfaces.
type ownPlain struct{ _ byte }

func (*ownPlain) A() {}
func (*ownPlain) B() {}
func (*ownPlain) C() {}

type (
	ownA interface{ A() }
	ownB interface{ B() }
	ownC interface{ C() }
)

// ownConn is a connection released with Close.
type ownConn struct{ j *journal }

func (c *ownConn) Close() error {
	c.j.add("close:conn")
	return nil
}

func (*ownConn) Exec() {}

type ownExecer interface{ Exec() }

// ownExecUser consumes the connection through ownExecer.
type ownExecUser struct{ j *journal }

func (u *ownExecUser) Shutdown(context.Context) error {
	u.j.add("stop:exec-user")
	return nil
}

// TestOwnership_NoLifecycleManyBindings: one pointer with no lifecycle under
// several interface bindings, the concrete type never bound, is accepted
// with no declaration in any order; so is a value with Close that no
// registration claims, which the App never closes.
func TestOwnership_NoLifecycleManyBindings(t *testing.T) {
	p := &ownPlain{}
	app := mustNew(t)
	app.ProvideValue[ownC](p)
	app.ProvideValue[ownA](p)
	app.ProvideValue[ownB](p)
	startComponents(t, app)
	if app.MustResolve[ownA]() != ownA(p) || app.MustResolve[ownB]() != ownB(p) || app.MustResolve[ownC]() != ownC(p) {
		t.Error("the bindings resolve to different values")
	}
	if app.Has[*ownPlain]() {
		t.Error("the concrete type is bound")
	}
	if err := shutdownApp(t, app, 2*time.Second); err != nil {
		t.Fatalf("Shutdown() = %v", err)
	}

	j := &journal{}
	conn := &ownConn{j: j}
	ownServe(t, func(app *credo.App) {
		app.ProvideValue[ownExecer](conn)
		app.ProvideValue[interface{ Close() error }](conn)
	})
	ownEvents(t, j)
}

// TestOwnership_CloseOwnerAndSecondBinding: one pointer with Close under two
// bindings, one of which carries credo.CloseOnShutdown(), in either order:
// the other claims nothing, so it is a plain holder and needs no
// declaration, though credo.View() is accepted on it; the connection is
// closed once, after a consumer of either binding.
func TestOwnership_CloseOwnerAndSecondBinding(t *testing.T) {
	for _, order := range []string{"owner first", "owner second"} {
		for _, declared := range []bool{false, true} {
			name := order + ", undeclared"
			var opts []credo.RegistrationOption
			if declared {
				name = order + ", declared"
				opts = append(opts, credo.View())
			}
			t.Run(name, func(t *testing.T) {
				j := &journal{}
				conn := &ownConn{j: j}
				ownServe(t, func(app *credo.App) {
					if order == "owner first" {
						app.ProvideValue(conn, credo.CloseOnShutdown())
						app.ProvideValue[ownExecer](conn, opts...)
					} else {
						app.ProvideValue[ownExecer](conn, opts...)
						app.ProvideValue(conn, credo.CloseOnShutdown())
					}
					app.Provide(func(ownExecer) *ownExecUser { return &ownExecUser{j: j} })
				}, func(app *credo.App) { app.MustResolve[*ownExecUser]() })
				ownEvents(t, j, "stop:exec-user", "close:conn")
			})
		}
	}
}

// TestOwnership_ViewTakesNoOwnerOption: credo.View() with an option that
// belongs to a resource's owner panics at the call, on Provide and on
// ProvideValue, naming the option and both remedies.
func TestOwnership_ViewTakesNoOwnerOption(t *testing.T) {
	for _, tc := range []struct {
		option credo.RegistrationOption
		name   string
		value  bool // accepted by ProvideValue alone
	}{
		{credo.Ingress(), "credo.Ingress()", false},
		{credo.ComponentName("pool"), "credo.ComponentName", false},
		{credo.OptionalReadiness(), "credo.OptionalReadiness()", false},
		{credo.CloseOnShutdown(), "credo.CloseOnShutdown()", false},
		{credo.Borrowed(), "credo.Borrowed()", true},
	} {
		const rest = "credo.View() declares credo_test.ownWrap a view of a resource another registration owns, " +
			"and the tier, the name, the readiness entry and the teardown of a resource are its owner's alone; " +
			"give %s to the owner's registration, or drop credo.View()"
		t.Run(tc.name+", ProvideValue", func(t *testing.T) {
			app := mustNew(t)
			r := newOwnRes(&journal{}, "pool")
			registerWant(t, mustPanicMessage(t, func() { app.ProvideValue(ownWrap{r}, credo.View(), tc.option) }),
				"credo: App.ProvideValue[credo_test.ownWrap]: "+strings.Replace(rest, "%s", tc.name, 1))
		})
		if tc.value {
			continue
		}
		t.Run(tc.name+", Provide", func(t *testing.T) {
			app := mustNew(t)
			registerWant(t, mustPanicMessage(t, func() {
				app.Provide(func(r *ownRes) ownWrap { return ownWrap{r} }, credo.View(), tc.option)
			}), "credo: App.Provide[credo_test.ownWrap]: "+strings.Replace(rest, "%s", tc.name, 1))
		})
	}
	t.Run("two options", func(t *testing.T) {
		app := mustNew(t)
		registerWant(t, mustPanicMessage(t, func() {
			app.ProvideValue(ownWrap{newOwnRes(&journal{}, "pool")}, credo.View(), credo.OptionalReadiness(),
				credo.ComponentName("pool"))
		}), "credo: App.ProvideValue[credo_test.ownWrap]: credo.View() declares credo_test.ownWrap a view of a "+
			"resource another registration owns, and the tier, the name, the readiness entry and the teardown of "+
			"a resource are its owner's alone; give credo.ComponentName and credo.OptionalReadiness() to the "+
			"owner's registration, or drop credo.View()")
	})
}

// ownIndex holds the resource without its lifecycle: it names the resource
// as its identity, and has no Shutdown of its own.
type ownIndex struct{ r *ownRes }

func (i ownIndex) ResourceIdentity() any { return i.r }

// ownMissing is never registered.
type ownMissing struct{}

// TestOwnership_ProvideValueViewNeedsAValueOwner: a view's owner exists
// before the view's value, and a view bound with ProvideValue exists before
// anything is built, so Finalize refuses one whose value has Shutdown and
// whose resource no value handed in owns — with no owner, or with a Provide
// owner registered before it or after it — in registration order among the
// other graph errors, naming the remedies; nothing is started. One whose
// owner an override replaced names the override.
func TestOwnership_ProvideValueViewNeedsAValueOwner(t *testing.T) {
	const want = "di: credo.View() declares credo_test.ownWrap a view of a resource another registration owns, " +
		"but no value handed in owns it, and its value, a credo_test.ownWrap, has Shutdown: a view's owner exists " +
		"before the view's value, and a value bound with ProvideValue exists before anything is built, so only a " +
		"value handed in can own its resource; give the owner as a value, with ProvideValue or Manage, or build " +
		"credo_test.ownWrap with a constructor that takes the owner's binding, with credo.View(), in place of " +
		"ProvideValue, so that the owner exists first, or drop credo.View() from credo_test.ownWrap, so that it " +
		"owns the resource"
	for _, owner := range []string{"no owner", "a Provide owner first", "a Provide owner after"} {
		t.Run(owner, func(t *testing.T) {
			j := &journal{}
			r := newOwnRes(j, "pool")
			app := mustNew(t)
			if owner == "a Provide owner first" {
				app.Provide(func() *ownRes { return r })
			}
			app.Provide(func(*ownMissing) *ownHolder { return nil })
			app.ProvideValue(ownWrap{r}, credo.View())
			app.Provide(func(*ownMissing) *ownUser { return nil })
			if owner == "a Provide owner after" {
				app.Provide(func() *ownRes { return r })
			}
			err := app.Finalize()
			joined, ok := err.(interface{ Unwrap() []error })
			if !ok {
				t.Fatalf("Finalize() = %v, want the joined graph errors", err)
			}
			got := joined.Unwrap()
			if len(got) != 3 || !strings.Contains(got[0].Error(), "*credo_test.ownHolder") ||
				got[1].Error() != want || !strings.Contains(got[2].Error(), "*credo_test.ownUser") {
				t.Fatalf("Finalize() = %v\nwant the view's refusal between the two missing dependencies: %q", err, want)
			}
			if err := app.Start(t.Context()); err == nil {
				t.Fatal("Start() = nil, want the Finalize error")
			}
			ownEvents(t, j)
		})
	}
	t.Run("owner replaced", func(t *testing.T) {
		r := newOwnRes(&journal{}, "pool")
		app := mustNew(t)
		app.ProvideValue(r)
		app.ProvideValue(ownWrap{r}, credo.View())
		app.ProvideValue(newOwnRes(&journal{}, "other"), credo.Override())
		const want = "di: credo.View() declares credo_test.ownWrap a view of a resource another registration " +
			"owns, but its owner, the ProvideValue binding of *credo_test.ownRes, was replaced with " +
			"credo.Override(), and a replaced value is not the App's; override credo_test.ownWrap too, so that it " +
			"holds the replacing value, or build credo_test.ownWrap with a constructor that takes the " +
			"*credo_test.ownRes, with credo.View(), in place of ProvideValue, so that it follows the override"
		if err := app.Finalize(); err == nil || err.Error() != want {
			t.Fatalf("Finalize() = %v\nwant %q", err, want)
		}
	})
	t.Run("an interface over the resource is told of Alias", func(t *testing.T) {
		app := mustNew(t)
		app.ProvideValue[ownLifecycle](newOwnRes(&journal{}, "pool"), credo.View())
		const want = "or bind credo_test.ownLifecycle with app.Alias[credo_test.ownLifecycle, *credo_test.ownRes]() " +
			"in place of ProvideValue, so that the owner exists first"
		if err := app.Finalize(); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("Finalize() = %v\nwant it to contain %q", err, want)
		}
	})
	t.Run("a view without Shutdown needs no owner", func(t *testing.T) {
		app := mustNew(t)
		app.ProvideValue[ownA](&ownPlain{}, credo.View())
		startComponents(t, app)
		if err := shutdownApp(t, app, 2*time.Second); err != nil {
			t.Fatalf("Shutdown() = %v", err)
		}
	})
}

// TestOwnership_ProvideViewNeedsItsOwnerFirst: a view a constructor builds
// whose value has Shutdown fails its build when no registration owns its
// resource at that moment — in the start walk, for a component that
// consumes it, or at a Resolve after the start — naming the remedies;
// nothing is started. One whose owner an override replaced names the
// override.
func TestOwnership_ProvideViewNeedsItsOwnerFirst(t *testing.T) {
	const want = "di: constructing credo_test.ownWrap: credo.View() declares credo_test.ownWrap a view of a " +
		"resource another registration owns, but no registration owns it when credo_test.ownWrap is built, and " +
		"its value, a credo_test.ownWrap, has Shutdown: a view's owner exists before the view's value; give the " +
		"owner as a value, with ProvideValue or Manage, or have the constructor of credo_test.ownWrap take the " +
		"owner's binding, so that the owner exists first, or drop credo.View() from credo_test.ownWrap, so that " +
		"it owns the resource"
	t.Run("in the start walk", func(t *testing.T) {
		for _, owner := range []string{"no owner", "a Provide owner built after it"} {
			t.Run(owner, func(t *testing.T) {
				j := &journal{}
				r := newOwnRes(j, "pool")
				app := mustNew(t)
				app.Provide(func(ownWrap) *ownUser { return &ownUser{j: j} })
				app.Provide(func() ownWrap { return ownWrap{r} }, credo.View())
				if owner != "no owner" {
					app.Provide(func() *ownRes { return r })
				}
				ownStartFailure(t, app.Start(t.Context()), "*credo_test.ownUser", want)
				ownEvents(t, j)
			})
		}
	})
	t.Run("at a Resolve after the start", func(t *testing.T) {
		j := &journal{}
		r := newOwnRes(j, "pool")
		app := testutil.NewApp(t, testutil.WithWiring(func(app *credo.App) {
			app.Provide(func() ownWrap { return ownWrap{r} }, credo.View())
		}))
		testutil.Start(t, app)
		if _, err := app.Resolve[ownWrap](); err == nil || err.Error() != want {
			t.Fatalf("Resolve() = %v\nwant %q", err, want)
		}
		ownEvents(t, j)
	})
	t.Run("owner replaced", func(t *testing.T) {
		r := newOwnRes(&journal{}, "pool")
		app := testutil.NewApp(t,
			testutil.WithWiring(func(app *credo.App) {
				app.ProvideValue(r)
				app.Provide(func() ownWrap { return ownWrap{r} }, credo.View())
			}),
			testutil.WithOverride[*ownRes](newOwnRes(&journal{}, "fake")),
		)
		testutil.Start(t, app)
		const want = "di: constructing credo_test.ownWrap: credo.View() declares credo_test.ownWrap a view of a " +
			"resource another registration owns, but its owner, the ProvideValue binding of *credo_test.ownRes, " +
			"was replaced with credo.Override(), and a replaced value is not the App's; override " +
			"credo_test.ownWrap too, so that it holds the replacing value, or have the constructor of " +
			"credo_test.ownWrap take the *credo_test.ownRes, so that it follows the override"
		if _, err := app.Resolve[ownWrap](); err == nil || err.Error() != want {
			t.Fatalf("Resolve() = %v\nwant %q", err, want)
		}
	})
}

// TestOwnership_OwnerAfterItsView: a value a constructor builds that claims
// a resource a view already holds, while no registration owns it, fails its
// build — a Provide binding, a worker constructor's value, a Manage
// constructor's value — whichever was registered first, since the view
// bound with ProvideValue exists before anything is built; each names the
// remedies that fit it. Nothing is started, and the drain releases the
// refused value, which no other registration owns.
func TestOwnership_OwnerAfterItsView(t *testing.T) {
	const view = "the ProvideValue binding of credo_test.ownIndex, a view, already holds, and a view's owner " +
		"exists before the view's value; "
	for _, tc := range []struct {
		name     string
		register func(app *credo.App, r *ownRes)
		entry    string
		want     string
	}{
		{"a Provide owner", func(app *credo.App, r *ownRes) {
			app.Provide(func() *ownRes { return r })
		}, "*credo_test.ownRes", "the Provide binding of *credo_test.ownRes claims the resource that " + view +
			"bind the *credo_test.ownRes with ProvideValue, or build credo_test.ownIndex with a constructor that " +
			"takes the *credo_test.ownRes, with credo.View(), in place of ProvideValue, so that the owner exists " +
			"first"},
		{"a worker constructor's value", func(app *credo.App, r *ownRes) {
			app.Workers().Continuous("relay", func() *ownEmbedder { return &ownEmbedder{r} })
		}, "worker-value:relay", `the value Workers.Continuous("relay") builds, a *credo_test.ownEmbedder, claims ` +
			"the resource that " + view + "bind the *credo_test.ownEmbedder in place of the worker's constructor " +
			"— with ProvideValue, or with Provide and build credo_test.ownIndex with a constructor that takes the " +
			"*credo_test.ownEmbedder, with credo.View(), in place of ProvideValue — and register the worker by its " +
			"run function, the method expression (*credo_test.ownEmbedder).Run, so that the owner exists first"},
		{"a Manage constructor's value", func(app *credo.App, r *ownRes) {
			app.Manage(func() *ownRes { return r }, credo.ComponentName("pool"))
		}, "pool", "the value the constructor handed to Manage(pool) builds, a *credo_test.ownRes, claims the " +
			"resource that " + view + "hand the *credo_test.ownRes to Manage as a value, or bind it in place of the " +
			"Manage call — with ProvideValue, or with Provide and build credo_test.ownIndex with a constructor that " +
			"takes the *credo_test.ownRes, with credo.View(), in place of ProvideValue — so that the owner exists " +
			"first"},
	} {
		for _, order := range []string{"view first", "owner first"} {
			t.Run(tc.name+", "+order, func(t *testing.T) {
				j := &journal{}
				r := newOwnRes(j, "pool")
				app := mustNew(t)
				if order == "owner first" {
					tc.register(app, r)
				}
				app.ProvideValue(ownIndex{r}, credo.View())
				if order == "view first" {
					tc.register(app, r)
				}
				ownStartFailure(t, app.Start(t.Context()), tc.entry, tc.want)
				ownEvents(t, j, "stop:pool")
			})
		}
	}
}

// TestOwnership_ViewStartsAfterItsOwner: a consumer of a view registered
// before both — a view bound with ProvideValue before its owner handed in,
// or a view a constructor builds from the owner's binding, before the owner
// or after it — starts after the owner and stops before it, and /ready
// shows the owner's one entry.
func TestOwnership_ViewStartsAfterItsOwner(t *testing.T) {
	t.Run("a ProvideValue view before its owner handed in", func(t *testing.T) {
		j := &journal{}
		r := newOwnRes(j, "pool")
		ready := ownServe(t, func(app *credo.App) {
			app.Provide(func(ownWrap) *ownUser { return &ownUser{j: j} })
			app.ProvideValue(ownWrap{r}, credo.View())
			app.ProvideValue(r, credo.ComponentName("pool"))
		})
		if !slices.Equal(ready, []string{"pool"}) {
			t.Errorf("/ready entries = %q, want the owner's one entry", ready)
		}
		ownEvents(t, j, "start:pool", "start:user", "stop:user", "stop:pool")
	})
	for _, order := range ownOrders {
		t.Run("a Provide view built from a Provide owner, "+order, func(t *testing.T) {
			j := &journal{}
			r := newOwnRes(j, "pool")
			ready := ownServe(t, func(app *credo.App) {
				app.Provide(func(ownWrap) *ownUser { return &ownUser{j: j} })
				view := func() { app.Provide(func(r *ownRes) ownWrap { return ownWrap{r} }, credo.View()) }
				if order == "view first" {
					view()
				}
				app.Provide(func() *ownRes { return r }, credo.ComponentName("pool"))
				if order == "owner first" {
					view()
				}
			})
			if !slices.Equal(ready, []string{"pool"}) {
				t.Errorf("/ready entries = %q, want the owner's one entry", ready)
			}
			ownEvents(t, j, "start:pool", "start:user", "stop:user", "stop:pool")
		})
	}
}
