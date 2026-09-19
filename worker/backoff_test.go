package worker

import (
	"context"
	"errors"
	"math"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/credo-go/credo"
)

// lowerBound and upperBound pin the restart jitter to one end of the window.
func lowerBound(lo, _ time.Duration) time.Duration { return lo }
func upperBound(_, hi time.Duration) time.Duration { return hi }

type window struct{ lo, hi time.Duration }

// windows feeds nextDelay one failed run per entry of runs (its duration) and
// returns the window each delay was drawn from.
func windows(base, maxDelay time.Duration, runs []time.Duration) []window {
	var got []window
	c := &continuousPolicy{
		p: &Pool{jitter: func(lo, hi time.Duration) time.Duration {
			got = append(got, window{lo, hi})
			return lo
		}},
		r: &runner{def: &definition{restartPolicy: restartPolicy{
			restartDelay:    base,
			maxRestartDelay: maxDelay,
		}}},
	}
	for _, d := range runs {
		c.nextDelay(d)
	}
	return got
}

// immediate returns n run durations of zero: runs that fail at once.
func immediate(n int) []time.Duration { return make([]time.Duration, n) }

func TestNextDelay_DoublesUpToTheCap(t *testing.T) {
	const s = time.Second
	got := windows(3*s, time.Minute, immediate(8))
	want := []window{
		{3 * s, 3 * s}, // the first delay is exactly the base
		{3 * s, 6 * s},
		{6 * s, 12 * s},
		{12 * s, 24 * s},
		{24 * s, 48 * s},
		{30 * s, 60 * s}, // 96 s is capped
		{30 * s, 60 * s},
		{30 * s, 60 * s},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("windows = %v, want %v", got, want)
	}
}

func TestNextDelay_CapEqualToBaseIsFixed(t *testing.T) {
	for i, w := range windows(time.Minute, time.Minute, immediate(6)) {
		if w != (window{time.Minute, time.Minute}) {
			t.Fatalf("delay %d window = %v, want exactly one minute", i+1, w)
		}
	}
}

func TestNextDelay_SaturatesWithoutOverflow(t *testing.T) {
	const largest = time.Duration(math.MaxInt64)

	got := windows(time.Nanosecond, largest, immediate(200))
	for i, w := range got {
		if w.lo < time.Nanosecond || w.lo > w.hi {
			t.Fatalf("delay %d window = %v, want base <= lo <= hi", i+1, w)
		}
	}
	if last := got[len(got)-1]; last.hi != largest {
		t.Fatalf("last window = %v, want the ceiling saturated at the cap", last)
	}

	base := largest - 1
	want := []window{{base, base}, {base, largest}, {base, largest}}
	if got := windows(base, largest, immediate(3)); !slices.Equal(got, want) {
		t.Fatalf("near the largest duration: windows = %v, want %v", got, want)
	}
}

