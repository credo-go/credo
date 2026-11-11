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

// buildError builds every unit of the start plan and returns the first
// build's failure, failing the test when nothing fails.
func buildError(t *testing.T, c *di.Container) string {
	t.Helper()
	err := buildAll(t, c)
	if err == nil {
		t.Fatal("build = nil, want the refusal of a second owner")
	}
	return err.Error()
}

// TestOwner_ManageConstructorReturningTheBindingsValue: a constructor handed
// to Manage that returns the value a binding owns fails its build, whatever
// the registration order, naming the call, the built type, the owner and the
// remedy; the binding keeps the one entry and the one teardown.
func TestOwner_ManageConstructorReturningTheBindingsValue(t *testing.T) {
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
	const want = "the value the constructor handed to Manage(*di_test.holderRes) builds, a *di_test.holderRes, " +
		"holds the resource that the Provide binding of *di_test.holderRes owns, and a resource has one owner; " +
		"drop the Manage call, since that binding already owns the value"
	if got := buildError(t, c); !strings.Contains(got, want) {
		t.Fatalf("build = %v\nwant it to contain %q", got, want)
	}
	entries := c.ReadinessEntries()
	if len(entries) != 1 || entries[0].Name != "res" || entries[0].Unit.Managed() {
		t.Fatalf("ReadinessEntries() = %+v, want the binding's one entry, res", entries)
	}
	if got := failedTeardowns(t, c.Shutdown(t.Context())); !slices.Equal(got, []string{"res"}) {
		t.Errorf("teardowns = %q, want the owner's alone", got)
	}
	if got := ev.list(); len(got) != 1 {
		t.Errorf("events = %v, want one teardown", got)
	}
}

// TestOwner_FirstBuiltValueOwnsWithoutARegistration: of two constructors
// that return one resource no registration holds, the first built owns it,
// and the second, a wrapper, fails its build naming the named field.
func TestOwner_FirstBuiltValueOwnsWithoutARegistration(t *testing.T) {
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
	const want = "the value the constructor handed to Manage(di_test.holderWrap) builds, a di_test.holderWrap, " +
		"holds the resource that the value the constructor handed to Manage as first builds owns, and a resource " +
		"has one owner; hold the *di_test.holderRes in a named field of di_test.holderWrap, so that it is a " +
		"resource of its own"
	if got := buildError(t, c); !strings.Contains(got, want) {
		t.Fatalf("build = %v\nwant it to contain %q", got, want)
	}
	if entries := c.ReadinessEntries(); len(entries) != 1 || entries[0].Name != "first" {
		t.Fatalf("ReadinessEntries() = %+v, want one entry, first", entries)
	}
	if got := failedTeardowns(t, c.Shutdown(t.Context())); !slices.Equal(got, []string{"first"}) {
		t.Errorf("teardowns = %q, want the owner's alone", got)
	}
}

// TestOwner_ManageConstructorOverABoundHandle: a constructor handed to Manage
// that returns a wrapper over a bound handle fails its build, with or without
// options, naming the named field; there is no option to declare it a view.
func TestOwner_ManageConstructorOverABoundHandle(t *testing.T) {
	for _, tc := range []struct {
		name string
		o    di.Options
	}{
		{"without options", di.Options{}},
		{"with options", di.Options{Name: "wrap", Ingress: true, OptionalReadiness: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := di.New()
			c.MustProvideValue[*holderRes](&holderRes{ev: &events{}})
			if _, err := c.Manage(func(r *holderRes) holderWrap { return holderWrap{r} }, tc.o); err != nil {
				t.Fatal(err)
			}
			seal(t, c)
			want := "a di_test.holderWrap, holds the resource that the ProvideValue binding of *di_test.holderRes " +
				"owns, and a resource has one owner; hold the *di_test.holderRes in a named field of " +
				"di_test.holderWrap, so that it is a resource of its own"
			if got := buildError(t, c); !strings.Contains(got, want) {
				t.Fatalf("build = %v\nwant it to contain %q", got, want)
			}
		})
	}
}

