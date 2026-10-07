//go:build pending_w6

package credo_test

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/worker"
)

// acceptanceDB is a database component: writes fail once it has shut down.
type acceptanceDB struct {
	log *acceptanceLog

	mu     sync.Mutex
	closed bool
	rows   []string
}

func (db *acceptanceDB) Write(row string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return errors.New("database is closed")
	}
	db.rows = append(db.rows, row)
	return nil
}

func (db *acceptanceDB) Rows() []string {
	db.mu.Lock()
	defer db.mu.Unlock()
	return slices.Clone(db.rows)
}

func (db *acceptanceDB) Shutdown(context.Context) error {
	db.mu.Lock()
	db.closed = true
	db.mu.Unlock()
	db.log.add("database stopped")
	return nil
}

// jobQueue is the application's in-process queue between handlers and the
// worker. It has no teardown of its own.
type jobQueue struct{ jobs chan string }

// jobWriter is a continuous worker that writes every queued job through the
// database. When its context ends it writes what is already queued and
// returns, so it loses no job that was handed to it before it was stopped.
type jobWriter struct {
	queue *jobQueue
	db    *acceptanceDB
	log   *acceptanceLog
}

func (w *jobWriter) Run(ctx context.Context) error {
	for {
		select {
		case job := <-w.queue.jobs:
			if err := w.db.Write(job); err != nil {
				return err
			}
		case <-ctx.Done():
			for {
				select {
				case job := <-w.queue.jobs:
					if err := w.db.Write(job); err != nil {
						return err
					}
				default:
					w.log.add("worker stopped")
					return nil
				}
			}
		}
	}
}

// TestAcceptance_DrainDeliversLastJob is the first acceptance scenario of the
// lifecycle components (ADR-024): the last job an HTTP handler hands to a
// worker before the drain reaches the database. A request accepted just before
// shutdown completes during the HTTP drain; the continuous worker, in the
// internal tier, is stopped only after that drain, so the job is written; and
// the database, which the worker depends on, shuts down after the worker.
func TestAcceptance_DrainDeliversLastJob(t *testing.T) {
	log := &acceptanceLog{}
	db := &acceptanceDB{log: log}
	queue := &jobQueue{jobs: make(chan string, 16)}
	host, port, addr := freePort(t)
	app := mustNew(t, credo.WithAddr(host, port))

	app.ProvideValue(log)
	app.ProvideValue(db)
	app.ProvideValue(queue)
	app.Provide[*jobWriter](func(q *jobQueue, d *acceptanceDB, l *acceptanceLog) *jobWriter {
		return &jobWriter{queue: q, db: d, log: l}
	})
	workers := worker.Use(app)
	workers.ContinuousProvided[*jobWriter]("job-writer")

	entered := make(chan struct{})
	release := make(chan struct{})
	app.POST("/jobs", func(c *credo.Context) error {
		close(entered)
		<-release
		queue.jobs <- "last-job"
		return c.Response().NoContent(http.StatusAccepted)
	})
	if err := app.Finalize(); err != nil {
		t.Fatalf("Finalize: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- app.RunContext(ctx) }()
	waitFor(t, "the App to run", app.IsRunning)

	status := make(chan int, 1)
	go func() {
		resp, err := http.Post("http://"+addr+"/jobs", "text/plain", nil)
		if err != nil {
			t.Errorf("POST /jobs: %v", err)
			status <- 0
			return
		}
		resp.Body.Close()
		status <- resp.StatusCode
	}()

	<-entered
	cancel()
	waitFor(t, "the drain to begin", func() bool { return !app.IsRunning() })
	close(release)

	if got := <-status; got != http.StatusAccepted {
		t.Errorf("POST /jobs status = %d, want %d", got, http.StatusAccepted)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunContext: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("RunContext did not return after the drain")
	}

	if got := db.Rows(); !slices.Equal(got, []string{"last-job"}) {
		t.Errorf("database rows = %q, want the last job written", got)
	}
	assertBefore(t, log.snapshot(), "worker stopped", "database stopped")
}

// waitFor polls cond until it holds, failing the test after five seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
