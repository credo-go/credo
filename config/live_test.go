package config_test

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/credo-go/credo/config"
)

type liveLimits struct {
	RPS   int
	Burst int
}

func (l liveLimits) Validate() error {
	if l.RPS <= 0 {
		return errors.New("rps must be positive")
	}
	return nil
}

type liveName struct {
	Name string
}

var (
	limitsLive = config.Section[liveLimits]("limits")
	nameLive   = config.Section[liveName]("app")
)

func mustLive[T any](t *testing.T, s config.Section[T], c *config.Config) *config.Live[T] {
	t.Helper()
	l, err := s.Live(c)
	if err != nil {
		t.Fatalf("%s.Live: %v", string(s), err)
	}
	return l
}

func mustReload(t *testing.T, c *config.Config) config.ReloadResult {
	t.Helper()
	res, err := c.Reload(t.Context())
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	return res
}

func assertContains(t *testing.T, what, got string, parts ...string) {
	t.Helper()
	for _, p := range parts {
		if !strings.Contains(got, p) {
			t.Errorf("%s %q does not contain %q", what, got, p)
		}
	}
}

func TestLive_FollowsItsSection(t *testing.T) {
	c, path := loadYAML(t, "limits:\n  rps: 10\n  burst: 1\napp:\n  name: a\n")
	limits := mustLive(t, limitsLive, c)
	name := mustLive(t, nameLive, c)
	if got := limits.Load(); got.RPS != 10 {
		t.Fatalf("initial RPS = %d, want 10", got.RPS)
	}

	writeFile(t, path, "limits:\n  rps: 20\n  burst: 1\napp:\n  name: a\n")
	res := mustReload(t, c)
	if got := limits.Load().RPS; got != 20 {
		t.Errorf("RPS after a reload that changes limits = %d, want 20", got)
	}
	if got := name.Load().Name; got != "a" {
		t.Errorf("app after a reload that changes only limits = %q, want a", got)
	}
	if !res.Published || !slices.Equal(res.Replaced, []string{"limits"}) || !slices.Equal(res.Changed, []string{"limits.rps"}) {
		t.Errorf("result = %+v, want published, replaced [limits], changed [limits.rps]", res)
	}

	writeFile(t, path, "limits:\n  rps: 20\n  burst: 1\napp:\n  name: b\n")
	mustReload(t, c)
	if got := limits.Load().RPS; got != 20 {
		t.Errorf("RPS after a reload that changes another section = %d, want 20", got)
	}
	if got := name.Load().Name; got != "b" {
		t.Errorf("app after its reload = %q, want b", got)
	}
}

func TestLive_FailsAsDecodeDoes(t *testing.T) {
	c, _ := loadYAML(t, "limits:\n  rps: 0\n")
	for _, tt := range []struct {
		name  string
		build func() error
		parts []string
	}{
		{"missing section", func() error { _, err := config.Section[liveName]("absent").Live(c); return err },
			[]string{`section "absent"`, "not found"}},
		{"invalid section", func() error { _, err := limitsLive.Live(c); return err },
			[]string{`section "limits"`, "rps must be positive"}},
		{"empty key", func() error { _, err := config.Section[liveName]("").Live(c); return err },
			[]string{"Section.Live", "empty section key", `config.Section[T]("app")`}},
		{"custom RawConfig", func() error { _, err := limitsLive.Live(customRaw{}); return err },
			[]string{"config_test.customRaw", "cannot reload", "config.Load"}},
		{"uninitialized Config", func() error { _, err := limitsLive.Live(&config.Config{}); return err },
			[]string{"cannot reload", "config.Load"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.build()
			if err == nil {
				t.Fatal("Live: expected an error, got nil")
			}
			assertContains(t, "error", err.Error(), tt.parts...)
		})
	}
}

func TestLive_InvalidSectionAbortsTheWholeReload(t *testing.T) {
	c, path := loadYAML(t, "limits:\n  rps: 10\napp:\n  name: a\n")
	limits := mustLive(t, limitsLive, c)
	name := mustLive(t, nameLive, c)
	called := false
	name.OnChange(func(context.Context, liveName, liveName) error { called = true; return nil })

	writeFile(t, path, "limits:\n  rps: -1\napp:\n  name: b\n")
	res, err := c.Reload(t.Context())
	if err == nil {
		t.Fatal("Reload: expected the invalid live section's error")
	}
	assertContains(t, "error", err.Error(), `live section "limits"`, "rps must be positive", "nothing was published")
	if res.Published || res.Aborted == "" || len(res.Replaced) != 0 {
		t.Errorf("result = %+v, want unpublished with the reason", res)
	}
	assertContains(t, "Aborted", res.Aborted, `live section "limits"`, "rps must be positive")
	if limits.Load().RPS != 10 || name.Load().Name != "a" || called {
		t.Errorf("an aborted reload published: limits %+v, app %+v, callback %v", limits.Load(), name.Load(), called)
	}
	var v string
	if err := c.Unmarshal("app.name", &v); err != nil || v != "a" {
		t.Errorf("an aborted reload published the snapshot: app.name = %q, %v", v, err)
	}

	// The next reload sees the change again and applies it once valid.
	writeFile(t, path, "limits:\n  rps: 30\napp:\n  name: b\n")
	mustReload(t, c)
	if limits.Load().RPS != 30 || name.Load().Name != "b" || !called {
		t.Errorf("the fixed reload did not apply both sections: %+v %+v %v", limits.Load(), name.Load(), called)
	}
}

