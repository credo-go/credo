package credo

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/credo-go/credo/config"
)

// tlsConfigPrefix is the config key prefix owned by the file-based TLS reload
// participant. Changes under it are applied by the participant rather than
// reported as restart-required.
const tlsConfigPrefix = "server.tls"

// fileCertSource serves the key pair loaded from a PEM file pair through
// tls.Config.GetCertificate, backed by an atomic pointer so that a reload can
// swap the pair without touching the running server: new handshakes see the
// new certificate immediately, open connections are unaffected, and a failed
// re-read keeps the previous pair (ADR-020).
//
// The zero value is inactive; preflight activates it by loading the initial
// pair, which keeps the startup failure modes identical to loading into
// tls.Config.Certificates.
type fileCertSource struct {
	cur atomic.Pointer[tls.Certificate]

	// mu guards the paths. Reload is serialized by the App, but the paths are
	// also read by preflight, which may run again after a rollback.
	mu       sync.Mutex
	certFile string
	keyFile  string
}

// active reports whether a pair has been loaded, i.e. the server serves
// file-based TLS.
func (s *fileCertSource) active() bool { return s.cur.Load() != nil }

// getCertificate is the tls.Config.GetCertificate callback: one atomic load
// per handshake.
func (s *fileCertSource) getCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return s.cur.Load(), nil
}

// load reads the pair from certFile/keyFile and, on success, publishes it and
// remembers the paths. On failure nothing changes.
func (s *fileCertSource) load(certFile, keyFile string) error {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return fmt.Errorf("load TLS key pair: %w", err)
	}
	s.mu.Lock()
	s.certFile, s.keyFile = certFile, keyFile
	s.mu.Unlock()
	s.cur.Store(&cert)
	return nil
}

// paths returns the file pair currently served.
func (s *fileCertSource) paths() (certFile, keyFile string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.certFile, s.keyFile
}

// tlsReloadParticipant is the framework reload step for file-based TLS. It
// runs on every reload, not only when server.tls.* changes, because a key pair
// rotated in place is invisible to the config diff. When the source is not
// active (plaintext or WithTLSConfig) it is a no-op and its prefix does not
// count as covered.
func (app *App) tlsReloadParticipant() reloadParticipant {
	src := &app.tlsFiles
	return reloadParticipant{
		prefixes: []string{tlsConfigPrefix},
		active:   src.active,
		run: func(ctx context.Context, changes config.Changes) error {
			if !src.active() {
				return nil
			}
			certFile, keyFile := src.paths()
			// WithTLSFiles pins the paths; the server.tls.* keys follow the new
			// snapshot so a reload can move the pair to a different location.
			if !app.tlsFilesSet && changes.Affects(tlsConfigPrefix) {
				var next serverTLS
				if err := app.rawConfig.Unmarshal(tlsConfigPrefix, &next); err != nil {
					return fmt.Errorf("tls rotation: read %s: %w", tlsConfigPrefix, err)
				}
				if next.CertFile == "" || next.KeyFile == "" {
					return fmt.Errorf("tls rotation: %s requires both cert_file and key_file (cert=%q key=%q); keeping the current pair",
						tlsConfigPrefix, next.CertFile, next.KeyFile)
				}
				certFile, keyFile = next.CertFile, next.KeyFile
			}
			if err := src.load(certFile, keyFile); err != nil {
				return fmt.Errorf("tls rotation: %w (keeping the current pair)", err)
			}
			app.logger.LogAttrs(ctx, slog.LevelInfo, "credo: TLS certificate reloaded",
				slog.String("cert_file", certFile), slog.String("key_file", keyFile))
			return nil
		},
	}
}
