package di_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/credo-go/credo/internal/di"
)

// releaser is an independent Shutdowner whose Shutdown releases a pending
// constructor and waits for it to complete, so the build fails while the
// shutdown pass is inside an attempt — between picking a ready vertex and
// deciding whether anything is still pending.
type releaser struct {
	release  chan struct{}
	resolved <-chan error
}

func (r *releaser) Shutdown(context.Context) error {
	close(r.release)
	<-r.resolved
	return nil
}

func TestShutdown_BuildFailsDuringAnotherShutdown_ReleasesDependencies(t *testing.T) {
	log := newCloseLog()
	c := di.New()
	started, release := make(chan struct{}), make(chan struct{})
	resolved := make(chan error, 1)
	c.MustProvideValue[*nodeDB](&nodeDB{newCloser(log, "db")})
	c.MustProvide[*nodeService](func(*nodeDB) (*nodeService, error) {
		close(started)
		<-release
		return nil, errors.New("init failed")
	})
	c.MustProvideValue[*releaser](&releaser{release: release, resolved: resolved})
	seal(t, c)

	go func() {
		_, err := c.Resolve[*nodeService]()
		resolved <- err
	}()
	<-started

	if err := c.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown: %v (the failed build must release the DB, not leave it blocked)", err)
	}
	if got := log.snapshot(); !slices.Equal(got, []string{"db"}) {
		t.Fatalf("closed = %v, want [db]", got)
	}
}

// gateOpener releases a pending constructor from its Shutdown and returns at
// once, so the build completes concurrently with the shutdown pass — at any
// point between the pass picking the next ready vertex and deciding whether
// anything is still pending.
type gateOpener struct{ release chan struct{} }

func (g *gateOpener) Shutdown(context.Context) error {
	close(g.release)
	return nil
}

func TestShutdown_BuildSucceedsDuringShutdown_IsAttempted(t *testing.T) {
	for range 300 {
		log := newCloseLog()
		c := di.New()
		started, release := make(chan struct{}), make(chan struct{})
		c.MustProvideValue[*nodeDB](&nodeDB{newCloser(log, "db")})
		c.MustProvide[*nodeService](func(*nodeDB) (*nodeService, error) {
			close(started)
			<-release
			return &nodeService{newCloser(log, "service")}, nil
		})
		c.MustProvideValue[*gateOpener](&gateOpener{release: release})
		seal(t, c)

		resolved := make(chan struct{})
		go func() {
			defer close(resolved)
			_, _ = c.Resolve[*nodeService]()
		}()
		<-started

		if err := c.Shutdown(t.Context()); err != nil {
			t.Fatalf("Shutdown: %v (a build completing during the pass must be shut down, not left unattempted)", err)
		}
		<-resolved
		if got := log.snapshot(); !slices.Equal(got, []string{"service", "db"}) {
			t.Fatalf("closed = %v, want [service db]", got)
		}
	}
}
