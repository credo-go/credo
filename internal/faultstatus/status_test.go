package faultstatus

import (
	"net/http"
	"testing"

	"github.com/credo-go/credo/fault"
)

func TestHTTP(t *testing.T) {
	tests := []struct {
		kind   fault.Kind
		status int
		known  bool
	}{
		{fault.KindNotFound, http.StatusNotFound, true},
		{fault.KindAlreadyExists, http.StatusConflict, true},
		{fault.KindConstraint, http.StatusConflict, true},
		{fault.KindSerialization, http.StatusConflict, true},
		{fault.KindDeadlock, http.StatusConflict, true},
		{fault.KindContention, http.StatusConflict, true},
		{fault.KindConflict, http.StatusConflict, true},
		{fault.KindTimeout, http.StatusGatewayTimeout, true},
		{fault.KindUnavailable, http.StatusServiceUnavailable, true},
		{fault.KindReadOnly, http.StatusServiceUnavailable, true},
		{fault.KindUnknown, 0, false},
		{fault.Kind("future"), 0, false},
	}

	for _, tt := range tests {
		t.Run(string(tt.kind), func(t *testing.T) {
			status, known := HTTP(tt.kind)
			if status != tt.status || known != tt.known {
				t.Fatalf("HTTP(%q) = (%d, %v), want (%d, %v)",
					tt.kind, status, known, tt.status, tt.known)
			}
		})
	}
}
