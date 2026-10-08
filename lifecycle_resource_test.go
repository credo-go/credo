package credo_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/credo-go/credo"
)

// counted is a resource that names itself and counts its lifecycle calls.
type counted struct {
	starts atomic.Int32
	stops  atomic.Int32
}

func (r *counted) ResourceIdentity() any { return r }

func (r *counted) Start(context.Context) error {
	r.starts.Add(1)
	return nil
}

func (r *counted) Shutdown(context.Context) error {
	r.stops.Add(1)
	return nil
}

func (*counted) Publish() {}

// countedView is a second binding type over the same *counted.
type countedView interface {
	Start(context.Context) error
	Shutdown(context.Context) error
}

// countedWrapper embeds the resource, so it shows Start and Shutdown and
// carries the resource's identity.
type countedWrapper struct{ *counted }

func (r *counted) check(t *testing.T, starts, stops int32) {
	t.Helper()
	if got := r.starts.Load(); got != starts {
		t.Errorf("Start calls = %d, want %d", got, starts)
	}
	if got := r.stops.Load(); got != stops {
		t.Errorf("Shutdown calls = %d, want %d", got, stops)
	}
}

// TestComponents_SharedResourceStartsOnce pins one start per resource: the
// holders that share an identity are started once, as they are shut down
// once.
func TestComponents_SharedResourceStartsOnce(t *testing.T) {
	cases := []struct {
		name string
		bind func(app *credo.App)
	}{
		{"same pointer under a second type", func(app *credo.App) {
			app.Provide[countedView](func(r *counted) countedView { return r })
		}},
		{"wrapper that embeds the resource", func(app *credo.App) {
			app.Provide[countedWrapper](func(r *counted) countedWrapper { return countedWrapper{r} })
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := mustNew(t)
			r := &counted{}
			app.ProvideValue(r)
			tc.bind(app)
			startComponents(t, app)
			if err := shutdownApp(t, app, time.Second); err != nil {
				t.Fatalf("Shutdown() = %v", err)
			}
			r.check(t, 1, 1)
		})
	}
}

// TestComponents_BorrowedResourceIsNeverStarted pins that a borrowed
// resource is the caller's to start as well as to stop, through every holder.
func TestComponents_BorrowedResourceIsNeverStarted(t *testing.T) {
	app := mustNew(t)
	r := &counted{}
	app.ProvideValue(r, credo.Borrowed())
	app.Provide[countedWrapper](func(r *counted) countedWrapper { return countedWrapper{r} })
	startComponents(t, app)
	if err := shutdownApp(t, app, time.Second); err != nil {
		t.Fatalf("Shutdown() = %v", err)
	}
	r.check(t, 0, 0)
}

// countedUser consumes the resource and is a component of its own.
type countedUser struct {
	r     *counted
	stops *atomic.Int32
	// resourceOpen records whether the resource was still open when the
	// consumer stopped.
	resourceOpen atomic.Bool
}

func (u *countedUser) Shutdown(context.Context) error {
	u.resourceOpen.Store(u.r.stops.Load() == 0)
	u.stops.Add(1)
	return nil
}

type publisher interface{ Publish() }

// TestComponents_SharedResourceKeepsDependencyOrder pins a holder that
// obtains the resource through one of its consumers: the holder's route to
// the resource is no reason to keep that consumer open, so the consumer
// stops first and the resource once, after it.
func TestComponents_SharedResourceKeepsDependencyOrder(t *testing.T) {
	app := mustNew(t)
	r := &counted{}
	var userStops atomic.Int32
	var user *countedUser
	app.ProvideValue(r)
	app.Provide[*countedUser](func(r *counted) *countedUser {
		user = &countedUser{r: r, stops: &userStops}
		return user
	})
	// The interface view is the resource itself, reached through its
	// consumer; it has no state or teardown of its own.
	app.Provide[publisher](func(u *countedUser) publisher { return u.r })
	if err := app.Finalize(); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Resolve[publisher](); err != nil {
		t.Fatal(err)
	}
	if err := shutdownApp(t, app, time.Second); err != nil {
		t.Fatalf("Shutdown() = %v", err)
	}
	if got := userStops.Load(); got != 1 {
		t.Errorf("consumer Shutdown calls = %d, want 1", got)
	}
	if !user.resourceOpen.Load() {
		t.Error("the consumer stopped after the resource it uses")
	}
	r.check(t, 0, 1)
}

// badIdentity has a ResourceIdentity the App cannot use.
type badIdentity struct{ stops *atomic.Int32 }

func (*badIdentity) ResourceIdentity() any { return []int{1} }

func (b *badIdentity) Shutdown(context.Context) error {
	b.stops.Add(1)
	return nil
}