// TestOwner_TheTierIsTheOwners: a second value handed in that claims a
// resource is refused whatever tier it asks for, naming the option a view
// leaves to the owner; a view in the ingress tier is refused at the call;
// and a view over a borrowed owner is accepted, with the owner's one entry.
func TestOwner_TheTierIsTheOwners(t *testing.T) {
	res := &holderRes{ev: &events{}}
	c := di.New()
	c.MustProvideValue[*holderRes](res)
	err := c.ProvideValueWith[holderWrap](holderWrap{res}, di.Options{Ingress: true})
	const want = "di_test.holderWrap holds the resource that the ProvideValue binding of *di_test.holderRes owns, " +
		"and a resource has one owner; declare di_test.holderWrap a view: give credo.View() to its registration " +
		"in place of credo.Ingress(), which only the owner takes, or, when di_test.holderWrap releases state of " +
		"its own, hold the *di_test.holderRes in a named field of di_test.holderWrap, so that it is a resource of " +
		"its own"
	if reason := misuseReason(t, err); reason != want {
		t.Fatalf("reason = %q\nwant      %q", reason, want)
	}
	err = c.ProvideValueWith[holderWrap](holderWrap{res}, di.Options{View: true, Ingress: true})
	const wantView = "credo.View() declares di_test.holderWrap a view of a resource another registration owns, " +
		"and the tier, the name, the readiness entry and the teardown of a resource are its owner's alone; give " +
		"credo.Ingress() to the owner's registration, or drop credo.View()"
	if reason := misuseReason(t, err); reason != wantView {
		t.Fatalf("reason = %q\nwant      %q", reason, wantView)
	}

	c = di.New()
	if err := c.ProvideValueWith[*holderRes](res, di.Options{Borrowed: true, Name: "res"}); err != nil {
		t.Fatal(err)
	}
	if err := c.ProvideWith(func(r *holderRes) holderWrap { return holderWrap{r} },
		di.Options{View: true}); err != nil {
		t.Fatal(err)
	}
	seal(t, c)
	if _, err := c.Resolve[holderWrap](); err != nil {
		t.Fatalf("a view of a borrowed resource: %v", err)
	}
	if entries := c.ReadinessEntries(); len(entries) != 1 || entries[0].Name != "res" {
		t.Fatalf("ReadinessEntries() = %+v, want the borrowed owner's one entry, res", entries)
	}
}

// TestOwner_ViewWithoutIdentity: a view whose value carries no identity is
// refused, since no registration could own what it holds.
func TestOwner_ViewWithoutIdentity(t *testing.T) {
	c := di.New()
	err := c.ProvideValueWith[[]int]([]int{1}, di.Options{View: true})
	const want = "credo.View() declares []int a view of a resource another registration owns, but its value, a " +
		"[]int, carries no resource identity: it is neither a non-nil pointer, a comparable struct nor a " +
		"credo.ResourceIdentifier, so no registration can own what it holds; drop credo.View()"
	if reason := misuseReason(t, err); reason != want {
		t.Fatalf("reason = %q\nwant      %q", reason, want)
	}
}

