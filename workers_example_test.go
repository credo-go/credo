package credo_test

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/credo-go/credo"
)

// heartbeat is a continuous worker: Run stays active until the application
// shuts down.
type heartbeat struct{}

func (heartbeat) Run(ctx context.Context) error {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			run, _ := credo.CurrentWorkerRun(ctx)
			slog.InfoContext(ctx, "heartbeat", "worker", run.Worker)
		}
	}
}

// metricsReport is a scheduled worker: one run per activation, never
// overlapping.
type metricsReport struct{}

func (metricsReport) Run(context.Context) error { return nil }

func ExampleApp_Workers() {
	app, err := credo.New()
	if err != nil {
		panic(err)
	}
	workers := app.Workers()

	workers.Continuous("heartbeat", heartbeat{},
		credo.ContinuousConfig{Restart: credo.RestartPolicy{MinDelay: 5 * time.Second}})
	workers.Scheduled("metrics-report", "@every 1m", metricsReport{})

	for _, info := range workers.Snapshot() {
		var tier credo.Tier
		if info.Continuous != nil {
			tier = info.Continuous.Tier
		} else {
			tier = info.Scheduled.Tier
		}
		fmt.Println(info.Name, info.Kind, tier, info.Status)
	}
	// Output:
	// heartbeat continuous internal pending
	// metrics-report scheduled ingress pending
}

// invoiceSweeper is a scheduled worker built by the DI container.
type invoiceSweeper struct {
	log *slog.Logger
}

func newInvoiceSweeper(infra credo.Infra) *invoiceSweeper {
	return &invoiceSweeper{log: infra.Logger}
}

// Run performs one activation.
func (s *invoiceSweeper) Run(ctx context.Context) error {
	run, _ := credo.CurrentWorkerRun(ctx)
	s.log.InfoContext(ctx, "sweeping invoices", "run_id", run.ID)
	return nil
}

func ExampleWorkers_ScheduledProvided() {
	app, err := credo.New()
	if err != nil {
		panic(err)
	}

	// The worker is built in the start walk, so the provider and the
	// registration may come in either order, both before Finalize.
	app.Provide(newInvoiceSweeper)
	app.Workers().ScheduledProvided[*invoiceSweeper]("invoice-sweeper", "@every 1m", credo.ScheduledConfig{
		RunOnStart:             true,
		RunTimeout:             15 * time.Second,
		MaxConsecutiveFailures: 3,
	})

	info, ok := app.Workers().Lookup("invoice-sweeper")
	fmt.Println(ok, info.Kind, info.Schedule)
	fmt.Println(info.Scheduled.Tier, info.Scheduled.RunOnStart, info.Scheduled.RunTimeout,
		info.Scheduled.MaxConsecutiveFailures)
	// Output:
	// true scheduled @every 1m
	// ingress true 15s 3
}

// reportOnce is a scheduled worker that prints its run metadata and closes
// done.
type reportOnce struct{ done chan struct{} }

func (w reportOnce) Run(ctx context.Context) error {
	defer close(w.done)
	run, ok := credo.CurrentWorkerRun(ctx)
	fmt.Println("run context:", ok)
	fmt.Println("worker:", run.Worker)
	fmt.Println("run id set:", run.ID != "")
	fmt.Println("run on start, so no activation time:", run.ScheduledAt.IsZero())
	return nil
}

func ExampleCurrentWorkerRun() {
	app, err := credo.New(credo.WithLogger(slog.New(slog.DiscardHandler)))
	if err != nil {
		panic(err)
	}

	done := make(chan struct{})
	app.Workers().Scheduled("report", "@every 1h", reportOnce{done: done}, credo.ScheduledConfig{RunOnStart: true})

	_, ok := credo.CurrentWorkerRun(context.Background())
	fmt.Println("outside a run:", ok)

	if err = app.Start(context.Background()); err != nil {
		panic(err)
	}
	<-done
	if err = app.Shutdown(context.Background()); err != nil {
		panic(err)
	}
	// Output:
	// outside a run: false
	// run context: true
	// worker: report
	// run id set: true
	// run on start, so no activation time: true
}
