package websocket

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

func logConnectionOpen(record *connectionRecord) {
	if record.logger == nil {
		return
	}
	record.logger.LogAttrs(
		context.Background(),
		slog.LevelDebug,
		"websocket: connection opened",
		connectionAttrs(record, "open", -1, 0)...,
	)
}

func logConnectionTerminal(record *connectionRecord, handlerErr error, panicInfo *handlerPanic) {
	if record.logger == nil {
		return
	}
	classification := record.classification
	code := record.closeCode
	if code < 0 {
		code = CloseStatus(record.terminalCause)
	}
	level := slog.LevelDebug
	message := "websocket: connection closed"
	if panicInfo != nil {
		classification = "panic"
	}
	switch classification {
	case "peer_policy", "read_limit":
		level = slog.LevelWarn
	case "application_error", "transport_error", "panic":
		level = slog.LevelError
		message = "websocket: connection failed"
	}
	lifetime := time.Since(record.startedAt)
	if lifetime <= 0 {
		lifetime = time.Nanosecond
	}
	attrs := connectionAttrs(record, classification, code, lifetime)
	if panicInfo != nil && classification == "panic" {
		attrs = append(attrs,
			slog.String("panic_type", panicInfo.typeName),
			slog.String("stack", panicInfo.stack),
		)
	} else if handlerErr != nil && (classification == "application_error" || classification == "transport_error") {
		attrs = append(attrs, slog.String("error_type", safeErrorType(handlerErr)))
	}
	record.logger.LogAttrs(context.Background(), level, message, attrs...)
}

func logConnectionFailure(record *connectionRecord, classification string, err error) {
	if record.logger == nil {
		return
	}
	lifetime := time.Since(record.startedAt)
	if lifetime <= 0 {
		lifetime = time.Nanosecond
	}
	attrs := connectionAttrs(record, classification, record.closeCode, lifetime)
	attrs = append(attrs, slog.String("error_type", safeErrorType(err)))
	record.logger.LogAttrs(
		context.Background(), slog.LevelError, "websocket: connection close failed", attrs...,
	)
}

func connectionAttrs(
	record *connectionRecord,
	classification string,
	code StatusCode,
	lifetime time.Duration,
) []slog.Attr {
	attrs := make([]slog.Attr, 0, 5)
	attrs = append(attrs, slog.String("classification", classification))
	if record.route != "" {
		attrs = append(attrs, slog.String("route", record.route))
	}
	if record.subprotocol != "" {
		attrs = append(attrs, slog.String("subprotocol", record.subprotocol))
	}
	if code >= 0 {
		attrs = append(attrs, slog.Int("close_code", int(code)))
	}
	if lifetime > 0 {
		attrs = append(attrs, slog.Duration("lifetime", lifetime))
	}
	return attrs
}

func safeErrorType(err error) string {
	if operationErr, ok := errors.AsType[*operationError](err); ok {
		return fmt.Sprintf("%T", operationErr.cause)
	}
	return fmt.Sprintf("%T", err)
}

func isOperationError(err error) bool {
	_, ok := errors.AsType[*operationError](err)
	return ok
}

func classifyPeerClose(code StatusCode) string {
	switch code {
	case StatusNormalClosure, StatusGoingAway, StatusNoStatusReceived:
		return "peer_normal"
	case StatusMessageTooBig:
		return "read_limit"
	default:
		return "peer_policy"
	}
}
