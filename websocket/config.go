package websocket

import (
	"fmt"

	coderwebsocket "github.com/coder/websocket"
)

const (
	defaultReadLimit                     int64 = 32 << 10
	defaultNoContextCompressionThreshold       = 512
	defaultContextCompressionThreshold         = 128
)

// Config configures a WebSocket Server. Its zero value is secure: browser
// origins must be same-origin, compression is disabled, subprotocols are
// optional, and each message is limited to 32 KiB.
type Config struct {
	// AllowedOrigins adds exact or one-left-label wildcard browser origins to
	// the same-origin default, for example "https://app.example.com" or
	// "https://*.example.com".
	AllowedOrigins []string
	// Subprotocols lists supported protocols in server preference order.
	Subprotocols []string
	// RequireSubprotocol rejects clients that do not offer a subprotocol.
	RequireSubprotocol bool
	// ReadLimit is the maximum bytes in one message. Zero uses 32 KiB; negative
	// values are invalid. There is no public unlimited setting.
	ReadLimit int64
	// CompressionMode controls per-message deflate negotiation.
	CompressionMode CompressionMode
	// CompressionThreshold is the minimum message size to compress. Zero uses
	// the selected mode's default; it is ignored when compression is disabled.
	CompressionThreshold int
	// InsecureSkipOriginCheck disables browser origin authorization. It is a
	// CSRF footgun and cannot be combined with AllowedOrigins.
	InsecureSkipOriginCheck bool
}

type resolvedConfig struct {
	origins              originPolicy
	subprotocols         []string
	requireSubprotocol   bool
	readLimit            int64
	compressionMode      coderwebsocket.CompressionMode
	compressionThreshold int
}

func resolveConfig(cfg Config) (resolvedConfig, error) {
	if cfg.ReadLimit < 0 {
		return resolvedConfig{}, fmt.Errorf("ReadLimit must not be negative: %d", cfg.ReadLimit)
	}
	readLimit := cfg.ReadLimit
	if readLimit == 0 {
		readLimit = defaultReadLimit
	}

	compressionMode, err := compressionModeToUpstream(cfg.CompressionMode)
	if err != nil {
		return resolvedConfig{}, err
	}
	if cfg.CompressionThreshold < 0 {
		return resolvedConfig{}, fmt.Errorf(
			"CompressionThreshold must not be negative: %d",
			cfg.CompressionThreshold,
		)
	}
	compressionThreshold := cfg.CompressionThreshold
	switch cfg.CompressionMode {
	case CompressionDisabled:
		compressionThreshold = 0
	case CompressionNoContextTakeover:
		if compressionThreshold == 0 {
			compressionThreshold = defaultNoContextCompressionThreshold
		}
	case CompressionContextTakeover:
		if compressionThreshold == 0 {
			compressionThreshold = defaultContextCompressionThreshold
		}
	}

	subprotocols, err := validateSubprotocols(cfg.Subprotocols, cfg.RequireSubprotocol)
	if err != nil {
		return resolvedConfig{}, err
	}
	origins, err := compileOriginPolicy(cfg.AllowedOrigins, cfg.InsecureSkipOriginCheck)
	if err != nil {
		return resolvedConfig{}, err
	}

	return resolvedConfig{
		origins:              origins,
		subprotocols:         subprotocols,
		requireSubprotocol:   cfg.RequireSubprotocol,
		readLimit:            readLimit,
		compressionMode:      compressionMode,
		compressionThreshold: compressionThreshold,
	}, nil
}

func compressionModeToUpstream(mode CompressionMode) (coderwebsocket.CompressionMode, error) {
	switch mode {
	case CompressionDisabled:
		return coderwebsocket.CompressionDisabled, nil
	case CompressionNoContextTakeover:
		return coderwebsocket.CompressionNoContextTakeover, nil
	case CompressionContextTakeover:
		return coderwebsocket.CompressionContextTakeover, nil
	default:
		return 0, fmt.Errorf("unknown CompressionMode %d", mode)
	}
}

func (cfg resolvedConfig) acceptOptions() *coderwebsocket.AcceptOptions {
	return &coderwebsocket.AcceptOptions{
		Subprotocols:         append([]string(nil), cfg.subprotocols...),
		InsecureSkipVerify:   true,
		CompressionMode:      cfg.compressionMode,
		CompressionThreshold: cfg.compressionThreshold,
	}
}
