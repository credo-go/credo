package di_test

import (
	"testing"

	"github.com/credo-go/credo/internal/di"
)

// seal finalizes c and fails the test when validation fails. Resolve is
// admitted only after Seal, so every resolving test calls it first.
func seal(tb testing.TB, c *di.Container) {
	tb.Helper()
	if err := c.Seal(); err != nil {
		tb.Fatalf("Seal: %v", err)
	}
}
