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
)

// lowerBound and upperBound pin the restart jitter to one end of the window.
func lowerBound(lo, _ time.Duration) time.Duration { return lo }
func upperBound(_, hi time.Duration) time.Duration { return hi }

type window struct{ lo, hi time.Duration }

// windowsFor feeds the continuous policy of def one failed run per entry of
// runs (its duration) and returns the window each delay was drawn from.
func windowsFor(def *definition, runs []time.Duration) []window {
	var got []window
	c := &continuousPolicy{
		s: &Supervisor{jitter: func(lo, hi time.Duration) time.Duration {
			got = append(got, window{lo, hi})
			return lo
		}},
		r: &runner{def: def},
	}
	for _, d := range runs {
		c.nextDelay(d)
	}
	return got
}

// windows is windowsFor with an already resolved floor and cap.
func windows(base, maxDelay time.Duration, runs []time.Duration) []window {
	return windowsFor(&definition{name: "job", kind: KindContinuous, continuous: ContinuousConfig{
		Restart: Restart{MinDelay: base, MaxDelay: maxDelay},
	}}, runs)
}

// immediate returns n run durations of zero: runs that fail at once.
func immediate(n int) []time.Duration { return make([]time.Duration, n) }

func TestNextCeiling(t *testing.T) {
	const s = time.Second
	const largest = time.Duration(math.MaxInt64)
	tests := []struct {
		name                 string
		prev, base, maxDelay time.Duration
		want                 time.Duration
	}{
		{"a new sequence starts at the base", 0, 3 * s, time.Minute, 3 * s},
		{"doubles", 3 * s, 3 * s, time.Minute, 6 * s},
		{"doubles up to exactly the cap", 30 * s, 3 * s, time.Minute, time.Minute},
		{"saturates past half the cap", 48 * s, 3 * s, time.Minute, time.Minute},
		{"stays at the cap", time.Minute, 3 * s, time.Minute, time.Minute},
		{"a cap equal to the base is fixed", time.Minute, time.Minute, time.Minute, time.Minute},
		{"no overflow near the largest duration", largest - 1, largest - 1, largest, largest},
		{"no overflow from half the largest duration", largest/2 + 1, 1, largest, largest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := nextCeiling(tt.prev, tt.base, tt.maxDelay); got != tt.want {
				t.Fatalf("nextCeiling(%s, %s, %s) = %s, want %s", tt.prev, tt.base, tt.maxDelay, got, tt.want)
			}
		})
	}
}

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
		{"a run longer than the cap starts a new one",
			[]time.Duration{0, 0, 0, 0, 0, time.Hour},
			[]window{{3 * s, 3 * s}, {3 * s, 6 * s}, {6 * s, 12 * s}, {12 * s, 24 * s}, {24 * s, 48 * s}, {3 * s, 3 * s}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := windows(3*s, time.Minute, tt.runs); !slices.Equal(got, tt.want) {
				t.Fatalf("windows = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestNextDelay_UsesTheResolvedRestart drives the backoff from the Restart a
// registration resolves to: the floor and cap the policy uses are the ones
// Info reports.
func TestNextDelay_UsesTheResolvedRestart(t *testing.T) {
	const s, m = time.Second, time.Minute
	fixed := func(d time.Duration, n int) []window {
		out := make([]window, n)
		for i := range out {
			out[i] = window{d, d}
		}
		return out
	}
	tests := []struct {
		name    string
		restart Restart
		want    []window
	}{
		{"the defaults", Restart{},
			[]window{{3 * s, 3 * s}, {3 * s, 6 * s}, {6 * s, 12 * s}, {12 * s, 24 * s}, {24 * s, 48 * s}, {30 * s, m}}},
		{"a cap alone keeps the default floor", Restart{MaxDelay: 5 * m},
			[]window{{3 * s, 3 * s}, {3 * s, 6 * s}, {6 * s, 12 * s}, {12 * s, 24 * s}, {24 * s, 48 * s},
				{48 * s, 96 * s}, {96 * s, 192 * s}, {150 * s, 5 * m}}},
		{"a floor alone grows to the default cap", Restart{MinDelay: 20 * s},
			[]window{{20 * s, 20 * s}, {20 * s, 40 * s}, {30 * s, m}, {30 * s, m}}},
		{"a floor above the default cap is a fixed delay", Restart{MinDelay: 10 * m}, fixed(10*m, 4)},
		{"a floor of exactly the default cap is a fixed delay", Restart{MinDelay: m}, fixed(m, 4)},
		{"equal floor and cap are a fixed delay", Restart{MinDelay: 5 * s, MaxDelay: 5 * s}, fixed(5*s, 4)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			def := continuousDef("job", ContinuousConfig{Restart: tt.restart})
			info := def.info(runState{status: StatusPending})
			if got := info.Continuous.Restart; got != def.continuous.Restart {
				t.Fatalf("Info reports %+v, the policy uses %+v", got, def.continuous.Restart)
			}
			if got := windowsFor(def, immediate(len(tt.want))); !slices.Equal(got, tt.want) {
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

// backoffRunStarts adds one continuous worker under cfg to s, starts it, lets
// span of virtual time pass and returns the component and the offset of every
// run's start. run receives the 1-based run number. It must be called inside a
// synctest bubble.
func backoffRunStarts(t *testing.T, s *Supervisor, span time.Duration, run func(ctx context.Context, n int) error,
	cfg ...ContinuousConfig) (*component, []time.Duration) {
	t.Helper()
	var mu sync.Mutex
	var starts []time.Duration
	origin := time.Now()
	c := addWorker(s, continuousDef("consumer", cfg...), Func(func(ctx context.Context) error {
		mu.Lock()
		starts = append(starts, time.Since(origin))
		n := len(starts)
		mu.Unlock()
		return run(ctx, n)
	}))
	startWorker(t, c)
	time.Sleep(span)
	synctest.Wait()
	mu.Lock()
	defer mu.Unlock()
	return c, slices.Clone(starts)
}

func backoffFail(context.Context, int) error { return errors.New("boom") }

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
				s := newTestSupervisor()
				s.jitter = tt.pick
				c, starts := backoffRunStarts(t, s, tt.span, backoffFail)
				if !slices.Equal(starts, tt.want) {
					t.Fatalf("runs started at %v, want %v", starts, tt.want)
				}
				info := s.Snapshot()[0]
				if info.Status != StatusBackoff || info.Restarts != int64(len(tt.want)-1) {
					t.Fatalf("%+v, want backoff with %d restarts", info, len(tt.want)-1)
				}

				// Shutdown during a long wait ends the loop without a restart.
				stopWorker(t, c)
				if after := s.Snapshot()[0]; after.Status != StatusStopped || after.Restarts != info.Restarts ||
					after.LastError != "boom" || !after.LastStartedAt.Equal(info.LastStartedAt) {
					t.Fatalf("after shutdown in the delay: %+v, want stopped with %d restarts and the last run kept",
						after, info.Restarts)
				}
				if n := len(starts); n != len(tt.want) {
					t.Fatalf("%d runs, want no run after shutdown", n)
				}
			})
		})
	}
}

func TestRunContinuous_BackoffResetsAfterALongRun(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newTestSupervisor()
		s.jitter = upperBound
		_, starts := backoffRunStarts(t, s, 80*time.Second, func(ctx context.Context, n int) error {
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
		if info := s.Snapshot()[0]; info.Restarts != 4 {
			t.Fatalf("Restarts = %d, want 4: a reset never changes the count", info.Restarts)
		}
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
				s := newTestSupervisor()
				s.jitter = upperBound
				_, starts := backoffRunStarts(t, s, 22*time.Second, run)
				if want := seconds(0, 3, 9, 21); !slices.Equal(starts, want) {
					t.Fatalf("runs started at %v, want %v", starts, want)
				}
			})
		})
	}
}

