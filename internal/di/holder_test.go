package di_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/credo-go/credo/internal/di"
)

// holderRes is a resource with Ready and a Shutdown that fails, so a
// teardown report names the holder that ran it.
type holderRes struct{ ev *events }

func (*holderRes) Ready(context.Context) error { return nil }

func (r *holderRes) Shutdown(context.Context) error {
	r.ev.add("shutdown")
	return errors.New("release failed")
}

// holderWrap embeds the resource: one resource with it.
type holderWrap struct{ *holderRes }

func (w holderWrap) ResourceIdentity() any { return w.holderRes }

// failedTeardowns returns the names of the units whose teardown failed.
func failedTeardowns(t *testing.T, err error) []string {
	t.Helper()
	report, ok := errors.AsType[*di.ShutdownError](err)
	if !ok {
		t.Fatalf("Shutdown() = %v, want a *ShutdownError", err)
	}
	var names []string
	for _, e := range report.Entries {
		if e.State == di.ShutdownFailed {
			names = append(names, e.Name)
		}
	}
	return names
}

// buildAll builds every unit of the start plan, in order.
func buildAll(t *testing.T, c *di.Container) error {
	t.Helper()
	for _, tier := range []di.Tier{di.TierInternal, di.TierIngress} {
		for _, u := range c.StartPlan(tier) {
			if _, err := c.BuildUnit(u); err != nil {
				return err
			}
		}
	}
	return nil
}

// TestHolderOfRecord_RegistrationEvenWhenTheConstructorComesFirst: a value a
// constructor handed to Manage builds claims nothing: the binding of its
// resource gives the one readiness entry and runs the one teardown, under
// its name, though the constructor was registered first.
func TestHolderOfRecord_RegistrationEvenWhenTheConstructorComesFirst(t *testing.T) {
	ev := &events{}
	res := &holderRes{ev: ev}
	c := di.New()
	if _, err := c.Manage(func(r *holderRes) *holderRes { return r }, di.Options{}); err != nil {
		t.Fatal(err)
	}
	if err := c.ProvideWith(func() *holderRes { return res }, di.Options{Name: "res"}); err != nil {
		t.Fatal(err)
	}
	seal(t, c)
	if err := buildAll(t, c); err != nil {
		t.Fatalf("build: %v", err)
	}
	entries := c.ReadinessEntries()
	if len(entries) != 1 || entries[0].Name != "res" || entries[0].Unit.Managed() {
		t.Fatalf("ReadinessEntries() = %+v, want the binding's one entry, res", entries)
	}
	if got := failedTeardowns(t, c.Shutdown(t.Context())); !slices.Equal(got, []string{"res"}) {
		t.Errorf("teardowns = %q, want the binding's alone", got)
	}
	if got := ev.list(); len(got) != 1 {
		t.Errorf("events = %v, want one teardown", got)
	}
}

