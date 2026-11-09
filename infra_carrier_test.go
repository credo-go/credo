package credo_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/credo-go/credo"
)

// carrierBuffer is a log sink safe for the goroutines of a component.
type carrierBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *carrierBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// records returns the JSON records written so far.
func (b *carrierBuffer) records(t *testing.T) []map[string]any {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(b.buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

// find returns the record with message msg.
func (b *carrierBuffer) find(t *testing.T, msg string) map[string]any {
	t.Helper()
	for _, rec := range b.records(t) {
		if rec["msg"] == msg {
			return rec
		}
	}
	t.Fatalf("no record %q in %v", msg, b.records(t))
	return nil
}

// carrierComponent logs through InfraFrom in each call the App makes on it,
// and from a goroutine whose context derives from Start's.
type carrierComponent struct {
	done chan struct{}
}

func (c *carrierComponent) Start(ctx context.Context) error {
	credo.InfraFrom(ctx).Logger.Info("in Start")
	bg := context.WithoutCancel(ctx)
	c.done = make(chan struct{})
	go func() {
		defer close(c.done)
		credo.InfraFrom(bg).Logger.Info("in goroutine")
	}()
	return nil
}

func (c *carrierComponent) Ready(ctx context.Context) error {
	credo.InfraFrom(ctx).Logger.Info("in Ready")
	return nil
}

func (c *carrierComponent) Shutdown(ctx context.Context) error {
	<-c.done
	credo.InfraFrom(ctx).Logger.Info("in Shutdown")
	return nil
}

// TestInfraFrom_CarriesTheComponentScope: Start, Ready, Shutdown and the
// hooks receive the component's scoped Infra, whose logger writes
// component=<name> — the lifecycle's name for it, a credo.ComponentName name
// included — also from a goroutine derived from Start's context.
func TestInfraFrom_CarriesTheComponentScope(t *testing.T) {
	for _, tc := range []struct {
		name     string
		register func(app *credo.App, c *carrierComponent)
		want     string
	}{
		{"binding", func(app *credo.App, c *carrierComponent) { app.ProvideValue(c) },
			"*credo_test.carrierComponent"},
		{"managed with a name", func(app *credo.App, c *carrierComponent) {
			app.Manage(c, credo.ComponentName("carrier"))
		}, "carrier"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buf := &carrierBuffer{}
			app := mustNew(t, credo.WithLogger(slog.New(slog.NewJSONHandler(buf, nil))))
			tc.register(app, &carrierComponent{})
			app.OnStart(func(ctx context.Context) error {
				credo.InfraFrom(ctx).Logger.Info("in OnStart")
				return nil
			})
			app.OnStop(func(ctx context.Context) error {
				credo.InfraFrom(ctx).Logger.Info("in OnStop")
				return nil
			})
			app.UseHealth()
			startComponents(t, app)
			rec := httptest.NewRecorder()
			app.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ready", nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("/ready = %d %s", rec.Code, rec.Body)
			}
			if err := shutdownApp(t, app, 2*time.Second); err != nil {
				t.Fatalf("Shutdown() = %v", err)
			}
			for _, msg := range []string{"in Start", "in goroutine", "in Ready", "in Shutdown"} {
				if got := buf.find(t, msg)["component"]; got != tc.want {
					t.Errorf("%q: component = %v, want %q", msg, got, tc.want)
				}
			}
			if got := buf.find(t, "in OnStart")["component"]; got != "OnStart[0]" {
				t.Errorf("OnStart hook: component = %v, want OnStart[0]", got)
			}
			if got := buf.find(t, "in OnStop")["component"]; got != "OnStop[0]" {
				t.Errorf("OnStop hook: component = %v, want OnStop[0]", got)
			}
			for _, r := range buf.records(t) {
				if _, ok := r["service"]; ok {
					t.Errorf("record %v carries service; the key is retired", r)
				}
			}
		})
	}
}

// TestInfraFrom_OutsideACallIsZero: outside a call the App makes, InfraFrom
// returns the zero Infra.
func TestInfraFrom_OutsideACallIsZero(t *testing.T) {
	if got := credo.InfraFrom(t.Context()); got.Logger != nil {
		t.Errorf("InfraFrom(outside) = %+v, want the zero Infra", got)
	}
	var nilCtx context.Context
	if got := credo.InfraFrom(nilCtx); got.Logger != nil {
		t.Errorf("InfraFrom(nil) = %+v, want the zero Infra", got)
	}
}

// carrierRelay takes Infra in its constructor.
type carrierRelay struct{ infra credo.Infra }

func (*carrierRelay) Shutdown(context.Context) error { return nil }

// TestInfra_ConstructorScopeNamesTheComponent: the logger a constructor
// receives writes component=<name> with the name of what it builds: the
// binding's full type, or a managed component's credo.ComponentName name.
func TestInfra_ConstructorScopeNamesTheComponent(t *testing.T) {
	buf := &carrierBuffer{}
	app := mustNew(t, credo.WithLogger(slog.New(slog.NewJSONHandler(buf, nil))))
	app.Provide(newModel1Service)
	app.Manage(func(infra credo.Infra) *carrierRelay {
		infra.Logger.Info("relay built")
		return &carrierRelay{infra: infra}
	}, credo.ComponentName("relay"))
	startComponents(t, app)
	app.MustResolve[*model1Service]().Infra.Logger.Info("service built")
	if err := shutdownApp(t, app, 2*time.Second); err != nil {
		t.Fatalf("Shutdown() = %v", err)
	}
	if got := buf.find(t, "service built")["component"]; got != "*credo_test.model1Service" {
		t.Errorf("binding: component = %v, want *credo_test.model1Service", got)
	}
	if got := buf.find(t, "relay built")["component"]; got != "relay" {
		t.Errorf("managed constructor: component = %v, want relay", got)
	}
}

// carrierCloser is released through credo.CloseOnShutdown() and logs
// through InfraFrom in its Close.
type carrierCloser struct{}

func (*carrierCloser) Close(ctx context.Context) error {
	credo.InfraFrom(ctx).Logger.Info("in Close")
	return nil
}

// TestInfraFrom_CloseCarriesTheComponentScope: the Close(ctx) of a
// credo.CloseOnShutdown() binding receives the component's scoped Infra, as
// every call the App makes on a component does.
func TestInfraFrom_CloseCarriesTheComponentScope(t *testing.T) {
	buf := &carrierBuffer{}
	app := mustNew(t, credo.WithLogger(slog.New(slog.NewJSONHandler(buf, nil))))
	app.ProvideValue(&carrierCloser{}, credo.CloseOnShutdown())
	startComponents(t, app)
	if err := shutdownApp(t, app, 2*time.Second); err != nil {
		t.Fatalf("Shutdown() = %v", err)
	}
	if got := buf.find(t, "in Close")["component"]; got != "*credo_test.carrierCloser" {
		t.Errorf("Close: component = %v, want *credo_test.carrierCloser", got)
	}
}