// TestComponents_RejectedValueIsReleased pins that a value the App refuses
// after its constructor built it — its identity is unusable, so no other
// holder can share it — is still the App's to release.
func TestComponents_RejectedValueIsReleased(t *testing.T) {
	app := mustNew(t)
	var stops atomic.Int32
	app.Provide[*badIdentity](func() *badIdentity { return &badIdentity{stops: &stops} })
	if err := app.Finalize(); err != nil {
		t.Fatal(err)
	}
	_, err := app.Resolve[*badIdentity]()
	if err == nil || !strings.Contains(err.Error(), "resource identity") {
		t.Fatalf("Resolve() = %v, want the identity error", err)
	}
	if err := shutdownApp(t, app, time.Second); err != nil {
		t.Fatalf("Shutdown() = %v", err)
	}
	if got := stops.Load(); got != 1 {
		t.Fatalf("rejected value Shutdown calls = %d, want 1", got)
	}
}

// borrowedConn is a resource released with Close.
type borrowedConn struct{ closes atomic.Int32 }

func (c *borrowedConn) Close() error {
	c.closes.Add(1)
	return nil
}

type connCloser interface{ Close() error }

// TestComponents_ConflictingHolderIsNotReleased pins the other side: a value
// refused because it shares a resource whose holders disagree is that
// resource, owned elsewhere, so the App does not release it.
func TestComponents_ConflictingHolderIsNotReleased(t *testing.T) {
	app := mustNew(t)
	conn := &borrowedConn{}
	app.ProvideValue(conn, credo.Borrowed())
	app.Provide[connCloser](func(c *borrowedConn) connCloser { return c }, credo.Closer())
	if err := app.Finalize(); err != nil {
		t.Fatal(err)
	}
	_, err := app.Resolve[connCloser]()
	if err == nil || !strings.Contains(err.Error(), "disagree on its owner") {
		t.Fatalf("Resolve() = %v, want the owner conflict", err)
	}
	if err := shutdownApp(t, app, time.Second); err != nil {
		t.Fatalf("Shutdown() = %v", err)
	}
	if got := conn.closes.Load(); got != 0 {
		t.Fatalf("borrowed resource Close calls = %d, want 0", got)
	}
}

// groupedPool and groupedService are two resources, each held through a
// second view, where a holder of the pool reaches it through the service.
type groupedPool struct{ stops atomic.Int32 }

func (p *groupedPool) Shutdown(context.Context) error { p.stops.Add(1); return nil }
func (*groupedPool) PublishGrouped()                  {}

type groupedPublisher interface{ PublishGrouped() }

type groupedService struct {
	pool        *groupedPool
	stops       atomic.Int32
	sawPoolOpen atomic.Bool
}

func (s *groupedService) Shutdown(context.Context) error {
	s.sawPoolOpen.Store(s.pool.stops.Load() == 0)
	s.stops.Add(1)
	return nil
}

func (*groupedService) ServeGrouped() {}

type groupedServiceView interface{ ServeGrouped() }

// TestComponents_SharedGroupsKeepARealDependency pins that only a holder's
// route to its resource is dropped from the drain order: the service's own
// dependency on the pool holds, whichever is registered first, although a
// holder of the pool reaches it through the service.
func TestComponents_SharedGroupsKeepARealDependency(t *testing.T) {
	for _, serviceFirst := range []bool{true, false} {
		name := "pool registered first"
		if serviceFirst {
			name = "service registered first"
		}
		t.Run(name, func(t *testing.T) {
			app := mustNew(t)
			pool := &groupedPool{}
			registerService := func() {
				app.Provide[*groupedService](func(p *groupedPool) *groupedService {
					return &groupedService{pool: p}
				})
			}
			if serviceFirst {
				registerService()
			}
			app.ProvideValue(pool)
			if !serviceFirst {
				registerService()
			}
			app.Provide[groupedServiceView](func(s *groupedService) groupedServiceView { return s })
			app.Provide[groupedPublisher](func(s *groupedService) groupedPublisher { return s.pool })
			if err := app.Finalize(); err != nil {
				t.Fatal(err)
			}
			if _, err := app.Resolve[groupedServiceView](); err != nil {
				t.Fatal(err)
			}
			if _, err := app.Resolve[groupedPublisher](); err != nil {
				t.Fatal(err)
			}
			service := app.MustResolve[*groupedService]()
			if err := shutdownApp(t, app, time.Second); err != nil {
				t.Fatalf("Shutdown() = %v", err)
			}
			if pool.stops.Load() != 1 || service.stops.Load() != 1 {
				t.Fatalf("Shutdown calls: pool=%d service=%d, want one each", pool.stops.Load(), service.stops.Load())
			}
			if !service.sawPoolOpen.Load() {
				t.Fatal("the pool was shut down before the service that depends on it")
			}
		})
	}
}
