package worker_test

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/worker"
)

func ExampleUse() {
	app, err := credo.New()
	if err != nil {
		panic(err)
	}
	workers := worker.Use(app)

	// A continuous worker: Run stays active until the application shuts down.
	workers.Continuous("heartbeat", worker.Func(func(ctx context.Context) error {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-ticker.C:
				run, _ := worker.CurrentRun(ctx)
				slog.InfoContext(ctx, "heartbeat", "worker", run.Worker)
			}
		}
	}), worker.ContinuousConfig{Restart: worker.Restart{MinDelay: 5 * time.Second}})

	// A scheduled worker: one run per activation, never overlapping.
	workers.Scheduled("metrics-report", "@every 1m", worker.Func(func(context.Context) error {
		return nil
	}))

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
	run, _ := worker.CurrentRun(ctx)
	s.log.InfoContext(ctx, "sweeping invoices", "run_id", run.ID)
	return nil
}

func ExampleSupervisor_ScheduledProvided() {
	app, err := credo.New()
	if err != nil {
		panic(err)
	}
	workers := worker.Use(app)

	// The worker is built in the start walk, so the provider and the
	// registration may come in either order, both before Finalize.
	app.Provide[*invoiceSweeper](newInvoiceSweeper)
	workers.ScheduledProvided[*invoiceSweeper]("invoice-sweeper", "@every 1m", worker.ScheduledConfig{
		RunOnStart:             true,
		RunTimeout:             15 * time.Second,
		MaxConsecutiveFailures: 3,
	})

	info, ok := workers.Lookup("invoice-sweeper")
	fmt.Println(ok, info.Kind, info.Schedule)
	fmt.Println(info.Scheduled.Tier, info.Scheduled.RunOnStart, info.Scheduled.RunTimeout,
		info.Scheduled.MaxConsecutiveFailures)
	// Output:
	// true scheduled @every 1m
	// ingress true 15s 3
}

func ExampleCurrentRun() {
	app, err := credo.New(credo.WithLogger(slog.New(slog.DiscardHandler)))
	if err != nil {
		panic(err)
	}
	workers := worker.Use(app)

	done := make(chan struct{})
	workers.Scheduled("report", "@every 1h", worker.Func(func(ctx context.Context) error {
		defer close(done)
		run, ok := worker.CurrentRun(ctx)
		fmt.Println("run context:", ok)
		fmt.Println("worker:", run.Worker)
		fmt.Println("run id set:", run.ID != "")
		fmt.Println("run on start, so no activation time:", run.ScheduledAt.IsZero())
		return nil
	}), worker.ScheduledConfig{RunOnStart: true})

	_, ok := worker.CurrentRun(context.Background())
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