// TestOwner_SealRefusesAValueViewWithoutAValueOwner: a view bound with
// ProvideValue whose value has Shutdown and whose resource no value handed
// in owns is refused by Seal, naming the remedies — with no owner, or with
// a constructor owner, which is built only after the view exists; one whose
// owner an override replaced names the override; one with an owner handed
// in, before it or after it, is accepted.
func TestOwner_SealRefusesAValueViewWithoutAValueOwner(t *testing.T) {
	const want = "di: credo.View() declares di_test.holderWrap a view of a resource another registration owns, " +
		"but no value handed in owns it, and its value, a di_test.holderWrap, has Shutdown: a view's owner exists " +
		"before the view's value, and a value bound with ProvideValue exists before anything is built, so only a " +
		"value handed in can own its resource; give the owner as a value, with ProvideValue or Manage, or build " +
		"di_test.holderWrap with a constructor that takes the owner's binding, with credo.View(), in place of " +
		"ProvideValue, so that the owner exists first, or drop credo.View() from di_test.holderWrap, so that it " +
		"owns the resource"
	for _, owner := range []string{"no owner", "a constructor owner"} {
		t.Run(owner, func(t *testing.T) {
			res := &holderRes{ev: &events{}}
			c := di.New()
			if err := c.ProvideValueWith[holderWrap](holderWrap{res}, di.Options{View: true}); err != nil {
				t.Fatal(err)
			}
			if owner == "a constructor owner" {
				c.MustProvide(func() *holderRes { return res })
			}
			if err := c.Seal(); err == nil || err.Error() != want {
				t.Fatalf("Seal() = %v\nwant %q", err, want)
			}
		})
	}
	t.Run("owner replaced", func(t *testing.T) {
		res := &holderRes{ev: &events{}}
		c := di.New()
		c.MustProvideValue[*holderRes](res)
		if err := c.ProvideValueWith[holderWrap](holderWrap{res}, di.Options{View: true}); err != nil {
			t.Fatal(err)
		}
		if err := c.ProvideValueWith[*holderRes](&holderRes{ev: &events{}}, di.Options{Override: true}); err != nil {
			t.Fatal(err)
		}
		const want = "di: credo.View() declares di_test.holderWrap a view of a resource another registration " +
			"owns, but its owner, the ProvideValue binding of *di_test.holderRes, was replaced with " +
			"credo.Override(), and a replaced value is not the App's; override di_test.holderWrap too, so that " +
			"it holds the replacing value, or build di_test.holderWrap with a constructor that takes the " +
			"*di_test.holderRes, with credo.View(), in place of ProvideValue, so that it follows the override"
		if err := c.Seal(); err == nil || err.Error() != want {
			t.Fatalf("Seal() = %v\nwant %q", err, want)
		}
	})
	for _, order := range []string{"owner first", "view first"} {
		t.Run("owned, "+order, func(t *testing.T) {
			res := &holderRes{ev: &events{}}
			c := di.New()
			if order == "owner first" {
				c.MustProvideValue[*holderRes](res)
			}
			if err := c.ProvideValueWith[holderWrap](holderWrap{res}, di.Options{View: true}); err != nil {
				t.Fatal(err)
			}
			if order == "view first" {
				if _, err := c.Manage(res, di.Options{Name: "res"}); err != nil {
					t.Fatal(err)
				}
			}
			seal(t, c)
		})
	}
}

// holderReady is an interface over the resource itself.
type holderReady interface{ Ready(context.Context) error }

// TestOwner_BuiltViewNeedsItsOwnerFirst: a view a constructor builds whose
// value has Shutdown fails its build when no registration owns its resource
// at that moment, naming the remedies — an interface over the resource
// itself is told of Alias — and resolves once a value handed in owns it.
func TestOwner_BuiltViewNeedsItsOwnerFirst(t *testing.T) {
	res := &holderRes{ev: &events{}}
	c := di.New()
	if err := c.ProvideWith(func() holderReady { return res }, di.Options{View: true}); err != nil {
		t.Fatal(err)
	}
	seal(t, c)
	const want = "di: constructing di_test.holderReady: credo.View() declares di_test.holderReady a view of a " +
		"resource another registration owns, but no registration owns it when di_test.holderReady is built, and " +
		"its value, a *di_test.holderRes, has Shutdown: a view's owner exists before the view's value; give the " +
		"owner as a value, with ProvideValue or Manage, or bind di_test.holderReady with " +
		"app.Alias[di_test.holderReady, *di_test.holderRes]() in place of its constructor, so that the owner " +
		"exists first, or drop credo.View() from di_test.holderReady, so that it owns the resource"
	if _, err := c.Resolve[holderReady](); err == nil || err.Error() != want {
		t.Fatalf("Resolve() = %v\nwant %q", err, want)
	}

	c = di.New()
	if err := c.ProvideWith(func() holderReady { return res }, di.Options{View: true}); err != nil {
		t.Fatal(err)
	}
	c.MustProvideValue[*holderRes](res)
	seal(t, c)
	if _, err := c.Resolve[holderReady](); err != nil {
		t.Fatalf("Resolve() with an owner handed in = %v", err)
	}
}

// holderIndex holds the resource without its lifecycle: it names the
// resource as its identity and has no Shutdown.
type holderIndex struct{ r *holderRes }

func (i holderIndex) ResourceIdentity() any { return i.r }