func TestNextDelay_ResetsAfterARunOfAtLeastTheCap(t *testing.T) {
	const s = time.Second
	tests := []struct {
		name string
		runs []time.Duration
		want []window
	}{
		{"a run just short of the cap keeps the sequence",
			[]time.Duration{0, 0, 0, time.Minute - time.Nanosecond},
			[]window{{3 * s, 3 * s}, {3 * s, 6 * s}, {6 * s, 12 * s}, {12 * s, 24 * s}}},
		{"a run of exactly the cap starts a new one",
			[]time.Duration{0, 0, time.Minute, 0},
			[]window{{3 * s, 3 * s}, {3 * s, 6 * s}, {3 * s, 3 * s}, {3 * s, 6 * s}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := windows(3*s, time.Minute, tt.runs); !slices.Equal(got, tt.want) {
				t.Fatalf("windows = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestUniformJitter(t *testing.T) {
	if got := uniformJitter(time.Second, time.Second); got != time.Second {
		t.Fatalf("empty window: got %s, want lo", got)
	}
	lo, hi := time.Second, time.Second+2*time.Nanosecond
	seen := map[time.Duration]bool{}
	for range 1000 {
		d := uniformJitter(lo, hi)
		if d < lo || d > hi {
			t.Fatalf("uniformJitter(%s, %s) = %s, outside the window", lo, hi, d)
		}
		seen[d] = true
	}
	if len(seen) != 3 {
		t.Fatalf("1000 draws hit %d of the 3 values in the window, want all", len(seen))
	}
}

func TestRegister_EffectiveMaxRestartDelay(t *testing.T) {
	const m = time.Minute
	tests := []struct {
		name     string
		pool     *poolConfig // nil: no worker section
		opts     []Option
		wantBase time.Duration
		wantCap  time.Duration
	}{
		{"default", nil, nil, DefaultRestartDelay, DefaultMaxRestartDelay},
		{"pool cap", &poolConfig{MaxRestartDelay: 5 * m}, nil, DefaultRestartDelay, 5 * m},
		{"zero pool cap means the default", &poolConfig{}, nil, DefaultRestartDelay, DefaultMaxRestartDelay},
		{"explicit zero skips the pool cap", &poolConfig{MaxRestartDelay: 5 * m},
			[]Option{WithMaxRestartDelay(0)}, DefaultRestartDelay, DefaultMaxRestartDelay},
		{"explicit cap overrides the pool cap", &poolConfig{MaxRestartDelay: 5 * m},
			[]Option{WithMaxRestartDelay(2 * m)}, DefaultRestartDelay, 2 * m},
		{"a larger base raises the default cap", nil,
			[]Option{WithRestartDelay(10 * m)}, 10 * m, 10 * m},
		{"a larger base raises an explicit zero cap", nil,
			[]Option{WithRestartDelay(10 * m), WithMaxRestartDelay(0)}, 10 * m, 10 * m},
		{"a pool cap below the base is raised, not an error", &poolConfig{MaxRestartDelay: 2 * time.Second},
			nil, DefaultRestartDelay, DefaultRestartDelay},
		{"equal options give a fixed delay", &poolConfig{MaxRestartDelay: 5 * m},
			[]Option{WithRestartDelay(m), WithMaxRestartDelay(m)}, m, m},
		{"a base alone grows to a larger pool cap", &poolConfig{MaxRestartDelay: 5 * m},
			[]Option{WithRestartDelay(m)}, m, 5 * m},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var opts []credo.Option
			if tt.pool != nil {
				opts = append(opts, credo.WithRawConfig(fakeRawConfig{exists: true, worker: *tt.pool}))
			}
			app := newTestApp(t, opts...)
			MustRegister(app, "job", blockingFunc(), tt.opts...)
			finalize(t, app)
			pool, err := app.Resolve[*Pool]()
			if err != nil {
				t.Fatal(err)
			}
			cfg := pool.Workers()[0].Config
			if cfg.RestartDelay != tt.wantBase || cfg.MaxRestartDelay != tt.wantCap {
				t.Fatalf("Config = base %s, cap %s; want %s, %s", cfg.RestartDelay, cfg.MaxRestartDelay, tt.wantBase, tt.wantCap)
			}
			if got := pool.definitions[0].restartPolicy; got.restartDelay != cfg.RestartDelay || got.maxRestartDelay != cfg.MaxRestartDelay {
				t.Fatalf("runner policy %+v differs from Config (Config must report what the runner uses)", got)
			}
		})
	}
}

func TestRegister_MaxRestartDelayErrors(t *testing.T) {
	tests := []struct {
		name string
		pool *poolConfig
		opts []Option
		want string
	}{
		{"negative", nil, []Option{WithMaxRestartDelay(-time.Second)}, "max restart delay must be >= 0, got -1s"},
		{"below the base", nil, []Option{WithRestartDelay(10 * time.Second), WithMaxRestartDelay(5 * time.Second)},
			"max restart delay must be >= the restart delay 10s, got 5s"},
		{"below the pool base", &poolConfig{RestartDelay: 10 * time.Second}, []Option{WithMaxRestartDelay(5 * time.Second)},
			"max restart delay must be >= the restart delay 10s, got 5s"},
		{"scheduled, zero included", nil, []Option{WithSchedule("@every 1m"), WithMaxRestartDelay(0)},
			"WithMaxRestartDelay is for continuous workers"},
		{"negative pool cap", &poolConfig{MaxRestartDelay: -time.Second}, nil, "max_restart_delay must be >= 0, got -1s"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var opts []credo.Option
			if tt.pool != nil {
				opts = append(opts, credo.WithRawConfig(fakeRawConfig{exists: true, worker: *tt.pool}))
			}
			err := Register(newTestApp(t, opts...), "job", blockingFunc(), tt.opts...)
			requireErrContaining(t, err, tt.want)
		})
	}
}

// runStarts starts p with one continuous worker, lets span of virtual time
// pass and returns the offset of every run's start. run receives the 1-based
// run number. It must be called inside a synctest bubble.
func runStarts(t *testing.T, p *Pool, span time.Duration, run func(ctx context.Context, n int) error, opts ...Option) []time.Duration {
	t.Helper()
	var mu sync.Mutex
	var starts []time.Duration
	origin := time.Now()
	startPool(t, p, mustDefinition(t, "consumer", Func(func(ctx context.Context) error {
		mu.Lock()
		starts = append(starts, time.Since(origin))
		n := len(starts)
		mu.Unlock()
		return run(ctx, n)
	}), opts...))
	time.Sleep(span)
	synctest.Wait()
	mu.Lock()
	defer mu.Unlock()
	return slices.Clone(starts)
}

func fail(context.Context, int) error { return errors.New("boom") }

func seconds(values ...int) []time.Duration {
	out := make([]time.Duration, len(values))
	for i, v := range values {
		out[i] = time.Duration(v) * time.Second
	}
	return out
}

func TestRunContinuous_BackoffWithTheDefaults(t *testing.T) {
	tests := []struct {
		name string
		pick func(lo, hi time.Duration) time.Duration
		span time.Duration
		want []time.Duration
	}{
		// Delays 3, 3, 6, 12, 24, 30, 30 s.
		{"lower bound", lowerBound, 110 * time.Second, seconds(0, 3, 6, 12, 24, 48, 78, 108)},
		// Delays 3, 6, 12, 24, 48, 60, 60 s.
		{"upper bound", upperBound, 215 * time.Second, seconds(0, 3, 9, 21, 45, 93, 153, 213)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				p := newTestPool()
				p.jitter = tt.pick
				starts := runStarts(t, p, tt.span, fail)
				if !slices.Equal(starts, tt.want) {
					t.Fatalf("runs started at %v, want %v", starts, tt.want)
				}
				info := p.Workers()[0]
				if info.Status != StatusWaiting || info.Restarts != int64(len(tt.want)-1) {
					t.Fatalf("%+v, want waiting with %d restarts", info, len(tt.want)-1)
				}

				// Shutdown during a long wait ends the loop without a restart.
				shutdownPool(t, p)
				if after := p.Workers()[0]; after.Status != StatusStopped || after.Restarts != info.Restarts {
					t.Fatalf("after shutdown in the delay: %+v, want stopped with %d restarts", after, info.Restarts)
				}
			})
		})
	}
}

