package observe

import (
	"context"
	"log/slog"
	"time"
)

// MetaAccessLogKey is the route-meta key that toggles access logging for a
// route or, through LookupMeta inheritance, a whole group. The root package
// re-exports it as credo.MetaAccessLog; every access-log producer (the
// built-in tier and middleware.AccessLog) decodes it through SilencedByMeta so
// the opt-out semantics cannot drift between them.
const MetaAccessLogKey = "credo.accesslog"

// AccessLogRecord is the transport-neutral snapshot an access-log producer
// captures at its observation boundary. The root package's public
// credo.AccessLogEntry has the identical field list and converts to it
// field-for-field; RouteName is carried for result filters but never emitted,
// while Route (the matched route's registered pattern) is emitted as "route"
// whenever a route matched.
type AccessLogRecord struct {
	Method       string
	Path         string
	OriginalPath string
	Route        string
	Status       int
	Bytes        int64
	Duration     time.Duration
	RemoteAddr   string
	UserAgent    string
	RequestID    string
	RouteName    string
}

// SilencedByMeta decodes a MetaAccessLogKey lookup result. Only a present bool
// false silences the request; a missing key or a non-bool value fails open so
// a mistyped meta value can never hide traffic.
func SilencedByMeta(value any, present bool) bool {
	if !present {
		return false
	}
	enabled, ok := value.(bool)
	return ok && !enabled
}

// BelowMinLevel reports whether the status-derived log level is below the
// producer's configured minimum, in which case the record is dropped before
// any per-request snapshot work.
func BelowMinLevel(status int, minLevel slog.Leveler) bool {
	return Level(status) < minLevel.Level()
}

// EmitAccessLog writes the single "request completed" entry for rec at the
// status-derived level, or returns without building attributes when logger
// is not enabled for that level. requestID is attached explicitly only when the logger
// does not already carry it; callers pass "" otherwise. It is the one source
// for the attribute set, message, and level shared by the built-in access
// logger and middleware.AccessLog (this package cannot import the root
// package, so callers collect the per-request primitives).
func EmitAccessLog(ctx context.Context, logger *slog.Logger, rec AccessLogRecord, requestID string) {
	level := Level(rec.Status)
	// A handler that would discard the record (a Warn-only access logger, say)
	// must not pay for the attribute set; slog itself checks Enabled only
	// after the attrs are built.
	if !logger.Enabled(ctx, level) {
		return
	}
	attrs, n := AccessLogAttrs(rec, requestID)
	logger.LogAttrs(ctx, level, "request completed", attrs[:n]...)
}

// AccessLogAttrs builds the common structured attributes for rec. path_original
// is added only when the served path differs from the client path, route only
// when a route matched (its registered pattern, never a concrete value), and
// request_id only when requestID is non-empty.
func AccessLogAttrs(rec AccessLogRecord, requestID string) ([10]slog.Attr, int) {
	const baseAccessLogAttrCount = 7

	var attrs [10]slog.Attr
	attrs[0] = slog.String("method", rec.Method)
	attrs[1] = slog.String("path", rec.Path)
	attrs[2] = slog.Int("status", rec.Status)
	attrs[3] = slog.Int64("bytes", rec.Bytes)
	attrs[4] = slog.Duration("duration", rec.Duration)
	attrs[5] = slog.String("remote_addr", rec.RemoteAddr)
	attrs[6] = slog.String("user_agent", rec.UserAgent)
	n := baseAccessLogAttrCount
	if rec.OriginalPath != "" && rec.OriginalPath != rec.Path {
		attrs[n] = slog.String("path_original", rec.OriginalPath)
		n++
	}
	if rec.Route != "" {
		attrs[n] = slog.String("route", rec.Route)
		n++
	}
	if requestID != "" {
		attrs[n] = slog.String("request_id", requestID)
		n++
	}
	return attrs, n
}
