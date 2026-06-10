package testutil_test

import (
	"fmt"
	"log/slog"

	"github.com/credo-go/credo/testutil"
)

// ExampleLogBuffer captures structured log records and inspects them. A
// LogBuffer is usually wired into a test App with [testutil.WithLogBuffer], but
// it works with any *slog.Logger. Records are inspected via Entries (here) or
// matched with AssertHas (which needs a *testing.T).
func ExampleLogBuffer() {
	buf := testutil.NewLogBuffer()
	logger := slog.New(buf.Handler())

	logger.Info("user login", "user", "alice", "ok", true)
	logger.Warn("rate limited", "user", "bob")

	for _, e := range buf.Entries() {
		fmt.Printf("%s level=%s user=%s\n", e["msg"], e["level"], e["user"])
	}
	// Output:
	// user login level=INFO user=alice
	// rate limited level=WARN user=bob
}