func TestRunContinuous_BackoffResetsAfterALongRun(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := newTestPool()
		p.jitter = upperBound
		starts := runStarts(t, p, 80*time.Second, func(ctx context.Context, n int) error {
			if n == 3 {
				// Run for exactly the cap before failing.
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(DefaultMaxRestartDelay):
				}
			}
			return errors.New("boom")
		})
		// Delays 3 and 6 s; run 3 lasts 60 s (until 69 s) and resets the
		// sequence, so the next delays are 3 and 6 s again, not 12 s.
		if want := seconds(0, 3, 9, 72, 78); !slices.Equal(starts, want) {
			t.Fatalf("runs started at %v, want %v", starts, want)
		}
		if info := p.Workers()[0]; info.Restarts != 4 {
			t.Fatalf("Restarts = %d, want 4: a reset never changes the count", info.Restarts)
		}
		shutdownPool(t, p)
	})
}

func TestRunContinuous_BackoffAppliesToEveryFailure(t *testing.T) {
	cases := map[string]func(context.Context, int) error{
		"early nil return": func(context.Context, int) error { return nil },
		"panic":            func(context.Context, int) error { panic("kaboom") },
	}
	for name, run := range cases {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				p := newTestPool()
				p.jitter = upperBound
				if starts, want := runStarts(t, p, 22*time.Second, run), seconds(0, 3, 9, 21); !slices.Equal(starts, want) {
					t.Fatalf("runs started at %v, want %v", starts, want)
				}
				shutdownPool(t, p)
			})
		})
	}
}

