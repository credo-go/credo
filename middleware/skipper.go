package middleware

import "github.com/credo-go/credo"

// Skipper decides whether middleware should be skipped for the current request.
type Skipper func(ctx *credo.Context) bool

// DefaultSkipper never skips middleware.
func DefaultSkipper(*credo.Context) bool {
	return false
}
