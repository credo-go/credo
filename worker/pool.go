package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/credo-go/credo"
)

type poolConfig struct {
	RestartDelay time.Duration `credo:"restart_delay"`
}

// Pool manages registered workers and integrates with app lifecycle.
type Pool struct {
	mu                  sync.Mutex
	definitions         []*Definition
	runners             []*runner
	logger              *slog.Logger
	cancel              context.CancelFunc
	wg                  sync.WaitGroup
	started             bool
	defaultRestartDelay time.Duration
}

// Register adds w to the application's worker pool.
//
// Registration is startup configuration, so misuse — nil worker, empty or
// duplicate name, cross-mode options, malformed schedule or worker config —
// panics rather than returning an error, per the credo package's "Panics
// and Errors" policy. (store.Register returns an error instead because it
// touches the outside world: it pings the connection.)
func Register(app *credo.App, w Worker, opts ...Option) {
	if app == nil {
		panic("worker: app must not be nil")
	}
	if isNilWorker(w) {
		panic("worker: worker must not be nil")
	}

	name := strings.TrimSpace(w.Name())
	if name == "" {
		panic("worker: worker name must not be empty")
	}

	o := options{}
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}

	if o.hasMaxRestarts && o.maxRestarts < 0 {
		panic(fmt.Sprintf("worker: max restarts must be >= 0, got %d", o.maxRestarts))
	}
	if o.hasRestartDelay && o.restartDelay < 0 {
		panic(fmt.Sprintf("worker: restart delay must be >= 0, got %s", o.restartDelay))
	}
	if o.hasMaxConsecutiveFailures && o.maxConsecutiveFailures < 0 {
		panic(fmt.Sprintf("worker: max consecutive failures must be >= 0, got %d", o.maxConsecutiveFailures))
	}

	var schedule *Schedule
	if o.hasSchedule {
		parsed, err := ParseSchedule(o.scheduleExpr)
		if err != nil {
			panic(err.Error())
		}
		schedule = parsed
	}

	if schedule != nil {
		if o.hasMaxRestarts {
			panic("worker: WithMaxRestarts is for continuous workers; use WithMaxConsecutiveFailures")
		}
		if o.hasRestartDelay {
			panic("worker: WithRestartDelay is for continuous workers")
		}
	} else {
		if o.hasMaxConsecutiveFailures {
			panic("worker: WithMaxConsecutiveFailures is for scheduled workers; use WithMaxRestarts")
		}
		if o.startImmediately {
			panic("worker: WithStartImmediately is for scheduled workers")
		}
	}

	p, err := ensurePool(app)
	if err != nil {
		panic(err.Error())
	}

	def := &Definition{
		name:             name,
		worker:           w,
		schedule:         schedule,
		startImmediately: o.startImmediately,
	}
	if schedule == nil {
		restartDelay := p.defaultRestartDelay
		if o.hasRestartDelay {
			restartDelay = o.restartDelay
		}
		// A zero delay would busy-loop a worker that fails immediately. Treat 0
		// as "use the default", matching how restart_delay is read from config.
		if restartDelay == 0 {
			restartDelay = DefaultRestartDelay
		}
		def.restartPolicy = restartPolicy{
			maxRestarts:  o.maxRestarts,
			restartDelay: restartDelay,
		}
	} else {
		def.failurePolicy = failurePolicy{
			maxConsecutiveFailures: o.maxConsecutiveFailures,
		}
	}

	if err := p.addDefinition(def); err != nil {
		panic(err.Error())
	}
}

