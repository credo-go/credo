package config_test

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/credo-go/credo/config"
)

// writeFile writes content to path, failing the test on error.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// loadYAML writes a YAML config file and loads it with a prefix no test sets,
// returning the Config and the file path for later edits.
func loadYAML(t *testing.T, content string, opts ...config.Option) (*config.Config, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, path, content)
	opts = append([]config.Option{config.WithFiles(path), config.WithPrefix("NOTSET_")}, opts...)
	c, err := config.Load(opts...)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return c, path
}

func TestReloadImplementsReloader(t *testing.T) {
	var _ config.Reloader = (*config.Config)(nil)
}

func TestReloadPicksUpFileChanges(t *testing.T) {
	c, path := loadYAML(t, "server:\n  port: 8080\n  host: a\nname: app\n")

	writeFile(t, path, "server:\n  port: 9090\n  host: a\nname: app\nextra: 1\n")
	changes, err := c.Reload()
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}

	if got, want := changes.Keys(), []string{"extra", "server.port"}; !slices.Equal(got, want) {
		t.Errorf("Keys = %v, want %v", got, want)
	}
	if port := c.MustGet[int]("server.port"); port != 9090 {
		t.Errorf("server.port after reload = %d, want 9090", port)
	}
	if !c.Exists("extra") {
		t.Error("added key should exist after reload")
	}

	for prefix, want := range map[string]bool{
		"server":      true,
		"server.port": true,
		"server.host": false,
		"serv":        false, // prefix match is per path segment, not substring
		"name":        false,
		"extra":       true,
		"":            true,
	} {
		if got := changes.Affects(prefix); got != want {
			t.Errorf("Affects(%q) = %v, want %v", prefix, got, want)
		}
	}
}

func TestReloadRemovedKeyIsAChange(t *testing.T) {
	c, path := loadYAML(t, "a: 1\nsection:\n  x: 1\n  y: 2\n")

	writeFile(t, path, "a: 1\nsection:\n  x: 1\n")
	changes, err := c.Reload()
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got, want := changes.Keys(), []string{"section.y"}; !slices.Equal(got, want) {
		t.Errorf("Keys = %v, want %v", got, want)
	}
	if c.Exists("section.y") {
		t.Error("removed key should not exist after reload")
	}
}

func TestReloadUnchangedIsEmpty(t *testing.T) {
	c, _ := loadYAML(t, "a: 1\nlist: [1, 2]\nnested:\n  b: x\n")

	changes, err := c.Reload()
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if !changes.Empty() {
		t.Errorf("expected no changes, got %v", changes.Keys())
	}
	if changes.Affects("") {
		t.Error("Affects(\"\") must be false when nothing changed")
	}
}

func TestReloadKeysReturnsCopy(t *testing.T) {
	c, path := loadYAML(t, "a: 1\n")
	writeFile(t, path, "a: 2\n")
	changes, err := c.Reload()
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	keys := changes.Keys()
	keys[0] = "mutated"
	if got := changes.Keys(); got[0] != "a" {
		t.Errorf("Keys must return a copy; got %v", got)
	}
}

func TestReloadErrorKeepsPreviousSnapshot(t *testing.T) {
	c, path := loadYAML(t, "server:\n  port: 8080\n")

	writeFile(t, path, "server:\n  port: [unclosed\n")
	changes, err := c.Reload()
	if err == nil {
		t.Fatal("expected a parse error from Reload")
	}
	if !strings.Contains(err.Error(), "config: reload:") {
		t.Errorf("error should carry the reload prefix, got %q", err)
	}
	if !changes.Empty() {
		t.Errorf("failed reload must report no changes, got %v", changes.Keys())
	}
	if port := c.MustGet[int]("server.port"); port != 8080 {
		t.Errorf("previous snapshot must survive a failed reload; port = %d", port)
	}

	// A missing explicit file is a load error too, with the same guarantee.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Reload(); err == nil {
		t.Fatal("expected an error when the explicit file disappears")
	}
	if port := c.MustGet[int]("server.port"); port != 8080 {
		t.Errorf("previous snapshot must survive a missing file; port = %d", port)
	}
}

func TestReloadReflectsEnvVarChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, path, "server:\n  port: 8080\n")
	t.Setenv("RELOADTEST_SERVER__PORT", "1111")

	c, err := config.Load(config.WithFiles(path), config.WithPrefix("RELOADTEST_"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if port := c.MustGet[int]("server.port"); port != 1111 {
		t.Fatalf("initial port = %d, want 1111", port)
	}

	t.Setenv("RELOADTEST_SERVER__PORT", "2222")
	changes, err := c.Reload()
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if !changes.Affects("server.port") {
		t.Errorf("env var change should be reported, got %v", changes.Keys())
	}
	if port := c.MustGet[int]("server.port"); port != 2222 {
		t.Errorf("port after reload = %d, want 2222", port)
	}
}

func TestReloadReflectsDotenvChanges(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	writeFile(t, cfgPath, "server:\n  host: from-yaml\n")
	dotenv := filepath.Join(dir, ".env")
	writeFile(t, dotenv, "SERVER__HOST=from-dotenv-1\n")

	c, err := config.Load(config.WithFiles(cfgPath), config.WithPrefix("NOTSET_"), config.WithDotenvPath(dotenv))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	writeFile(t, dotenv, "SERVER__HOST=from-dotenv-2\n")
	if _, err := c.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if host := c.MustGet[string]("server.host"); host != "from-dotenv-2" {
		t.Errorf("host after reload = %q, want from-dotenv-2", host)
	}
}

func TestReloadKeepsCredoEnvFixed(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "app.yaml")
	writeFile(t, base, "tier: base\n")
	writeFile(t, filepath.Join(dir, "app.dev.yaml"), "tier: dev\n")
	writeFile(t, filepath.Join(dir, "app.prod.yaml"), "tier: prod\n")

	t.Setenv("CREDO_ENV", "dev")
	c, err := config.Load(config.WithFiles(base), config.WithPrefix("NOTSET_"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if tier := c.MustGet[string]("tier"); tier != "dev" {
		t.Fatalf("initial tier = %q, want dev", tier)
	}

	t.Setenv("CREDO_ENV", "prod")
	changes, err := c.Reload()
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if !changes.Empty() {
		t.Errorf("switching CREDO_ENV must not change anything, got %v", changes.Keys())
	}
	if tier := c.MustGet[string]("tier"); tier != "dev" {
		t.Errorf("tier after reload = %q, want dev (CREDO_ENV is fixed at first load)", tier)
	}
}

func TestReloadLoadBytesReplaysDocument(t *testing.T) {
	doc := []byte(`{"server": {"port": 8080, "host": "embedded"}}`)
	t.Setenv("BYTESTEST_SERVER__PORT", "1")

	c, err := config.LoadBytes(doc, config.FormatJSON, config.WithPrefix("BYTESTEST_"))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	// Mutating the caller's slice must not affect what Reload replays.
	doc[0] = '['

	t.Setenv("BYTESTEST_SERVER__PORT", "2")
	changes, err := c.Reload()
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got, want := changes.Keys(), []string{"server.port"}; !slices.Equal(got, want) {
		t.Errorf("Keys = %v, want %v", got, want)
	}
	if port := c.MustGet[int]("server.port"); port != 2 {
		t.Errorf("port after reload = %d, want 2", port)
	}
	if host := c.MustGet[string]("server.host"); host != "embedded" {
		t.Errorf("embedded document must be replayed; host = %q", host)
	}
}

func TestReloadUninitialized(t *testing.T) {
	var zero config.Config
	if _, err := zero.Reload(); err == nil {
		t.Error("zero Config must not reload")
	}
	var nilCfg *config.Config
	if _, err := nilCfg.Reload(); err == nil {
		t.Error("nil Config must not reload")
	}
}

func TestReloadConcurrentReadersSeeWholeSnapshots(t *testing.T) {
	// Each snapshot keeps a and b equal; a reader must never observe a mix.
	c, path := loadYAML(t, "pair:\n  a: 0\n  b: 0\n")

	type pair struct {
		A int `credo:"a"`
		B int `credo:"b"`
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	var torn atomic.Bool
	for range 4 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				p, err := c.Get[pair]("pair")
				if err != nil {
					t.Error(err)
					return
				}
				if p.A != p.B {
					torn.Store(true)
					return
				}
				_ = c.Exists("pair.a")
			}
		})
	}
	for i := 1; i <= 25; i++ {
		writeFile(t, path, "pair:\n  a: "+strconv.Itoa(i)+"\n  b: "+strconv.Itoa(i)+"\n")
		if _, err := c.Reload(); err != nil {
			t.Fatalf("Reload %d: %v", i, err)
		}
	}
	close(stop)
	wg.Wait()
	if torn.Load() {
		t.Error("a reader observed a torn snapshot")
	}
}

func TestStagePublishesOnlyOnCommit(t *testing.T) {
	c, path := loadYAML(t, "server:\n  port: 8080\n")
	writeFile(t, path, "server:\n  port: 9090\n")

	staged, err := c.Stage()
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if !staged.Changes().Affects("server.port") {
		t.Fatalf("staged changes = %v, want server.port", staged.Changes().Keys())
	}
	if port := c.MustGet[int]("server.port"); port != 8080 {
		t.Errorf("parent must be unchanged before Commit; port = %d", port)
	}
	var candidate int
	if err := staged.Unmarshal("server.port", &candidate); err != nil || candidate != 9090 {
		t.Errorf("staged.Unmarshal = %d, %v; want 9090", candidate, err)
	}
	if !staged.Exists("server.port") || staged.Exists("nope") {
		t.Error("staged.Exists must read the candidate")
	}

	staged.Commit()
	if port := c.MustGet[int]("server.port"); port != 9090 {
		t.Errorf("port after Commit = %d, want 9090", port)
	}
}

func TestStageDiscardedWithoutCommit(t *testing.T) {
	c, path := loadYAML(t, "a: 1\n")
	writeFile(t, path, "a: 2\n")
	if _, err := c.Stage(); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if a := c.MustGet[int]("a"); a != 1 {
		t.Errorf("an uncommitted stage must not publish; a = %d", a)
	}
	changes, err := c.Reload()
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if !changes.Affects("a") {
		t.Errorf("Reload after a discarded stage must still see the change, got %v", changes.Keys())
	}
}

func TestStageImplementsInterfaces(t *testing.T) {
	var _ config.Stager = (*config.Config)(nil)
	var rc config.RawConfig
	c, _ := loadYAML(t, "a: 1\n")
	staged, err := c.Stage()
	if err != nil {
		t.Fatal(err)
	}
	rc = staged
	if !rc.Exists("a") {
		t.Error("Staged must satisfy RawConfig over the candidate")
	}
}