func TestRunContinuous_BackoffReachesFailedLater(t *testing.T) {
	tests := []struct {
		name     string
		pick     func(lo, hi time.Duration) time.Duration
		failedAt time.Duration
	}{
		{"lower bound", lowerBound, 48 * time.Second}, // 3+3+6+12+24
		{"upper bound", upperBound, 93 * time.Second}, // 3+6+12+24+48
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				p := newTestPool()
				p.jitter = tt.pick
				runStarts(t, p, tt.failedAt-time.Second, fail, WithMaxRestarts(5))
				if info := p.Workers()[0]; info.Status != StatusWaiting || info.Restarts != 4 {
					t.Fatalf("one second before: %+v, want waiting with 4 restarts", info)
				}
				time.Sleep(time.Second)
				synctest.Wait()
				if info := p.Workers()[0]; info.Status != StatusFailed || info.Restarts != 5 {
					t.Fatalf("at %s: %+v, want failed with 5 restarts", tt.failedAt, info)
				}
				shutdownPool(t, p)
			})
		})
	}
}

func TestRunContinuous_NextRestartIn(t *testing.T) {
	t.Run("planned restarts carry the delay; the last failure does not", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			logs := newLogCapture()
			p := newPool(logs.logger(), poolConfig{})
			p.jitter = upperBound
			runStarts(t, p, 20*time.Second, fail, WithMaxRestarts(2))
			shutdownPool(t, p)

			var got []any
			var messages []string
			for _, rec := range logs.all() {
				switch rec.Message {
				case "worker run failed":
					got = append(got, rec.Attrs["next_restart_in"])
					messages = append(messages, rec.Message)
				case "worker exceeded max restarts":
					messages = append(messages, rec.Message)
				}
			}
			if want := []any{3 * time.Second, 6 * time.Second, nil}; !slices.Equal(got, want) {
				t.Fatalf("next_restart_in = %v, want %v", got, want)
			}
			want := []string{"worker run failed", "worker run failed", "worker run failed", "worker exceeded max restarts"}
			if !slices.Equal(messages, want) {
				t.Fatalf("lines = %q, want %q", messages, want)
			}
		})
	})
	t.Run("a failure during shutdown plans nothing", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			logs := newLogCapture()
			p := newPool(logs.logger(), poolConfig{})
			startPool(t, p, mustDefinition(t, "consumer", Func(func(ctx context.Context) error {
				<-ctx.Done()
				return errors.New("flush failed")
			})))
			synctest.Wait()
			shutdownPool(t, p)

			lines := logs.withMessage("worker run failed")
			if len(lines) != 1 {
				t.Fatalf("%d failure lines, want 1", len(lines))
			}
			if v, ok := lines[0].Attrs["next_restart_in"]; ok {
				t.Fatalf("next_restart_in = %v on a failure during shutdown, want absent", v)
			}
		})
	})
}