// TestHolderOfRecord_FirstHolderWithoutARegistration: two constructors that
// return one resource they did not create give one entry and one teardown,
// the first registered's.
func TestHolderOfRecord_FirstHolderWithoutARegistration(t *testing.T) {
	ev := &events{}
	res := &holderRes{ev: ev}
	c := di.New()
	if _, err := c.Manage(func() *holderRes { return res }, di.Options{Name: "first"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Manage(func() holderWrap { return holderWrap{res} }, di.Options{}); err != nil {
		t.Fatal(err)
	}
	seal(t, c)
	if err := buildAll(t, c); err != nil {
		t.Fatalf("build: %v", err)
	}
	if entries := c.ReadinessEntries(); len(entries) != 1 || entries[0].Name != "first" {
		t.Fatalf("ReadinessEntries() = %+v, want one entry, first", entries)
	}
	if got := failedTeardowns(t, c.Shutdown(t.Context())); !slices.Equal(got, []string{"first"}) {
		t.Errorf("teardowns = %q, want the first holder's alone", got)
	}
}

// TestHolderOfRecord_DeferringValueRefusesOptions: a constructor handed to
// Manage with an option its value cannot apply, since it defers to the
// binding of its resource, fails its build naming the binding and the
// remedies.
func TestHolderOfRecord_DeferringValueRefusesOptions(t *testing.T) {
	for _, tc := range []struct {
		name  string
		o     di.Options
		given string
	}{
		{"name", di.Options{Name: "wrap"}, "credo.ComponentName"},
		{"ingress", di.Options{Ingress: true}, "credo.Ingress()"},
		{"optional", di.Options{OptionalReadiness: true}, "credo.OptionalReadiness()"},
		{"all three", di.Options{Name: "wrap", Ingress: true, OptionalReadiness: true},
			"credo.ComponentName, credo.Ingress() and credo.OptionalReadiness()"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := di.New()
			c.MustProvideValue[*holderRes](&holderRes{ev: &events{}})
			if _, err := c.Manage(func(r *holderRes) holderWrap { return holderWrap{r} }, tc.o); err != nil {
				t.Fatal(err)
			}
			seal(t, c)
			err := buildAll(t, c)
			want := "di_test.holderWrap shares the resource of the ProvideValue binding of *di_test.holderRes, " +
				"whose readiness and shutdown follow that binding; give " + tc.given + " to that binding, or " +
				"hold the *di_test.holderRes in a named field"
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("build = %v\nwant it to contain %q", err, want)
			}
		})
	}
	t.Run("without options", func(t *testing.T) {
		c := di.New()
		c.MustProvideValue[*holderRes](&holderRes{ev: &events{}})
		if _, err := c.Manage(func(r *holderRes) holderWrap { return holderWrap{r} }, di.Options{}); err != nil {
			t.Fatal(err)
		}
		seal(t, c)
		if err := buildAll(t, c); err != nil {
			t.Fatalf("build = %v, want a value that defers to start", err)
		}
	})
}

// TestHolderOfRecord_RegistrationsAgreeOnTheTier: two values handed in that
// hold one resource in different tiers are refused at the second, naming
// both, their tiers and the remedies; a borrowed resource is exempt.
func TestHolderOfRecord_RegistrationsAgreeOnTheTier(t *testing.T) {
	res := &holderRes{ev: &events{}}
	c := di.New()
	c.MustProvideValue[*holderRes](res)
	err := c.ProvideValueWith[holderWrap](holderWrap{res}, di.Options{Ingress: true})
	const want = "*di_test.holderRes and di_test.holderWrap hold one resource, which has one teardown, but are " +
		"placed in different tiers: *di_test.holderRes is internal and di_test.holderWrap is ingress; give them " +
		"one tier, or, when one of them has a teardown of its own to run in its tier, hold the handle in a " +
		"named field, so that it is a resource of its own"
	if reason := misuseReason(t, err); reason != want {
		t.Fatalf("reason = %q\nwant      %q", reason, want)
	}

	c = di.New()
	if err := c.ProvideValueWith[*holderRes](res, di.Options{Borrowed: true}); err != nil {
		t.Fatal(err)
	}
	if err := c.ProvideWith(func(r *holderRes) holderWrap { return holderWrap{r} },
		di.Options{Ingress: true}); err != nil {
		t.Fatal(err)
	}
	seal(t, c)
	if _, err := c.Resolve[holderWrap](); err != nil {
		t.Fatalf("a borrowed resource is exempt from the tier check: %v", err)
	}
}

