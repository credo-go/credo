package credo_test

import (
	"context"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/store"
	"github.com/credo-go/credo/worker"
)

// exampleDB stands in for a database handle such as *sqldb.DB.
type exampleDB struct{}

func (*exampleDB) Ping(context.Context) error     { return nil }
func (*exampleDB) Shutdown(context.Context) error { return nil }
func (*exampleDB) Health(context.Context) store.Health {
	return store.Health{Status: store.StatusUp}
}

type examplePayments struct{}

func (*examplePayments) Ping(context.Context) error { return nil }

type exampleOrders struct {
	db       *exampleDB
	payments *examplePayments
}

func newExampleOrders(db *exampleDB, payments *examplePayments) *exampleOrders {
	return &exampleOrders{db: db, payments: payments}
}

func (o *exampleOrders) List(ctx *credo.Context) error {
	return ctx.Response().Text(http.StatusOK, "3 orders")
}

// exampleRelay is a continuous worker built by the container.
type exampleRelay struct{ db *exampleDB }

func newExampleRelay(db *exampleDB) *exampleRelay { return &exampleRelay{db: db} }

func (*exampleRelay) Run(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

// Example_bootstrapOrder follows the documented bootstrap order, every
// satellite included: configuration, Provide, feature mounts and satellite
// registrations, Finalize, then Resolve and routes, then Run. Registration is
// sequential — every call comes from this goroutine, before the App runs.
func Example_bootstrapOrder() {
	// 1. Configuration.
	app, err := credo.New(
		credo.WithAddr("127.0.0.1", 0),
		credo.WithLogger(slog.New(slog.DiscardHandler)),
	)
	if err != nil {
		log.Fatal(err)
	}

	// 2. Provide. A misused registration panics here, at its line.
	app.Provide[*examplePayments](func() *examplePayments { return &examplePayments{} })
	app.Provide[*exampleOrders](newExampleOrders)
	app.Provide[*exampleRelay](newExampleRelay)

	// 3. Feature mounts and satellite registrations, in any order.
	if err := app.UseI18n(credo.I18nConfig{
		Default:  "en",
		Messages: credo.I18nMessages{"orders.empty": "No orders yet"},
	}); err != nil {
		log.Fatal(err)
	}
	app.UseHealth()
	if err := store.Register[*exampleDB](app, &exampleDB{}, store.WithName("orders-db")); err != nil {
		log.Fatal(err)
	}
	worker.MustRegisterProvided[*exampleRelay](app, "outbox-relay")

	// 4. Finalize reports every missing dependency and cycle at once.
	if err := app.Finalize(); err != nil {
		log.Fatal(err)
	}

	// 5. Resolve, routes and what is built from resolved values.
	orders := app.MustResolve[*exampleOrders]()
	app.GET("/orders", orders.List)
	payments := app.MustResolve[*examplePayments]()
	app.AddReadinessCheck("payments", credo.HealthCheckFunc(payments.Ping))

	// 6. Run. Here a start hook asks the running App for its routes and then
	// stops it, so the example ends.
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	app.OnStart(func(context.Context) error {
		go func() {
			defer stop()
			for _, path := range []string{"/orders", "/ready"} {
				resp, err := http.Get("http://" + app.Addr().String() + path)
				if err != nil {
					fmt.Println(err)
					return
				}
				body, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if path == "/ready" {
					// The readiness body carries check latencies.
					fmt.Println("GET", path, resp.StatusCode)
					continue
				}
				fmt.Println("GET", path, resp.StatusCode, string(body))
			}
		}()
		return nil
	})
	if err := app.RunContext(ctx); err != nil {
		log.Fatal(err)
	}
	fmt.Println(app.State())

	// Output:
	// GET /orders 200 3 orders
	// GET /ready 200
	// stopped
}
