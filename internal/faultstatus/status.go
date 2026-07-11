// Package faultstatus contains Credo's default HTTP transport policy for
// transport-neutral semantic faults.
package faultstatus

import (
	"net/http"

	"github.com/credo-go/credo/fault"
)

// HTTP maps a semantic fault kind to Credo's default HTTP status.
// Unknown kinds are deliberately left unmapped so callers can fail closed.
func HTTP(kind fault.Kind) (int, bool) {
	switch kind {
	case fault.KindNotFound:
		return http.StatusNotFound, true
	case fault.KindAlreadyExists,
		fault.KindConstraint,
		fault.KindSerialization,
		fault.KindDeadlock,
		fault.KindContention,
		fault.KindConflict:
		return http.StatusConflict, true
	case fault.KindTimeout:
		return http.StatusGatewayTimeout, true
	case fault.KindUnavailable, fault.KindReadOnly:
		return http.StatusServiceUnavailable, true
	default:
		return 0, false
	}
}
