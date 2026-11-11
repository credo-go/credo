package credo_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/credo-go/credo"
)

// tierGate is an ingress component by its type: its Shutdown waits at a
// barrier the other ingress components of the test reach too.
type tierGate struct{ barrier func(context.Context) error }

func (g *tierGate) Shutdown(ctx context.Context) error { return g.barrier(ctx) }
func (*tierGate) IngressTier()                         {}

// tierGateB is a second type of the same kind, so two gates can be bound.
type tierGateB struct{ tierGate }

var _ credo.IngressComponent = (*tierGate)(nil)

// concurrentBarrier returns a Shutdown that completes only when n callers
// reach it at once: the ingress tier stops its components concurrently,
// the internal tier one at a time.
func concurrentBarrier(n int) func(context.Context) error {
	var arrived sync.WaitGroup
	arrived.Add(n)
	all := make(chan struct{})
	go func() { arrived.Wait(); close(all) }()
	return func(ctx context.Context) error {
		arrived.Done()
		select {
		case <-all:
			return nil
		case <-ctx.Done():
			return errors.New("the components were stopped one at a time, not in the ingress tier")
		}
	}
}

// TestIngressComponent_TypeDeclaresTheTier: a component whose type
// implements credo.IngressComponent is planned in the ingress tier without
// credo.Ingress(), which stays accepted, whether it is managed, bound as a
// value or built by a constructor.
func TestIngressComponent_TypeDeclaresTheTier(t *testing.T) {
	for _, tc := range []struct {
		name     string
		register func(app *credo.App, barrier func(context.Context) error)
	}{
		{"Manage", func(app *credo.App, b func(context.Context) error) {
			app.Manage(&tierGate{b})
			app.Manage(&tierGateB{tierGate{b}})
		}},
		{"Manage with Ingress", func(app *credo.App, b func(context.Context) error) {
			app.Manage(&tierGate{b}, credo.Ingress())
			app.Manage(&tierGateB{tierGate{b}})
		}},
		{"ProvideValue", func(app *credo.App, b func(context.Context) error) {
			app.ProvideValue(&tierGate{b})
			app.ProvideValue(&tierGateB{tierGate{b}})
		}},
		{"Provide", func(app *credo.App, b func(context.Context) error) {
			app.Provide(func() *tierGate { return &tierGate{b} })
			app.Provide(func() *tierGateB { return &tierGateB{tierGate{b}} })
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := mustNew(t)
			tc.register(app, concurrentBarrier(2))
			startComponents(t, app)
			if err := shutdownApp(t, app, 2*time.Second); err != nil {
				t.Fatalf("Shutdown() = %v", err)
			}
		})
	}
}

// tierConsumer is an internal component that uses an ingress one.
type tierConsumer struct{ gate *tierGate }

func (*tierConsumer) Shutdown(context.Context) error { return nil }

// TestIngressComponent_InternalDependentFailsFinalize: the tier a type
// declares is known before anything is built, so Finalize refuses an
// internal component that depends on it before any constructor runs.
func TestIngressComponent_InternalDependentFailsFinalize(t *testing.T) {
	app := mustNew(t)
	var built []string
	app.Provide(func() *tierGate { built = append(built, "gate"); return &tierGate{} })
	app.Provide(func(g *tierGate) *tierConsumer {
		built = append(built, "consumer")
		return &tierConsumer{gate: g}
	})
	err := app.Finalize()
	if len(built) != 0 {
		t.Errorf("Finalize built %v; the tier is known from the type", built)
	}
	const want = "di: internal component *credo_test.tierConsumer depends on the ingress component " +
		"*credo_test.tierGate: *credo_test.tierConsumer → *credo_test.tierGate; declare " +
		"*credo_test.tierConsumer ingress with credo.Ingress(), or split *credo_test.tierGate so that what " +
		"internal components use is an internal part it depends on"
	if err == nil || err.Error() != want {
		t.Fatalf("Finalize() = %v\nwant %s", err, want)
	}
}

// tierView is an interface that hides the tier of what it binds.
type tierView interface{ Shutdown(context.Context) error }

// TestIngressComponent_HiddenTierIsRefused: a binding whose type hides the
// IngressTier of its value is refused, as a hidden Start is: ProvideValue
// panics at the call, a constructor's value fails the start.
func TestIngressComponent_HiddenTierIsRefused(t *testing.T) {
	t.Run("ProvideValue", func(t *testing.T) {
		app := mustNew(t)
		want := "credo: App.ProvideValue[credo_test.tierView]: the binding's type credo_test.tierView hides " +
			"IngressTier of its value, a *credo_test.tierGate, and the App plans Start, Ready and the ingress " +
			"tier only from the binding's type; bind the value as *credo_test.tierGate and the interface " +
			"with app.Alias[credo_test.tierView, *credo_test.tierGate](), or give credo.View() when another " +
			"registration owns the value, or credo.Borrowed() when the caller starts and stops it"
		defer func() {
			if r := recover(); fmt.Sprint(r) != want {
				t.Fatalf("panic = %v\nwant  %s", r, want)
			}
		}()
		app.ProvideValue[tierView](&tierGate{})
	})
	t.Run("Provide", func(t *testing.T) {
		app := mustNew(t)
		app.Provide(func() tierView { return &tierGate{} })
		if err := app.Finalize(); err != nil {
			t.Fatal(err)
		}
		_, err := app.Resolve[tierView]()
		const want = "the constructor of credo_test.tierView returns a *credo_test.tierGate, whose " +
			"IngressTier the binding's type hides, and the App plans Start, Ready and the ingress tier only " +
			"from the binding's type; declare the constructor to return *credo_test.tierGate and bind " +
			"credo_test.tierView with app.Alias[credo_test.tierView, *credo_test.tierGate](), or give " +
			"credo.View() when another registration owns the value"
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("Resolve() = %v\nwant it to contain %s", err, want)
		}
	})
}