// TestLive_OneGenerationForReads pins that once a Load returns a reload's
// value for one section, a Load of another section the reload changed returns
// that reload's value too.
func TestLive_OneGenerationForReads(t *testing.T) {
	doc := func(i int) string {
		n := strconv.Itoa(i)
		return "limits:\n  rps: " + n + "\napp:\n  name: \"" + n + "\"\n"
	}
	c, path := loadYAML(t, doc(1))
	limits := mustLive(t, limitsLive, c)
	name := mustLive(t, nameLive, c)

	var torn atomic.Bool
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				rps := limits.Load().RPS
				n, _ := strconv.Atoi(name.Load().Name)
				if n < rps {
					torn.Store(true)
					return
				}
			}
		})
	}
	for i := 2; i <= 30; i++ {
		writeFile(t, path, doc(i))
		mustReload(t, c)
	}
	close(stop)
	wg.Wait()
	if torn.Load() {
		t.Error("a Load returned an older generation after another section's newer value")
	}
}

func TestLive_OnChange(t *testing.T) {
	c, path := loadYAML(t, "limits:\n  rps: 10\napp:\n  name: a\n")
	limits := mustLive(t, limitsLive, c)
	name := mustLive(t, nameLive, c)

	var order []string
	limits.OnChange(func(_ context.Context, prev, next liveLimits) error {
		order = append(order, "limits:"+strconv.Itoa(prev.RPS)+"->"+strconv.Itoa(next.RPS))
		if limits.Load() != next {
			t.Error("OnChange ran before the new value was published")
		}
		return errors.New("limiter refused")
	})
	name.OnChange(func(context.Context, liveName, liveName) error {
		order = append(order, "app")
		panic("boom")
	})
	limits.OnChange(func(context.Context, liveLimits, liveLimits) error {
		order = append(order, "limits-2")
		return nil
	})

	writeFile(t, path, "limits:\n  rps: 20\napp:\n  name: b\n")
	res, err := c.Reload(t.Context())
	if err == nil {
		t.Fatal("Reload: expected the joined callback errors")
	}
	assertContains(t, "error", err.Error(), "limiter refused", "panic: boom", `live section "limits"`, `live section "app"`)
	if !res.Published {
		t.Error("callback errors must not undo the publish")
	}
	if want := []string{"limits:10->20", "app", "limits-2"}; !slices.Equal(order, want) {
		t.Errorf("callback order = %v, want %v", order, want)
	}

	order = nil
	writeFile(t, path, "limits:\n  rps: 20\napp:\n  name: c\n")
	if _, err := c.Reload(t.Context()); err == nil {
		t.Fatal("Reload: expected the app callback's panic")
	}
	if want := []string{"app"}; !slices.Equal(order, want) {
		t.Errorf("callbacks after a reload that changes only app = %v, want %v", order, want)
	}
}

func TestLive_OnChangeRunsWithACancelledContext(t *testing.T) {
	c, path := loadYAML(t, "limits:\n  rps: 10\n")
	limits := mustLive(t, limitsLive, c)
	var sawCancel bool
	limits.OnChange(func(ctx context.Context, _, _ liveLimits) error {
		sawCancel = ctx.Err() != nil
		return nil
	})
	writeFile(t, path, "limits:\n  rps: 11\n")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := c.Reload(ctx); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if !sawCancel {
		t.Error("the callback did not run, or did not see the cancelled context")
	}
}