// TestOwner_ConstructorOwnerAfterAView: a value a constructor builds that
// claims a resource a view already holds, while no registration owns it,
// fails its build — a binding's, a worker constructor's, a Manage
// constructor's — naming the remedies that fit it; the drain releases the
// refused value, which no other registration owns.
func TestOwner_ConstructorOwnerAfterAView(t *testing.T) {
	const view = "the ProvideValue binding of di_test.holderIndex, a view, already holds, and a view's owner " +
		"exists before the view's value; "
	for _, tc := range []struct {
		name  string
		owner func(c *di.Container, res *holderRes) (*di.Unit, error)
		want  string
	}{
		{"a binding", func(c *di.Container, res *holderRes) (*di.Unit, error) {
			c.MustProvide(func() *holderRes { return res })
			return nil, nil
		}, "di: constructing *di_test.holderRes: the Provide binding of *di_test.holderRes claims the resource " +
			"that " + view + "bind the *di_test.holderRes with ProvideValue, or build di_test.holderIndex with a " +
			"constructor that takes the *di_test.holderRes, with credo.View(), in place of ProvideValue, so that " +
			"the owner exists first"},
		{"a worker constructor", func(c *di.Container, res *holderRes) (*di.Unit, error) {
			return c.Manage(func() *holderRes { return res },
				di.Options{Name: "worker-value:x", Worker: `Workers.Continuous("x")`})
		}, `the value Workers.Continuous("x") builds, a *di_test.holderRes, claims the resource that ` + view +
			"bind the *di_test.holderRes in place of the worker's constructor"},
		{"a Manage constructor", func(c *di.Container, res *holderRes) (*di.Unit, error) {
			return c.Manage(func() *holderRes { return res }, di.Options{Name: "m"})
		}, "the value the constructor handed to Manage(m) builds, a *di_test.holderRes, claims the resource that " +
			view + "hand the *di_test.holderRes to Manage as a value, or bind it in place of the Manage call"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ev := &events{}
			res := &holderRes{ev: ev}
			c := di.New()
			if err := c.ProvideValueWith[holderIndex](holderIndex{res}, di.Options{View: true}); err != nil {
				t.Fatal(err)
			}
			u, err := tc.owner(c, res)
			if err != nil {
				t.Fatal(err)
			}
			seal(t, c)
			if u == nil {
				_, err = c.Resolve[*holderRes]()
			} else {
				_, err = c.BuildUnit(u)
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("build = %v\nwant it to contain %q", err, tc.want)
			}
			_ = c.Shutdown(t.Context())
			if got := ev.list(); !slices.Equal(got, []string{"shutdown"}) {
				t.Errorf("events = %v, want the refused value released once", got)
			}
		})
	}
}

// TestOwner_ViewDependsOnItsOwner: a consumer of a view handed in before its
// owner starts after the owner, and the ingress check follows the edge from
// the view to its owner.
func TestOwner_ViewDependsOnItsOwner(t *testing.T) {
	res := &holderRes{ev: &events{}}
	c := di.New()
	if err := c.ProvideValueWith[holderWrap](holderWrap{res}, di.Options{View: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Manage(func(holderWrap) *viewUser { return &viewUser{} }, di.Options{Name: "user"}); err != nil {
		t.Fatal(err)
	}
	c.MustProvideValue[*holderRes](res)
	seal(t, c)
	plan := c.StartPlan(di.TierInternal)
	if len(plan) != 2 || plan[0].Name() != "*di_test.holderRes" || plan[1].Name() != "user" {
		names := make([]string, len(plan))
		for i, u := range plan {
			names[i] = u.Name()
		}
		t.Fatalf("StartPlan = %q, want the owner before the view's consumer", names)
	}

	c = di.New()
	if err := c.ProvideValueWith[holderWrap](holderWrap{res}, di.Options{View: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Manage(func(holderWrap) *viewUser { return &viewUser{} }, di.Options{Name: "user"}); err != nil {
		t.Fatal(err)
	}
	if err := c.ProvideValueWith[*holderRes](res, di.Options{Ingress: true}); err != nil {
		t.Fatal(err)
	}
	const want = "di: internal component Manage(user) depends on the ingress component *di_test.holderRes: " +
		"Manage(user) → di_test.holderWrap → *di_test.holderRes"
	if err := c.Seal(); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("Seal() = %v\nwant it to contain %q", err, want)
	}
}

// viewUser consumes a view and is a component of its own.
type viewUser struct{}

func (*viewUser) Ready(context.Context) error    { return nil }
func (*viewUser) Shutdown(context.Context) error { return nil }

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
		const want = "called after the container was finalized by Finalize"
		if reason := misuseReason(t, c.AddEdge(a, b)); !strings.Contains(reason, want) {
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