func TestRunContinuous_BackoffFixedDelay(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newTestSupervisor()
		s.jitter = upperBound
		// A floor of ten minutes alone raises the cap to it: a fixed delay.
		_, starts := backoffRunStarts(t, s, 31*time.Minute, backoffFail,
			ContinuousConfig{Restart: Restart{MinDelay: 10 * time.Minute}})
		want := []time.Duration{0, 10 * time.Minute, 20 * time.Minute, 30 * time.Minute}
		if !slices.Equal(starts, want) {
			t.Fatalf("runs started at %v, want %v", starts, want)
		}
	})
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
				s := newTestSupervisor()
				s.jitter = tt.pick
				backoffRunStarts(t, s, tt.failedAt-time.Second, backoffFail,
					ContinuousConfig{Restart: Restart{Limit: 5}})
				if info := s.Snapshot()[0]; info.Status != StatusBackoff || info.Restarts != 4 {
					t.Fatalf("one second before: %+v, want backoff with 4 restarts", info)
				}
				time.Sleep(time.Second)
				synctest.Wait()
				if info := s.Snapshot()[0]; info.Status != StatusFailed || info.Restarts != 5 {
					t.Fatalf("at %s: %+v, want failed with 5 restarts", tt.failedAt, info)
				}
			})
		})
	}
}

