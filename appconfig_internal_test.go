package credo

import (
	"testing"
	"time"
)

// TestApplyServerDefaults verifies the Slowloris-mitigating ReadHeaderTimeout
// default is applied only when the value is unset.
func TestApplyServerDefaults(t *testing.T) {
	t.Run("zero gets default", func(t *testing.T) {
		c := serverConfig{}
		applyServerDefaults(&c)
		if c.ReadHeaderTimeout != defaultReadHeaderTimeout {
			t.Errorf("ReadHeaderTimeout = %v, want %v", c.ReadHeaderTimeout, defaultReadHeaderTimeout)
		}
		if c.MaxBodyBytes != defaultMaxBodyBytes {
			t.Errorf("MaxBodyBytes = %d, want %d", c.MaxBodyBytes, defaultMaxBodyBytes)
		}
	})

	t.Run("negative MaxBodyBytes preserved (limit disabled)", func(t *testing.T) {
		c := serverConfig{MaxBodyBytes: -1}
		applyServerDefaults(&c)
		if c.MaxBodyBytes != -1 {
			t.Errorf("MaxBodyBytes = %d, want -1 preserved", c.MaxBodyBytes)
		}
	})

	t.Run("explicit value preserved", func(t *testing.T) {
		c := serverConfig{ReadHeaderTimeout: 3 * time.Second}
		applyServerDefaults(&c)
		if c.ReadHeaderTimeout != 3*time.Second {
			t.Errorf("ReadHeaderTimeout = %v, want 3s preserved", c.ReadHeaderTimeout)
		}
	})
}

// TestNew_AppliesReadHeaderTimeoutDefault verifies the default is wired through
// New() so the built server is never left without a header read timeout.
func TestNew_AppliesReadHeaderTimeoutDefault(t *testing.T) {
	app, err := New()
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	if app.serverCfg.ReadHeaderTimeout != defaultReadHeaderTimeout {
		t.Errorf("serverCfg.ReadHeaderTimeout = %v, want %v",
			app.serverCfg.ReadHeaderTimeout, defaultReadHeaderTimeout)
	}
}
