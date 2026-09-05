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
	internalhealth "github.com/credo-go/credo/internal/health"
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
	readiness           []internalhealth.ReadinessCheck // one stable probe per WithReadiness worker

	// managed marks a pool built by ensurePool. Only such a pool carries the
	// OnStart/OnDrain wiring and the protected DI binding; a *Pool published
	// into the container by other means is rejected by Register.
	managed bool

	// stopOnce starts the single stop sequence (cancel + wait). Shutdown is
	// reached twice on every teardown — from the OnDrain hook and again from
	// the container's Shutdowner pass — and both callers observe one result.
	stopOnce sync.Once
	// stopping is set under mu by the first Shutdown; Start refuses afterwards
	// so no goroutine can be added to wg once the wait has begun.
	stopping bool
	// stopped is closed once every worker goroutine has returned.
	stopped chan struct{}
}

// Register adds w to the application's worker pool.
//
// Workers are started during [credo.App.Run] and stopped during
// [credo.App.Shutdown]. Register must be called before the app is finalized or
// run. Use [MustRegister] when bootstrap code should fail fast by panicking.
func Register(app *credo.App, w Worker, opts ...Option) error {
	if app == nil {
		return fmt.Errorf("worker: app must not be nil")
	}
	if isNilWorker(w) {
		return fmt.Errorf("worker: worker must not be nil")
	}

	name := strings.TrimSpace(w.Name())
	if name == "" {
		return fmt.Errorf("worker: worker name must not be empty")
	}

	o, schedule, err := validateOptions(opts)
	if err != nil {
		return err
	}
	if o.hasReadiness {
		if err = internalhealth.ValidateName(readinessCheckName(name)); err != nil {
			return fmt.Errorf("worker: WithReadiness: %w", err)
		}
	}

	p, err := ensurePool(app)
	if err != nil {
		return err
	}

	return p.addDefinition(buildDefinition(name, w, o, schedule, p.defaultRestartDelay))
}

// validateOptions applies the registration options, checks their values, and
// parses the schedule; it also rejects options that belong to the other
// worker kind.
func validateOptions(opts []Option) (options, *Schedule, error) {
	o := options{}
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}

	if o.hasMaxRestarts && o.maxRestarts < 0 {
		return options{}, nil, fmt.Errorf("worker: max restarts must be >= 0, got %d", o.maxRestarts)
	}
	if o.hasRestartDelay && o.restartDelay < 0 {
		return options{}, nil, fmt.Errorf("worker: restart delay must be >= 0, got %s", o.restartDelay)
	}
	if o.hasMaxConsecutiveFailures && o.maxConsecutiveFailures < 0 {
		return options{}, nil, fmt.Errorf("worker: max consecutive failures must be >= 0, got %d", o.maxConsecutiveFailures)
	}
	if o.hasReadiness {
		if err := o.readiness.validate(o.hasSchedule); err != nil {
			return options{}, nil, err
		}
	}

	var schedule *Schedule
	if o.hasSchedule {
		parsed, err := ParseSchedule(o.scheduleExpr)
		if err != nil {
			return options{}, nil, err
		}
		schedule = parsed
	}

	if schedule != nil {
		if o.hasMaxRestarts {
			return options{}, nil, fmt.Errorf("worker: WithMaxRestarts is for continuous workers; use WithMaxConsecutiveFailures")
		}
		if o.hasRestartDelay {
			return options{}, nil, fmt.Errorf("worker: WithRestartDelay is for continuous workers")
		}
	} else {
		if o.hasMaxConsecutiveFailures {
			return options{}, nil, fmt.Errorf("worker: WithMaxConsecutiveFailures is for scheduled workers; use WithMaxRestarts")
		}
		if o.startImmediately {
			return options{}, nil, fmt.Errorf("worker: WithStartImmediately is for scheduled workers")
		}
	}
	return o, schedule, nil
}

// buildDefinition turns validated options into the immutable Definition,
// resolving the kind-specific restart or failure policy.
func buildDefinition(name string, w Worker, o options, schedule *Schedule, defaultRestartDelay time.Duration) *Definition {
	def := &Definition{
		name:             name,
		worker:           w,
		schedule:         schedule,
		startImmediately: o.startImmediately,
	}
	if o.hasReadiness {
		policy := o.readiness
		def.readiness = &policy
	}
	if schedule != nil {
		def.failurePolicy = failurePolicy{
			maxConsecutiveFailures: o.maxConsecutiveFailures,
		}
		return def
	}

	restartDelay := defaultRestartDelay
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
	return def
}

// MustRegister is like [Register] but panics on error.
func MustRegister(app *credo.App, w Worker, opts ...Option) {
	if err := Register(app, w, opts...); err != nil {
		panic(err)
	}
}

// registrationProbe is never registered in the container. Asking whether it
// could be registered answers exactly one question — is the container still
// open — which is the registration window every Register call must respect,
// not only the first one that creates the pool.
type registrationProbe struct{}

