package websocket

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	coderwebsocket "github.com/coder/websocket"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/internal/httpwriter"
)

var protocolErrorHeaders = []string{
	"Allow",
	"Connection",
	"Upgrade",
	"Sec-WebSocket-Version",
}

// Handler adapts h into a Credo route handler. It panics for a nil handler.
// The application handler runs synchronously for the full connection lifetime.
func (s *Server) Handler(h Handler) credo.Handler {
	if h == nil {
		panic("credo/websocket: Server.Handler called with a nil Handler")
	}
	return func(ctx *credo.Context) error {
		return s.handle(ctx, h)
	}
}

func (s *Server) handle(ctx *credo.Context, handler Handler) error {
	if err := validateMechanicalHandshake(ctx); err != nil {
		return err
	}
	if err := s.config.origins.authorize(
		ctx.Request().Header.Values("Origin"),
		ctx.Request().Scheme(),
		ctx.Request().Host,
	); err != nil {
		return credo.NewHTTPError(http.StatusForbidden).WithInternal(err)
	}
	clientProtocols, err := parseClientSubprotocols(
		ctx.Request().Header.Values("Sec-WebSocket-Protocol"),
	)
	if err != nil {
		return credo.NewHTTPError(http.StatusBadRequest).WithInternal(err)
	}
	selectedProtocol, err := selectSubprotocol(
		clientProtocols,
		s.config.subprotocols,
		s.config.requireSubprotocol,
	)
	if err != nil {
		return credo.NewHTTPError(http.StatusBadRequest).WithInternal(err)
	}
	if !s.acquireToken() {
		return credo.NewHTTPError(http.StatusServiceUnavailable)
	}
	tokenAttached := false
	defer func() {
		if !tokenAttached {
			s.releaseToken()
		}
	}()

	if _, err := httpwriter.ResolveHijacker(ctx.Response()); err != nil {
		return credo.NewHTTPError(http.StatusNotImplemented).WithInternal(err)
	}
	connectionID := rand.Text()
	route := ""
	if matched := ctx.Route(); matched != nil {
		route = matched.GetName()
		if route == "" {
			route = matched.GetPattern()
		}
	}
	logger := ctx.Logger().With("connection_id", connectionID)

	request := ctx.Request().Request.Clone(ctx.Context())
	request.Header = ctx.Request().Header.Clone()
	if selectedProtocol == "" {
		request.Header.Del("Sec-WebSocket-Protocol")
	} else {
		request.Header.Set("Sec-WebSocket-Protocol", selectedProtocol)
	}
	opts := s.config.acceptOptions()
	if selectedProtocol == "" {
		opts.Subprotocols = nil
	} else {
		opts.Subprotocols = []string{selectedProtocol}
	}
	capture := newHandshakeWriter(ctx.Response())
	raw, err := coderwebsocket.Accept(capture, request, opts)
	if err != nil {
		if ctx.Response().Committed() {
			attrs := []slog.Attr{
				slog.String("classification", "handshake_transport_error"),
				slog.String("error_type", fmt.Sprintf("%T", err)),
			}
			if route != "" {
				attrs = append(attrs, slog.String("route", route))
			}
			if selectedProtocol != "" {
				attrs = append(attrs, slog.String("subprotocol", selectedProtocol))
			}
			logger.LogAttrs(ctx.Context(), slog.LevelError,
				"websocket: handshake transport failure",
				attrs...,
			)
			return nil
		}
		capture.publishErrorHeaders()
		status := capture.status
		if status == 0 {
			status = http.StatusInternalServerError
		}
		return credo.NewHTTPError(status).WithInternal(err)
	}

	connectionCtx, cancel := context.WithCancelCause(context.WithoutCancel(ctx.Context()))
	conn := newConn(raw, connectionCtx, s.config.readLimit)
	record := &connectionRecord{
		conn: conn, close: conn.Close, closeNow: raw.CloseNow,
		cancel: cancel, logger: logger,
		connectionID: connectionID, requestID: ctx.RequestID(), route: route,
		subprotocol: conn.Subprotocol(), startedAt: time.Now(), closeDone: make(chan struct{}),
	}
	tokenAttached = true
	if !s.attach(record) {
		s.finish(record)
		logConnectionTerminal(record, nil, nil)
		return nil
	}
	logConnectionOpen(record)

	handlerErr, panicInfo := invokeHandler(handler, ctx, conn)
	switch {
	case panicInfo != nil:
		s.startTerminal(
			record, StatusInternalError, "internal error", context.Canceled, false, "panic",
		)
	case handlerErr == nil:
		s.startTerminal(record, StatusNormalClosure, "", context.Canceled, false, "normal")
	case CloseStatus(handlerErr) >= 0:
		s.startTerminal(record, -1, "", handlerErr, false, classifyPeerClose(CloseStatus(handlerErr)))
	case isOperationError(handlerErr):
		s.startTerminal(
			record, StatusInternalError, "internal error", handlerErr, false, "transport_error",
		)
	default:
		s.startTerminal(
			record, StatusInternalError, "internal error", handlerErr, false, "application_error",
		)
	}
	s.finish(record)
	logConnectionTerminal(record, handlerErr, panicInfo)
	return nil
}

