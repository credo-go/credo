package credo_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/credo-go/credo"
)

func TestResponse_StateAccessors(t *testing.T) {
	resp := credo.NewResponse(httptest.NewRecorder())

	if resp.Committed() || resp.Status() != 0 || resp.Size() != 0 {
		t.Fatalf("fresh response: Status=%d Size=%d Committed=%v, want 0/0/false",
			resp.Status(), resp.Size(), resp.Committed())
	}

	resp.WriteHeader(http.StatusTeapot)
	body := []byte("short and stout")
	n, err := resp.Write(body)
	if err != nil || n != len(body) {
		t.Fatalf("Write = (%d, %v), want (%d, nil)", n, err, len(body))
	}

	if !resp.Committed() {
		t.Error("Committed() = false after WriteHeader, want true")
	}
	if resp.Status() != http.StatusTeapot {
		t.Errorf("Status() = %d, want %d", resp.Status(), http.StatusTeapot)
	}
	if resp.Size() != int64(len(body)) {
		t.Errorf("Size() = %d, want %d", resp.Size(), len(body))
	}

	// A second WriteHeader is a no-op once committed.
	resp.WriteHeader(http.StatusOK)
	if resp.Status() != http.StatusTeapot {
		t.Errorf("Status() after second WriteHeader = %d, want %d", resp.Status(), http.StatusTeapot)
	}

	// Reset returns the tracking state to zero for pool reuse.
	resp.Reset(httptest.NewRecorder())
	if resp.Committed() || resp.Status() != 0 || resp.Size() != 0 {
		t.Errorf("after Reset: Status=%d Size=%d Committed=%v, want 0/0/false",
			resp.Status(), resp.Size(), resp.Committed())
	}
}

func TestResponse_ImplicitCommitOnWrite(t *testing.T) {
	resp := credo.NewResponse(httptest.NewRecorder())

	if _, err := resp.Write([]byte("hi")); err != nil {
		t.Fatalf("Write error: %v", err)
	}
	if !resp.Committed() {
		t.Error("Committed() = false after Write, want true")
	}
	if resp.Status() != http.StatusOK {
		t.Errorf("Status() = %d, want %d (implicit 200 on first Write)", resp.Status(), http.StatusOK)
	}
}
