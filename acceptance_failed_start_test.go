//go:build pending_w4

package credo_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/credo-go/credo"
)

// startStop is a component with a start step. Each acceptance type embeds it,
// because the container keys bindings by type.
type startStop struct {
	name      string
	log       *acceptanceLog
	failStart bool
}

func (s *startStop) Start(context.Context) error {
	s.log.add("start " + s.name)
	if s.failStart {
		return errors.New("cannot connect")
	}
	return nil
}

func (s *startStop) Shutdown(context.Context) error {
	s.log.add("stop " + s.name)
	return nil
}

type (
	componentA struct{ startStop }
	componentB struct{ startStop }
	componentC struct{ startStop }
	componentD struct{ startStop }
)

// componentE has no start step. It is built during bootstrap, so a failed
// start finds it built but never started.
type componentE struct{ log *acceptanceLog }

func (e *componentE) Shutdown(context.Context) error {
	e.log.add("stop e")
	return nil
}

// TestAcceptance_FailedStartStopsWhatWasBuilt is the second acceptance scenario
// of the lifecycle components (ADR-024): a failed start stops exactly the
// components that were built, in reverse dependency order, except the one whose
// Start failed, which has released what it opened. A component the walk never
// reached is never built.
func TestAcceptance_FailedStartStopsWhatWasBuilt(t *testing.T) {
	log := &acceptanceLog{}
	host, port, _ := freePort(t)
	app := mustNew(t, credo.WithAddr(host, port))

	app.ProvideValue(log)
	app.Provide[*componentA](func(l *acceptanceLog) *componentA {
		l.add("build a")
		return &componentA{startStop{name: "a", log: l}}
	})
	app.Provide[*componentB](func(_ *componentA, l *acceptanceLog) *componentB {
		l.add("build b")
		return &componentB{startStop{name: "b", log: l}}
	})
	app.Provide[*componentC](func(_ *componentB, l *acceptanceLog) *componentC {
		l.add("build c")
		return &componentC{startStop{name: "c", log: l, failStart: true}}
	})
	app.Provide[*componentD](func(_ *componentC, l *acceptanceLog) *componentD {
		l.add("build d")
		return &componentD{startStop{name: "d", log: l}}
	})
	app.Provide[*componentE](func(l *acceptanceLog) *componentE {
		l.add("build e")
		return &componentE{log: l}
	})
	if err := app.Finalize(); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if _, err := app.Resolve[*componentE](); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- app.RunContext(t.Context()) }()
	var err error
	select {
	case err = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("RunContext did not return after the failed start")
	}

	if err == nil {
		t.Fatal("RunContext returned nil after a failed start")
	}
	if _, ok := errors.AsType[*credo.LifecycleError](err); !ok {
		t.Errorf("error %T is not a *credo.LifecycleError: %v", err, err)
	}
	for _, want := range []string{"componentC", "start", "cannot connect"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
	if got := app.State(); got != "stopped" {
		t.Errorf("State() = %q, want stopped", got)
	}

	events := log.snapshot()
	for _, want := range []string{"start a", "start b", "start c", "stop b", "stop a", "stop e"} {
		if !slices.Contains(events, want) {
			t.Errorf("missing %q; events: %q", want, events)
		}
	}
	for _, unwanted := range []string{"stop c", "build d", "start d", "stop d"} {
		if slices.Contains(events, unwanted) {
			t.Errorf("unexpected %q; events: %q", unwanted, events)
		}
	}
	assertBefore(t, events, "start a", "start b")
	assertBefore(t, events, "start b", "start c")
	assertBefore(t, events, "start c", "stop b")
	assertBefore(t, events, "stop b", "stop a")
}
