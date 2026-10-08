// Package worker provides background task management for Credo applications.
//
// It unifies continuous workers (queue consumers, watchers, processors) and
// scheduled workers (cron-style maintenance jobs) under one interface. A
// worker is anything with a Run(ctx) error method; the name that identifies
// it is given at registration, and the registration method chooses its kind.
//
// # Quick Start
//
//	workers := worker.Use(app)
//	workers.Continuous("heartbeat", worker.Func(func(ctx context.Context) error {
//		<-ctx.Done()
//		return nil
//	}))
//
//	app.Provide[*Cleanup](NewCleanup)
//	workers.ScheduledProvided[*Cleanup]("cleanup", "@every 5m", worker.ScheduledConfig{
//		RunOnStart: true,
//		RunTimeout: time.Minute,
//	})
//
// # Contract
//
// Each registration adds the worker to the App as a lifecycle component named
// "worker:<name>": the start walk launches it, and the App stops it in its
// tier — a scheduled worker with the HTTP drain (ingress), a continuous
// worker after it and before the components it depends on (internal). A
// provided worker is built from the container in the start walk, after its
// dependencies, so the dependency graph orders it in both directions.
//
// A continuous worker's Run must stay active until its context is cancelled;
// returning nil earlier is a failure. A failed continuous worker is restarted
// after a delay that backs off with jitter while failures repeat
// ([Restart]). A scheduled worker's Run performs one activation: activations
// never overlap, those that come due during a run are skipped, and
// [ScheduledConfig.RunTimeout] bounds each run cooperatively. Panics are
// recovered and recorded as failures. [Supervisor.Snapshot] and
// [Supervisor.Lookup] report each worker's resolved configuration and live
// state; readiness participation is opt-in per condition.
//
// Registration validates names, configurations and cron expressions at the
// call and panics on misuse; [ParseSchedule] returns the error for an
// expression that comes from configuration.
//
// # Adapted From
//
// Cron expression parsing and next-fire calculation are adapted from
// robfig/cron v3 (MIT). See NOTICES for full attribution.
//
// Maturity: beta
package worker
