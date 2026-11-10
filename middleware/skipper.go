package middleware

import "github.com/credo-go/credo"

// Skipper decides whether middleware should be skipped for the current
// request. A config's Skipper left nil skips no request.
type Skipper func(ctx *credo.Context) bool

// neverSkip is the Skipper a config's nil Skipper selects.
func neverSkip(*credo.Context) bool {
	return false
}
