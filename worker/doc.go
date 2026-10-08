// Package worker provides background task management for Credo applications.
//
// It unifies continuous workers (queue consumers, watchers, processors) and
// scheduled workers (cron-style maintenance jobs) under a single registration
// and lifecycle model. A worker is anything with a Run(ctx) error method; the
// name that identifies it is given at registration.
//
// # Quick Start
//
//	worker.MustRegister(app, "heartbeat", worker.Func(func(ctx context.Context) error {
//		<-ctx.Done()
//		return nil
//	}))
//
//	app.Provide[*Cleanup](NewCleanup)
//	worker.MustRegisterProvided[*Cleanup](app, "cleanup",
//		worker.WithSchedule("@every 5m"),
//		worker.WithStartImmediately(),
//		worker.WithRunTimeout(time.Minute),
//	)
//
// # Contract
//
// A continuous worker's Run must stay active until its context is cancelled;
// returning nil earlier is a failure. A failed continuous worker is restarted
// after its restart delay, which backs off with jitter up to a cap while
// failures repeat ([WithRestartDelay], [WithMaxRestartDelay]). A scheduled
// worker's Run performs one activation: activations
// never overlap, those that come due during a run are skipped, and
// [WithRunTimeout] bounds each run cooperatively. Panics are recovered and
// recorded as failures. The pool is an ingress component of the App: its
// workers start in the start walk, before the App accepts requests, and stop
// with the ingress tier, concurrently with the HTTP drain and before the
// internal components they use. [Pool.Workers] reports each worker's
// effective [Config] and live state.
//
// # Adapted From
//
// Cron expression parsing and next-fire calculation are adapted from
// robfig/cron v3 (MIT). See NOTICES for full attribution.
//
// Maturity: beta
package worker