// TestAddEdge_OrdersStartAndDrain: an edge between two units that are not
// bindings builds its target first and stops its source first, whatever
// their registration order.
func TestAddEdge_OrdersStartAndDrain(t *testing.T) {
	ev := &events{}
	c := di.New()
	loop, err := c.Manage(func() *unitPool { return &unitPool{ev: ev, name: "loop"} }, di.Options{Name: "loop"})
	if err != nil {
		t.Fatal(err)
	}
	value, err := c.Manage(func() *unitPool { return &unitPool{ev: ev, name: "value"} }, di.Options{Name: "value"})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.AddEdge(loop, value); err != nil {
		t.Fatal(err)
	}
	seal(t, c)
	plan := c.StartPlan(di.TierInternal)
	if len(plan) != 2 || plan[0] != value || plan[1] != loop {
		t.Fatalf("StartPlan = %v, want value before loop", plan)
	}
	if err := buildAll(t, c); err != nil {
		t.Fatal(err)
	}
	if err := c.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Without the edge the later registered stops first; the edge reverses it.
	if got, want := ev.list(), []string{"shutdown loop", "shutdown value"}; !slices.Equal(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

// TestAddEdge_Misuse: an edge joins two registered units that are not
// bindings, before Finalize.
func TestAddEdge_Misuse(t *testing.T) {
	newContainer := func(t *testing.T) (*di.Container, *di.Unit, *di.Unit) {
		c := di.New()
		a, err := c.Manage(func() *unitPool { return &unitPool{ev: &events{}} }, di.Options{Name: "a"})
		if err != nil {
			t.Fatal(err)
		}
		c.MustProvide(func() *unitRepoComp { return &unitRepoComp{} })
		var binding *di.Unit
		for _, u := range c.Units() {
			if !u.Managed() {
				binding = u
			}
		}
		return c, a, binding
	}
	t.Run("a binding", func(t *testing.T) {
		c, a, binding := newContainer(t)
		want := "an edge joins two units that are not bindings; a binding's edges are its constructor's parameters"
		if reason := misuseReason(t, c.AddEdge(a, binding)); reason != want {
			t.Errorf("reason = %q, want %q", reason, want)
		}
	})
	t.Run("itself", func(t *testing.T) {
		c, a, _ := newContainer(t)
		if reason := misuseReason(t, c.AddEdge(a, a)); reason != "a unit cannot depend on itself" {
			t.Errorf("reason = %q", reason)
		}
	})
	t.Run("another container's unit", func(t *testing.T) {
		c, a, _ := newContainer(t)
		_, other, _ := newContainer(t)
		if reason := misuseReason(t, c.AddEdge(a, other)); reason != "both units must be registered in this container" {
			t.Errorf("reason = %q", reason)
		}
	})
	t.Run("after Finalize", func(t *testing.T) {
		c, a, _ := newContainer(t)
		b, err := c.Manage(func() *unitPool { return &unitPool{ev: &events{}} }, di.Options{Name: "b"})
		if err != nil {
			t.Fatal(err)
		}
		seal(t, c)
		if reason := misuseReason(t, c.AddEdge(a, b)); !strings.Contains(reason, "called after Finalize") {
			t.Errorf("reason = %q", reason)
		}
	})
}

// TestManageNameFree: the name check of a registration that adds several
// units, as Manage would refuse it.
func TestManageNameFree(t *testing.T) {
	c := di.New()
	if err := c.ManageNameFree("worker:x"); err != nil {
		t.Fatalf("ManageNameFree on a free name = %v", err)
	}
	if _, err := c.Manage(&unitPool{ev: &events{}}, di.Options{Name: "worker:x"}); err != nil {
		t.Fatal(err)
	}
	const want = `a component named "worker:x" is already registered, as Manage(worker:x); give each component ` +
		"its own name with credo.ComponentName"
	if reason := misuseReason(t, c.ManageNameFree("worker:x")); reason != want {
		t.Fatalf("reason = %q, want %q", reason, want)
	}
}

// TestManage_WorkerValueNeedNotBeAComponent: the value a worker's
// constructor builds is added whatever its result type shows; any other
// constructor handed to Manage must return a component.
func TestManage_WorkerValueNeedNotBeAComponent(t *testing.T) {
	c := di.New()
	if _, err := c.Manage(func() *noClose { return &noClose{} },
		di.Options{Name: "worker-value:x", Worker: `Workers.Continuous("x")`}); err != nil {
		t.Fatalf("Manage of a worker's constructor = %v", err)
	}
	_, err := c.Manage(func() *noClose { return &noClose{} }, di.Options{})
	if reason := misuseReason(t, err); !strings.Contains(reason, "no Shutdown") {
		t.Fatalf("reason = %q, want the refusal of a constructor that returns no component", reason)
	}
}