func TestLive_ReportsKeysThatNeedARestart(t *testing.T) {
	t.Setenv("LIVEREPORT_FEATURE__LEVEL", "env")
	c, path := loadYAML(t, "limits:\n  rps: 10\napp:\n  name: a\nfeature:\n  level: file\nother: 1\n",
		config.WithPrefix("LIVEREPORT_"))
	mustLive(t, limitsLive, c)
	if _, err := config.Section[liveName]("app").Decode(c); err != nil {
		t.Fatal(err)
	}

	writeFile(t, path, "limits:\n  rps: 20\napp:\n  name: b\nfeature:\n  level: file-2\nother: 2\n")
	res := mustReload(t, c)
	if want := []string{"app.name", "other"}; !slices.Equal(res.RestartRequired, want) {
		t.Errorf("RestartRequired = %v, want %v (limits is live, feature.level is overridden)", res.RestartRequired, want)
	}
	if want := []config.OverriddenKey{{Key: "feature.level", Source: "environment variable LIVEREPORT_FEATURE__LEVEL"}}; !slices.Equal(res.Overridden, want) {
		t.Errorf("Overridden = %v, want %v", res.Overridden, want)
	}

	res = mustReload(t, c)
	if len(res.Changed) != 0 || !slices.Equal(res.RestartRequired, []string{"app.name", "other"}) {
		t.Errorf("a reload that changes nothing: %+v, want the restart keys reported again", res)
	}

	writeFile(t, path, "limits:\n  rps: 20\napp:\n  name: a\nfeature:\n  level: file\nother: 1\n")
	res = mustReload(t, c)
	if len(res.RestartRequired) != 0 || len(res.Overridden) != 0 {
		t.Errorf("a reload that reverts every key: %+v, want no restart and no override reported", res)
	}
}

func TestLive_OverriddenByDotenvNamesTheEntry(t *testing.T) {
	dir := t.TempDir()
	dotenv := dir + "/.env"
	writeFile(t, dotenv, "APP__NAME=dotenv\n")
	c, path := loadYAML(t, "app:\n  name: a\n", config.WithDotenvPath(dotenv))
	writeFile(t, path, "app:\n  name: b\n")
	res := mustReload(t, c)
	if want := []config.OverriddenKey{{Key: "app.name", Source: ".env entry APP__NAME"}}; !slices.Equal(res.Overridden, want) {
		t.Errorf("Overridden = %v, want %v", res.Overridden, want)
	}
}

func TestLive_OneNaturePerSection(t *testing.T) {
	const doc = "limits:\n  rps: 10\n  burst: 1\nserver:\n  port: 1\n"
	for _, tt := range []struct {
		name          string
		first, second func(*config.Config) error
		reads         []string
	}{
		{
			name:   "Decode then Live",
			first:  func(c *config.Config) error { _, err := limitsLive.Decode(c); return err },
			second: func(c *config.Config) error { _, err := limitsLive.Live(c); return err },
			reads:  []string{`static, by config.Section[config_test.liveLimits]("limits").Decode`, `live, by config.Section[config_test.liveLimits]("limits").Live`},
		},
		{
			name:   "Live then Decode",
			first:  func(c *config.Config) error { _, err := limitsLive.Live(c); return err },
			second: func(c *config.Config) error { _, err := limitsLive.Decode(c); return err },
			reads:  []string{`("limits").Decode`, `("limits").Live`},
		},
		{
			name:   "static read nested in a live section",
			first:  func(c *config.Config) error { _, err := limitsLive.Live(c); return err },
			second: func(c *config.Config) error { _, err := config.Section[int]("limits.rps").Decode(c); return err },
			reads:  []string{`config.Section[int]("limits.rps").Decode`, `("limits").Live`},
		},
		{
			name:   "static read containing a live section",
			first:  func(c *config.Config) error { _, err := config.Section[int]("limits.rps").Live(c); return err },
			second: func(c *config.Config) error { _, err := limitsLive.Decode(c); return err },
			reads:  []string{`("limits").Decode`, `config.Section[int]("limits.rps").Live`},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := loadYAML(t, doc)
			if err := tt.first(c); err != nil {
				t.Fatalf("first read: %v", err)
			}
			err := tt.second(c)
			if err == nil {
				t.Fatal("second read of the other nature: expected an error")
			}
			assertContains(t, "error", err.Error(), append(tt.reads, "static or live within one configuration")...)
		})
	}

	t.Run("two reads of one nature", func(t *testing.T) {
		c, _ := loadYAML(t, doc)
		for range 2 {
			if _, err := limitsLive.Decode(c); err != nil {
				t.Fatalf("Decode: %v", err)
			}
		}
		if _, err := config.Section[int]("server.port").Live(c); err != nil {
			t.Fatalf("Live: %v", err)
		}
		if _, err := config.Section[int]("server.port").Live(c); err != nil {
			t.Fatalf("second Live: %v", err)
		}
	})
}

func TestFixed(t *testing.T) {
	l := config.Fixed(liveLimits{RPS: 5})
	l.OnChange(func(context.Context, liveLimits, liveLimits) error {
		t.Error("a Fixed handle ran a callback")
		return nil
	})
	if got := l.Load(); got.RPS != 5 {
		t.Errorf("Fixed.Load = %+v, want RPS 5", got)
	}
	defer func() {
		if r := recover(); r == nil || !strings.Contains(r.(string), "nil callback") {
			t.Errorf("OnChange(nil) = %v, want a panic naming the nil callback", r)
		}
	}()
	l.OnChange(nil)
}