func TestRunContinuous_NextRestartIn(t *testing.T) {
	t.Run("planned restarts carry the delay; the last failure does not", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			logs := newLogCapture()
			s := newTestSupervisorWithLogger(logs.logger())
			s.jitter = upperBound
			backoffRunStarts(t, s, 20*time.Second, backoffFail, ContinuousConfig{Restart: Restart{Limit: 2}})

			var got []any
			var messages []string
			for _, rec := range logs.all() {
				switch rec.Message {
				case "worker run failed":
					got = append(got, rec.Attrs["next_restart_in"])
					messages = append(messages, rec.Message)
				case "worker failed":
					messages = append(messages, rec.Message)
					if rec.Attrs["reason"] != "restart_limit" || rec.Attrs["limit"] != int64(2) {
						t.Fatalf("worker failed attrs = %v, want reason restart_limit, limit 2", rec.Attrs)
					}
				}
			}
			if want := []any{3 * time.Second, 6 * time.Second, nil}; !slices.Equal(got, want) {
				t.Fatalf("next_restart_in = %v, want %v", got, want)
			}
			want := []string{"worker run failed", "worker run failed", "worker run failed", "worker failed"}
			if !slices.Equal(messages, want) {
				t.Fatalf("lines = %q, want %q", messages, want)
			}
		})
	})
	t.Run("a failure during shutdown plans nothing", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			logs := newLogCapture()
			s := newTestSupervisorWithLogger(logs.logger())
			c := addWorker(s, continuousDef("consumer"), Func(func(ctx context.Context) error {
				<-ctx.Done()
				return errors.New("flush failed")
			}))
			startWorker(t, c)
			synctest.Wait()
			stopWorker(t, c)

			lines := logs.withMessage("worker run failed")
			if len(lines) != 1 {
				t.Fatalf("%d failure lines, want 1", len(lines))
			}
			if v, ok := lines[0].Attrs["next_restart_in"]; ok {
				t.Fatalf("next_restart_in = %v on a failure during shutdown, want absent", v)
			}
		})
	})
	t.Run("restarts disabled plans nothing", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			logs := newLogCapture()
			s := newTestSupervisorWithLogger(logs.logger())
			backoffRunStarts(t, s, time.Minute, backoffFail, ContinuousConfig{Restart: Restart{Disabled: true}})
			lines := logs.withMessage("worker run failed")
			if len(lines) != 1 {
				t.Fatalf("%d failure lines, want 1", len(lines))
			}
			if v, ok := lines[0].Attrs["next_restart_in"]; ok {
				t.Fatalf("next_restart_in = %v with restarts disabled, want absent", v)
			}
		})
	})
}