func ensurePool(app *credo.App) (*Pool, error) {
	p, err := credo.Resolve[*Pool](app)
	if err == nil {
		return p, nil
	}

	cfg, err := loadPoolConfig(app)
	if err != nil {
		return nil, err
	}

	p = newPool(app.Logger().With("module", "worker"), cfg.RestartDelay)
	// ProvideValue also hands the pool's shutdown to the container: Pool
	// implements credo.Shutdowner, so app.Shutdown stops all workers without
	// an explicit OnShutdown hook.
	if err := credo.ProvideValue[*Pool](app, p); err != nil {
		resolved, resolveErr := credo.Resolve[*Pool](app)
		if resolveErr == nil {
			return resolved, nil
		}
		return nil, fmt.Errorf("worker: register pool: %w", errors.Join(err, resolveErr))
	}

	app.OnStart(func(ctx context.Context) error {
		return p.Start(ctx)
	})

	return p, nil
}

func loadPoolConfig(app *credo.App) (poolConfig, error) {
	cfg := poolConfig{RestartDelay: DefaultRestartDelay}

	raw, _ := credo.Resolve[credo.RawConfig](app)
	if raw == nil || !raw.Exists("worker") {
		return cfg, nil
	}
	if err := raw.Unmarshal("worker", &cfg); err != nil {
		return poolConfig{}, fmt.Errorf("worker: invalid config: %w", err)
	}
	if cfg.RestartDelay < 0 {
		return poolConfig{}, fmt.Errorf("worker: restart_delay must be >= 0, got %s", cfg.RestartDelay)
	}
	if cfg.RestartDelay == 0 {
		cfg.RestartDelay = DefaultRestartDelay
	}
	return cfg, nil
}

func newPool(logger *slog.Logger, defaultRestartDelay time.Duration) *Pool {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	if defaultRestartDelay < 0 {
		defaultRestartDelay = DefaultRestartDelay
	}
	return &Pool{
		logger:              logger,
		defaultRestartDelay: defaultRestartDelay,
	}
}

func (p *Pool) addDefinition(def *Definition) error {
	if def == nil {
		return fmt.Errorf("worker: definition must not be nil")
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.started {
		return fmt.Errorf("worker: pool already started")
	}
	for _, existing := range p.definitions {
		if existing.name == def.name {
			return fmt.Errorf("worker: duplicate worker name %q", def.name)
		}
	}

	p.definitions = append(p.definitions, def)
	return nil
}

// Start launches registered workers.
func (p *Pool) Start(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}

	p.mu.Lock()
	if p.started {
		p.mu.Unlock()
		return fmt.Errorf("worker: pool already started")
	}

	poolCtx, cancel := context.WithCancel(ctx)
	p.cancel = cancel
	p.started = true

	runners := make([]*runner, 0, len(p.definitions))
	for _, def := range p.definitions {
		r := newRunner(def)
		if def.schedule != nil {
			r.setStatus(StatusWaiting)
		} else {
			r.setStatus(StatusRunning)
		}
		p.runners = append(p.runners, r)
		runners = append(runners, r)
	}
	p.mu.Unlock()

	for _, r := range runners {
		if r.def.schedule != nil {
			p.wg.Go(func() { p.runScheduled(poolCtx, r) })
			continue
		}
		p.wg.Go(func() { p.runContinuous(poolCtx, r) })
	}

	return nil
}

// Shutdown stops all workers and waits for them to exit.
func (p *Pool) Shutdown(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}

	p.mu.Lock()
	cancel := p.cancel
	p.mu.Unlock()

	if cancel != nil {
		cancel()
	}

	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Workers returns a snapshot of registered worker state.
func (p *Pool) Workers() []Info {
	p.mu.Lock()
	started := p.started
	defs := make([]*Definition, len(p.definitions))
	copy(defs, p.definitions)
	runners := make([]*runner, len(p.runners))
	copy(runners, p.runners)
	p.mu.Unlock()

	if !started {
		infos := make([]Info, 0, len(defs))
		for _, def := range defs {
			infos = append(infos, Info{
				Name:     def.name,
				Kind:     def.Kind(),
				Schedule: def.scheduleExpr(),
				Status:   StatusIdle,
			})
		}
		return infos
	}

	infos := make([]Info, 0, len(runners))
	for _, r := range runners {
		infos = append(infos, r.snapshot())
	}
	return infos
}
