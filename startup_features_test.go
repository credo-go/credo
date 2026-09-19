package credo_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/credo-go/credo"
)

const startMsg = "credo: server started"

// newJSONLoggingApp builds an app on a free port whose logger writes JSON
// records to logs. Only a JSON handler shows the difference between an empty
// list ([]) and a nil one (null).
func newJSONLoggingApp(t *testing.T, logs *syncBuffer, opts ...credo.Option) *credo.App {
	t.Helper()
	host, port, _ := freePort(t)
	all := append([]credo.Option{
		credo.WithAddr(host, port),
		credo.WithLogger(slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))),
	}, opts...)
	return mustNew(t, all...)
}

// startRecord runs app through RunContext and returns its start record.
func startRecord(t *testing.T, app *credo.App, logs *syncBuffer) map[string]any {
	t.Helper()
	startApp(t, app)
	return findRecord(t, logs.waitFor(t, startMsg), startMsg)
}

func findRecord(t *testing.T, out, msg string) map[string]any {
	t.Helper()
	for line := range strings.Lines(out) {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) == nil && rec["msg"] == msg {
			return rec
		}
	}
	t.Fatalf("no %q record in:\n%s", msg, out)
	return nil
}

func featuresOf(t *testing.T, rec map[string]any) []string {
	t.Helper()
	raw, ok := rec["features"].([]any)
	if !ok {
		t.Fatalf("features = %#v, want a JSON array", rec["features"])
	}
	names := make([]string, 0, len(raw))
	for _, v := range raw {
		names = append(names, v.(string))
	}
	return names
}

func TestStartLine_DefaultAppListsRecover(t *testing.T) {
	logs := &syncBuffer{}
	app := newJSONLoggingApp(t, logs)
	rec := startRecord(t, app, logs)

	if got := featuresOf(t, rec); !slices.Equal(got, []string{"recover"}) {
		t.Errorf("features = %q, want [recover]", got)
	}
	if rec["label"] != "RunContext" || rec["addr"] != app.Addr().String() {
		t.Errorf("label = %v, addr = %v, want RunContext and %s", rec["label"], rec["addr"], app.Addr())
	}
}

func TestStartLine_NothingOnIsAnEmptyArray(t *testing.T) {
	logs := &syncBuffer{}
	app := newJSONLoggingApp(t, logs, credo.WithoutRecover())
	startRecord(t, app, logs)

	out := logs.String()
	if !strings.Contains(out, `"features":[]`) {
		t.Errorf(`start line lacks "features":[]; logs:`+"\n%s", out)
	}
	if strings.Contains(out, `"level":"WARN"`) {
		t.Errorf("the summary must add no Warn for features that are off; logs:\n%s", out)
	}
}

func TestStartLine_FullListInDisplayOrder(t *testing.T) {
	install := []func(*credo.App){
		func(a *credo.App) { a.UseRequestID() },
		func(a *credo.App) { a.UseAccessLog() },
		func(a *credo.App) { a.UseDecompress() },
		func(a *credo.App) { a.UseCompress() },
		func(a *credo.App) {
			if err := a.UseI18n(credo.I18nConfig{Messages: credo.I18nMessages{"hello": "Hello"}}); err != nil {
				t.Fatal(err)
			}
		},
		func(a *credo.App) {
			a.UseErrorRenderer(func(*credo.Context, *credo.ErrorInfo) any { return nil })
		},
		func(a *credo.App) {
			a.UseSuccessRenderer(func(*credo.Context, credo.RenderInfo) any { return nil })
		},
		func(a *credo.App) { a.UseHealth() },
	}
	want := []string{
		"recover", "request_id", "access_log", "decompress", "compress",
		"i18n", "error_renderer", "success_renderer", "health",
	}

	for name, order := range map[string][]int{
		"registration order": {0, 1, 2, 3, 4, 5, 6, 7},
		"reversed":           {7, 6, 5, 4, 3, 2, 1, 0},
		"interleaved":        {4, 0, 7, 2, 6, 1, 5, 3},
	} {
		t.Run(name, func(t *testing.T) {
			logs := &syncBuffer{}
			app := newJSONLoggingApp(t, logs)
			for _, i := range order {
				install[i](app)
			}
			if got := featuresOf(t, startRecord(t, app, logs)); !slices.Equal(got, want) {
				t.Errorf("features = %q, want %q", got, want)
			}
		})
	}
}

func TestStartLine_InactiveI18nIsNotListed(t *testing.T) {
	logs := &syncBuffer{}
	app := newJSONLoggingApp(t, logs)
	// No locales/ directory: conventional discovery succeeds but stays inactive.
	if err := app.UseI18n(); err != nil {
		t.Fatal(err)
	}
	rec := startRecord(t, app, logs)

	if got := featuresOf(t, rec); !slices.Equal(got, []string{"recover"}) {
		t.Errorf("features = %q, want [recover]", got)
	}
	if !strings.Contains(logs.String(), "credo: i18n inactive") {
		t.Error("the existing inactive-i18n Warn must still be written")
	}
}

func TestStartLine_HealthNeedsAProbeRoute(t *testing.T) {
	tests := []struct {
		name   string
		setup  func(*credo.App)
		listed bool
	}{
		{"not called", func(*credo.App) {}, false},
		{"disabled", func(a *credo.App) { a.UseHealth(credo.HealthConfig{Enabled: new(false)}) }, false},
		{"both probes off", func(a *credo.App) {
			a.UseHealth(credo.HealthConfig{Liveness: new(false), Readiness: new(false)})
		}, false},
		{"liveness only", func(a *credo.App) { a.UseHealth(credo.HealthConfig{Readiness: new(false)}) }, true},
		{"readiness only", func(a *credo.App) { a.UseHealth(credo.HealthConfig{Liveness: new(false)}) }, true},
		{"defaults", func(a *credo.App) { a.UseHealth() }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logs := &syncBuffer{}
			app := newJSONLoggingApp(t, logs)
			tt.setup(app)
			got := featuresOf(t, startRecord(t, app, logs))
			if slices.Contains(got, "health") != tt.listed {
				t.Errorf("features = %q, health listed = %v, want %v", got, !tt.listed, tt.listed)
			}
		})
	}
}

func TestStartLine_ServeContext(t *testing.T) {
	logs := &syncBuffer{}
	app := newJSONLoggingApp(t, logs)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	errC := make(chan error, 1)
	go func() { errC <- app.ServeContext(context.Background(), l) }()
	rec := findRecord(t, logs.waitFor(t, startMsg), startMsg)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := app.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	<-errC

	if rec["label"] != "ServeContext" || rec["addr"] != l.Addr().String() {
		t.Errorf("label = %v, addr = %v, want ServeContext and %s", rec["label"], rec["addr"], l.Addr())
	}
	if got := featuresOf(t, rec); !slices.Equal(got, []string{"recover"}) {
		t.Errorf("features = %q, want [recover]", got)
	}
}

func TestStartLine_FailedStartWritesNone(t *testing.T) {
	logs := &syncBuffer{}
	app := newJSONLoggingApp(t, logs)
	app.OnStart(func(context.Context) error { return errors.New("boom") })

	if err := app.RunContext(context.Background()); err == nil {
		t.Fatal("RunContext succeeded, want the OnStart error")
	}
	if strings.Contains(logs.String(), startMsg) {
		t.Errorf("a failed start wrote a start line:\n%s", logs.String())
	}
}
