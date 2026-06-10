package worker

import (
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/credo-go/credo"
)

func newTestApp(t *testing.T, opts ...credo.Option) *credo.App {
	t.Helper()
	app, err := credo.New(opts...)
	if err != nil {
		t.Fatalf("credo.New() = %v", err)
	}
	return app
}

func mustSchedule(t *testing.T, expr string) *Schedule {
	t.Helper()
	s, err := ParseSchedule(expr)
	if err != nil {
		t.Fatalf("ParseSchedule(%q) = %v", expr, err)
	}
	return s
}

type fakeRawConfig struct {
	worker poolConfig
	err    error
	exists bool
}

func (c fakeRawConfig) Unmarshal(key string, dst any) error {
	if key != "worker" {
		return fmt.Errorf("unknown key %q", key)
	}
	if c.err != nil {
		return c.err
	}
	config, ok := dst.(*poolConfig)
	if !ok {
		return fmt.Errorf("unsupported destination %T", dst)
	}
	*config = c.worker
	return nil
}

func (c fakeRawConfig) Exists(key string) bool {
	return key == "worker" && c.exists
}

func newTestPool() *Pool {
	return newPool(slog.New(slog.DiscardHandler), DefaultRestartDelay)
}

func shutdownPool(t *testing.T, p *Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := p.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown() = %v", err)
	}
}
