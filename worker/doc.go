// Package worker provides background task management for Credo applications.
//
// It unifies continuous workers (queue consumers, watchers, processors) and
// scheduled workers (cron-style maintenance jobs) under a single registration
// and lifecycle model.
//
// # Quick Start
//
//	worker.MustRegister(app, worker.Func("heartbeat", func(ctx context.Context) error {
//		<-ctx.Done()
//		return nil
//	}))
//
//	worker.MustRegister(app, cleanup,
//		worker.WithSchedule("@every 5m"),
//		worker.WithStartImmediately(),
//	)
//
// # Adapted From
//
// Cron expression parsing and next-fire calculation are adapted from
// robfig/cron v3 (MIT). See NOTICES for full attribution.
//
// Maturity: experimental
package worker
