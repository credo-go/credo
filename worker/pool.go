package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/credo-go/credo"
	internalhealth "github.com/credo-go/credo/internal/health"
)

type poolConfig struct {
	RestartDelay time.Duration `credo:"restart_delay"`
}

// Pool manages registered workers and integrates with app lifecycle.
type Pool struct {
	mu                  sync.Mutex
	definitions         []*definition
	runners             []*runner
	logger              *slog.Logger
	cancel              context.CancelFunc
	wg                  sync.WaitGroup
	defaultRestartDelay time.Duration
	readiness           []internalhealth.ReadinessCheck // one stable probe per WithReadiness worker

	// claimed is set under mu by the first Start, before any worker is
	// resolved; addDefinition and every later Start are refused from then on.
	claimed bool
	// published is set under mu once Start has created the runners. Until
	// then — including after a failed or pre-empted Start — Workers reports
	// every definition as idle.
	published bool

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

// Register adds w to the application's worker pool under name.
//
// The name identifies the registration: it must be unique within the pool,
// non-empty, and free of surrounding whitespace and control characters. It
// appears in logs, in [Pool.Workers] and in the readiness check name, and
// [WorkerName] returns it inside Run. Registering the same instance under two
// names runs it on two independent loops, so it must then be safe for
// concurrent Run calls.
//
// Workers are started during [credo.App.Run] and stopped during
// [credo.App.Shutdown]. Register must be called before the app is finalized or
// run. Use [RegisterProvided] for a worker constructed by the DI container,
// and [MustRegister] when bootstrap code should fail fast by panicking.
func Register(app *credo.App, name string, w Worker, opts ...Option) error {
	if isNilWorker(w) {
		return fmt.Errorf("worker: worker %q must not be nil", name)
	}
	return register(app, name, instance(w), fmt.Sprintf("%T", w), opts)
}

// MustRegister is like [Register] but panics on error.
func MustRegister(app *credo.App, name string, w Worker, opts ...Option) {
	if err := Register(app, name, w, opts...); err != nil {
		panic(err)
	}
}

// RegisterProvided registers, under name, the worker that the application's
// DI container provides as T:
//
//	app.MustProvide[*InvoiceWorker](NewInvoiceWorker)
//	worker.MustRegisterProvided[*InvoiceWorker](app, "invoice-worker",
//		worker.WithSchedule("@every 1m"))
//
// T is resolved once, when the pool starts (after [credo.App.Finalize], before
// the server accepts traffic), so RegisterProvided and the matching Provide
// may be called in either order. T may be an interface bound with
// [credo.App.Alias]. A resolution failure — T not provided, a constructor
// error or panic, a nil result — fails the application's startup with an
// error naming the worker and the type, and no worker is started. The name
// and options follow the same rules as [Register].
func RegisterProvided[T Worker](app *credo.App, name string, opts ...Option) error {
	resolve := func() (Worker, error) {
		w, err := app.Resolve[T]()
		if err != nil {
			return nil, err
		}
		return w, nil
	}
	return register(app, name, resolve, reflect.TypeFor[T]().String(), opts)
}

// MustRegisterProvided is like [RegisterProvided] but panics on error.
func MustRegisterProvided[T Worker](app *credo.App, name string, opts ...Option) {
	if err := RegisterProvided[T](app, name, opts...); err != nil {
		panic(err)
	}
}

// instance is the resolver of a worker registered by value.
func instance(w Worker) func() (Worker, error) {
	return func() (Worker, error) { return w, nil }
}

// register is the single registration path behind Register and
// RegisterProvided: resolve yields the worker when the pool starts, source
// names it in resolution errors.
func register(app *credo.App, name string, resolve func() (Worker, error), source string, opts []Option) error {
	if app == nil {
		return fmt.Errorf("worker: app must not be nil")
	}
	if err := validateName(name); err != nil {
		return err
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

	def := buildDefinition(name, o, schedule, p.defaultRestartDelay)
	def.resolve = resolve
	def.source = source
	return p.addDefinition(def)
}

// validateName applies the worker name rules. Names are never normalized, so
// the registered name is exactly the one every log line and snapshot reports.
// The framework reserves no prefix: health's reserved "credo." prefix applies
// to the full readiness check name "worker:<name>", never to a worker name.
func validateName(name string) error {
	if name == "" {
		return errors.New("worker: name must not be empty")
	}
	if strings.TrimSpace(name) != name {
		return fmt.Errorf("worker: name %q must not have leading or trailing whitespace", name)
	}
	for _, char := range name {
		if unicode.IsControl(char) {
			return fmt.Errorf("worker: name %q must not contain control characters", name)
		}
	}
	return nil
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

// buildDefinition turns validated options into the immutable definition,
// resolving the kind-specific restart or failure policy. The caller sets the
// worker resolver.
func buildDefinition(name string, o options, schedule *Schedule, defaultRestartDelay time.Duration) *definition {
	def := &definition{
		name:             name,
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

func (p *Pool) addDefinition(def *definition) error {
	if def == nil {
		return fmt.Errorf("worker: definition must not be nil")
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.claimed {
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

// Start resolves every registered worker and launches them. It is
// all-or-nothing: when any worker fails to resolve, Start returns the joined
// errors, each naming its worker, and launches none.
//
// Start runs in three steps so that user constructors never run under the
// pool lock: it claims the pool (refusing further registrations), resolves the
// workers outside the lock, then publishes the runners — unless a Shutdown
// arrived meanwhile, which wins.
func (p *Pool) Start(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}

	p.mu.Lock()
	if p.stopping {
		p.mu.Unlock()
		return fmt.Errorf("worker: pool already shut down")
	}
	if p.claimed {
		p.mu.Unlock()
		return fmt.Errorf("worker: pool already started")
	}
	p.claimed = true
	defs := slices.Clone(p.definitions)
	p.mu.Unlock()

	workers := make([]Worker, len(defs))
	var errs []error
	for i, def := range defs {
		w, err := def.resolve()
		if err == nil && isNilWorker(w) {
			err = errors.New("resolved to nil")
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("worker: %q: resolve %s: %w", def.name, def.source, err))
			continue
		}
		workers[i] = w
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	// A Shutdown that arrived while the workers were resolving has already
	// begun its wait on wg; nothing may join it now.
	if p.stopping {
		return fmt.Errorf("worker: pool already shut down")
	}

	poolCtx, cancel := context.WithCancel(ctx)
	p.cancel = cancel

	runners := make([]*runner, 0, len(defs))
	for i, def := range defs {
		r := newRunner(def, workers[i])
		if def.schedule != nil {
			r.setStatus(StatusWaiting)
		}
		runners = append(runners, r)
	}
	p.runners = runners
	p.published = true
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
	published := p.published
	defs := slices.Clone(p.definitions)
	runners := slices.Clone(p.runners)
	p.mu.Unlock()

	if !published {
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
