package worker_test

import (
	"context"
	"log/slog"
	"time"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/worker"
)

func ExampleRegister() {
	app, err := credo.New()
	if err != nil {
		panic(err)
	}

	// A continuous worker: Run stays active until the application shuts down.
	heartbeat := worker.Func(func(ctx context.Context) error {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-ticker.C:
				slog.InfoContext(ctx, "heartbeat", "worker", worker.WorkerName(ctx))
			}
		}
	})
	if err = worker.Register(app, "heartbeat", heartbeat, worker.WithRestartDelay(5*time.Second)); err != nil {
		panic(err)
	}
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
	s.log.InfoContext(ctx, "sweeping invoices", "run_id", worker.RunID(ctx))
	return nil
}

func ExampleRegisterProvided() {
	app, err := credo.New()
	if err != nil {
		panic(err)
	}

	// The worker is resolved when the pool starts, so the provider and the
	// registration may come in either order.
	app.MustProvide[*invoiceSweeper](newInvoiceSweeper)
	if err = worker.RegisterProvided[*invoiceSweeper](app, "invoice-sweeper",
		worker.WithSchedule("@every 1m"),
		worker.WithStartImmediately(),
		worker.WithRunTimeout(15*time.Second),
		worker.WithMaxConsecutiveFailures(3),
	); err != nil {
		panic(err)
	}
}