func ensurePool(app *credo.App) (*Pool, error) {
	if err := app.CanProvideValue[registrationProbe](); err != nil {
		return nil, fmt.Errorf("worker: Register after app.Finalize: %w", err)
	}

	if app.Has[*Pool]() {
		return adoptPool(app)
	}

	cfg, err := loadPoolConfig(app)
	if err != nil {
		return nil, err
	}

	p := newPool(app.Logger().With("module", "worker"), cfg.RestartDelay)
	p.managed = true
	// The binding is protected: the pool wired into OnStart/OnDrain and the
	// readiness seam must stay the pool the container hands out, so a later
	// Replace[*Pool] is rejected rather than silently splitting the two.
	if err := app.ProvideProtectedValue[*Pool](p); err != nil {
		// Lost a registration race: the winner published (and wired) its pool.
		adopted, adoptErr := adoptPool(app)
		if adoptErr != nil {
			return nil, fmt.Errorf("worker: register pool: %w", errors.Join(err, adoptErr))
		}
		return adopted, nil
	}

	// Readiness contributions reach the health engine through the
	// module-internal DI seam, resolved lazily on each /ready request, so
	// worker.Register and UseHealth may run in either order.
	if _, _, err := app.Replace[internalhealth.ReadinessFunc](p.readinessChecks); err != nil {
		return nil, fmt.Errorf("worker: register readiness seam: %w", err)
	}

	app.OnStart(func(lifecycleCtx context.Context) error {
		return p.Start(lifecycleCtx)
	})
	// Workers finish in the OnDrain phase — after lifecycle cancellation,
	// concurrently with the HTTP drain, and before DI singletons are torn
	// down — so a worker's final batch never races the resources it uses.
	// Pool also implements credo.Shutdowner; that later container pass finds
	// the stop sequence already complete and returns its result.
	app.OnDrain(p.Shutdown)

	return p, nil
}

// adoptPool accepts the already-registered pool only when ensurePool built
// it, because only that pool carries the lifecycle and readiness wiring. The
// registration-time read is an adoption: the value is validated and its
// binding protected atomically, never constructed — a *Pool registered
// through Provide is rejected without running its constructor.
func adoptPool(app *credo.App) (*Pool, error) {
	p, err := app.AdoptValue[*Pool](func(p *Pool) error {
		if p == nil || !p.managed {
			return errors.New("a *worker.Pool provided outside worker.Register is not supported")
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("worker: %w", err)
	}
	return p, nil
}

// loadPoolConfig reads the optional "worker" section straight from the
// application's configuration: registration runs before Finalize, when
// Resolve is not yet available.
func loadPoolConfig(app *credo.App) (poolConfig, error) {
	cfg := poolConfig{RestartDelay: DefaultRestartDelay}

	if !app.ConfigExists("worker") {
		return cfg, nil
	}
	loaded, err := app.GetConfig[poolConfig]("worker")
	if err != nil {
		return poolConfig{}, fmt.Errorf("worker: invalid config: %w", err)
	}
	cfg = loaded
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
		stopped:             make(chan struct{}),
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
	if def.readiness != nil {
		p.readiness = append(p.readiness, internalhealth.ReadinessCheck{
			Name:  readinessCheckName(def.name),
			Probe: p.newReadinessProbe(def),
		})
	}
	return nil
}

// Start launches registered workers.
func (p *Pool) Start(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}

	p.mu.Lock()
	if p.stopping {
		p.mu.Unlock()
		return fmt.Errorf("worker: pool already shut down")
	}
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
	// Launch under mu: Shutdown sets stopping under the same lock before it
	// starts waiting on wg, so either every worker has joined wg before the
	// wait begins or Start is refused — a goroutine can never be added to a
	// WaitGroup that a concurrent Shutdown is already waiting on.
	for _, r := range runners {
		if r.def.schedule != nil {
			p.wg.Go(func() { p.runScheduled(poolCtx, r) })
			continue
		}
		p.wg.Go(func() { p.runContinuous(poolCtx, r) })
	}
	p.mu.Unlock()

	return nil
}

// Shutdown stops all workers and waits for them to exit. The first call
// cancels the pool context and starts the wait; every call, including a
// concurrent or later one, returns nil once all workers have returned —
// completion takes precedence over an already-ended ctx — and ctx.Err() only
// while workers are still running when ctx ends. A pool that was never
// started shuts down immediately, and Start is refused afterwards.
func (p *Pool) Shutdown(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}

	p.stopOnce.Do(func() {
		p.mu.Lock()
		p.stopping = true
		cancel := p.cancel
		if p.stopped == nil { // zero-value Pool
			p.stopped = make(chan struct{})
		}
		stopped := p.stopped
		p.mu.Unlock()

		if cancel != nil {
			cancel()
		}
		go func() {
			p.wg.Wait()
			close(stopped)
		}()
	})

	p.mu.Lock()
	stopped := p.stopped
	p.mu.Unlock()

	// Completion first: a select with both cases ready picks at random, which
	// would make the result of a finished shutdown depend on the caller's ctx.
	select {
	case <-stopped:
		return nil
	default:
	}
	select {
	case <-stopped:
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
