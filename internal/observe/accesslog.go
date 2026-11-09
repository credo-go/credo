package observe

import (
	"context"
	"log/slog"
	"time"
)

// AccessLogRecord is the transport-neutral set of values an access-log
// producer emits. Route (the matched route's registered pattern) is emitted as
// "route" whenever a route matched; RequestID is emitted only when non-empty,
// so a producer whose logger already carries the request ID leaves it empty.
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
}

// AppAttrsKey is the key of the group that carries the application's
// attributes on the access record. It is reserved: no framework attribute
// uses it, so an application attribute never shadows one.
const AppAttrsKey = "attrs"

// BelowMinLevel reports whether the status-derived log level is below the
// producer's configured minimum, in which case the record is dropped before
// any per-request snapshot work.
func BelowMinLevel(status int, minLevel slog.Leveler) bool {
	return Level(status) < minLevel.Level()
}

// EmitAccessLog writes the single "request completed" entry for rec at level,
// the status-derived level the caller found the logger enabled for, with the
// application's attributes app, when there are any, in the reserved
// [AppAttrsKey] group. It is the one source for the attribute set and the
// message of the access-log feature (this package cannot import the root
// package, so callers collect the per-request primitives).
func EmitAccessLog(ctx context.Context, logger *slog.Logger, level slog.Level, rec AccessLogRecord, app []slog.Attr) {
	attrs, n := AccessLogAttrs(rec)
	if len(app) != 0 {
		attrs[n] = slog.Attr{Key: AppAttrsKey, Value: slog.GroupValue(app...)}
		n++
	}
	logger.LogAttrs(ctx, level, "request completed", attrs[:n]...)
}

// AccessLogAttrs builds the common structured attributes for rec. path_original
// is added only when the served path differs from the client path, route only
// when a route matched (its registered pattern, never a concrete value), and
// request_id only when rec.RequestID is non-empty. The array keeps one slot
// free for the application's group.
func AccessLogAttrs(rec AccessLogRecord) ([11]slog.Attr, int) {
	const baseAccessLogAttrCount = 7

	var attrs [11]slog.Attr
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
	if rec.RequestID != "" {
		attrs[n] = slog.String("request_id", rec.RequestID)
		n++
	}
	return attrs, n
}