type handlerPanic struct {
	typeName string
	stack    string
}

func invokeHandler(
	handler Handler,
	ctx *credo.Context,
	conn *Conn,
) (err error, panicInfo *handlerPanic) {
	defer func() {
		if recovered := recover(); recovered != nil {
			panicInfo = &handlerPanic{
				typeName: fmt.Sprintf("%T", recovered),
				stack:    string(debug.Stack()),
			}
		}
	}()
	return handler(ctx, conn), nil
}

func validateMechanicalHandshake(ctx *credo.Context) error {
	r := ctx.Request().Request
	fail := func(status int, err error) error {
		return credo.NewHTTPError(status).WithInternal(err)
	}
	if !r.ProtoAtLeast(1, 1) {
		ctx.Response().Header().Set("Connection", "Upgrade")
		ctx.Response().Header().Set("Upgrade", "websocket")
		return fail(http.StatusUpgradeRequired, errors.New("WebSocket handshake requires HTTP/1.1"))
	}
	if !headerContainsToken(r.Header, "Connection", "Upgrade") ||
		!headerContainsToken(r.Header, "Upgrade", "websocket") {
		ctx.Response().Header().Set("Connection", "Upgrade")
		ctx.Response().Header().Set("Upgrade", "websocket")
		return fail(http.StatusUpgradeRequired, errors.New("missing WebSocket Upgrade headers"))
	}
	if r.Method != http.MethodGet {
		ctx.Response().Header().Set("Allow", http.MethodGet)
		return fail(http.StatusMethodNotAllowed, fmt.Errorf("WebSocket handshake method is %s", r.Method))
	}
	if r.Header.Get("Sec-WebSocket-Version") != "13" {
		ctx.Response().Header().Set("Sec-WebSocket-Version", "13")
		return fail(http.StatusBadRequest, errors.New("unsupported WebSocket version"))
	}
	keys := r.Header.Values("Sec-WebSocket-Key")
	if len(keys) != 1 {
		return fail(http.StatusBadRequest, errors.New("WebSocket handshake requires one key"))
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(keys[0]))
	if err != nil || len(key) != 16 {
		return fail(http.StatusBadRequest, errors.New("invalid WebSocket handshake key"))
	}
	return nil
}

func headerContainsToken(header http.Header, name, token string) bool {
	for _, value := range header.Values(name) {
		for part := range strings.SplitSeq(value, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

type handshakeWriter struct {
	response *credo.Response
	header   http.Header
	status   int
	body     bytes.Buffer
}

func newHandshakeWriter(response *credo.Response) *handshakeWriter {
	return &handshakeWriter{response: response, header: response.Header().Clone()}
}

func (w *handshakeWriter) Header() http.Header { return w.header }

func (w *handshakeWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *handshakeWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(data)
}

func (w *handshakeWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.publishSuccessHeaders()
	w.response.WriteHeader(http.StatusSwitchingProtocols)
	return w.response.Hijack()
}

func (w *handshakeWriter) publishSuccessHeaders() {
	destination := w.response.Header()
	clear(destination)
	for key, values := range w.header {
		destination[key] = append([]string(nil), values...)
	}
}

func (w *handshakeWriter) publishErrorHeaders() {
	destination := w.response.Header()
	for _, key := range protocolErrorHeaders {
		if values, ok := w.header[http.CanonicalHeaderKey(key)]; ok {
			destination[http.CanonicalHeaderKey(key)] = append([]string(nil), values...)
		}
	}
}
