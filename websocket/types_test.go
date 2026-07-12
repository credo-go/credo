package websocket

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	coderwebsocket "github.com/coder/websocket"
)

func TestPublicEnumValuesAndUpstreamMappings(t *testing.T) {
	if MessageText != 1 || MessageBinary != 2 {
		t.Fatalf("message values = (%d, %d), want (1, 2)", MessageText, MessageBinary)
	}
	messageCases := []struct {
		credo    MessageType
		upstream coderwebsocket.MessageType
	}{
		{MessageText, coderwebsocket.MessageText},
		{MessageBinary, coderwebsocket.MessageBinary},
	}
	for _, tc := range messageCases {
		got, err := messageTypeToUpstream(tc.credo)
		if err != nil || got != tc.upstream {
			t.Errorf("messageTypeToUpstream(%d) = (%d, %v), want (%d, nil)",
				tc.credo, got, err, tc.upstream)
		}
		roundTrip, err := messageTypeFromUpstream(tc.upstream)
		if err != nil || roundTrip != tc.credo {
			t.Errorf("messageTypeFromUpstream(%d) = (%d, %v), want (%d, nil)",
				tc.upstream, roundTrip, err, tc.credo)
		}
	}
	if _, err := messageTypeToUpstream(99); err == nil {
		t.Fatal("unknown Credo message type was accepted")
	}
	if _, err := messageTypeFromUpstream(99); err == nil {
		t.Fatal("unknown upstream message type was accepted")
	}

	compressionCases := []struct {
		credo    CompressionMode
		upstream coderwebsocket.CompressionMode
	}{
		{CompressionDisabled, coderwebsocket.CompressionDisabled},
		{CompressionNoContextTakeover, coderwebsocket.CompressionNoContextTakeover},
		{CompressionContextTakeover, coderwebsocket.CompressionContextTakeover},
	}
	for _, tc := range compressionCases {
		got, err := compressionModeToUpstream(tc.credo)
		if err != nil || got != tc.upstream {
			t.Errorf("compressionModeToUpstream(%d) = (%d, %v), want (%d, nil)",
				tc.credo, got, err, tc.upstream)
		}
	}
	if _, err := compressionModeToUpstream(99); err == nil {
		t.Fatal("unknown compression mode was accepted")
	}

	statusCases := []struct {
		credo    StatusCode
		upstream coderwebsocket.StatusCode
	}{
		{StatusNormalClosure, coderwebsocket.StatusNormalClosure},
		{StatusGoingAway, coderwebsocket.StatusGoingAway},
		{StatusProtocolError, coderwebsocket.StatusProtocolError},
		{StatusUnsupportedData, coderwebsocket.StatusUnsupportedData},
		{StatusInvalidFramePayloadData, coderwebsocket.StatusInvalidFramePayloadData},
		{StatusPolicyViolation, coderwebsocket.StatusPolicyViolation},
		{StatusMessageTooBig, coderwebsocket.StatusMessageTooBig},
		{StatusInternalError, coderwebsocket.StatusInternalError},
		{StatusServiceRestart, coderwebsocket.StatusServiceRestart},
		{StatusTryAgainLater, coderwebsocket.StatusTryAgainLater},
		{StatusBadGateway, coderwebsocket.StatusBadGateway},
		{3000, 3000},
		{4999, 4999},
	}
	for _, tc := range statusCases {
		got, err := statusCodeToUpstream(tc.credo)
		if err != nil || got != tc.upstream {
			t.Errorf("statusCodeToUpstream(%d) = (%d, %v), want (%d, nil)",
				tc.credo, got, err, tc.upstream)
		}
	}
	for _, code := range []StatusCode{
		-1, 999, 1004, StatusNoStatusReceived, StatusAbnormalClosure,
		StatusMandatoryExtension, StatusTLSHandshake, 1016, 2999, 5000,
	} {
		if _, err := statusCodeToUpstream(code); err == nil {
			t.Errorf("statusCodeToUpstream(%d) accepted a server-invalid code", code)
		}
	}
}

func TestCloseErrorAndCloseStatus(t *testing.T) {
	value := CloseError{Code: StatusGoingAway, Reason: "deploy"}
	pointer := &CloseError{Code: StatusPolicyViolation, Reason: "policy"}
	if got := value.Error(); !strings.Contains(got, "1001") || !strings.Contains(got, "deploy") {
		t.Errorf("CloseError.Error() = %q, want diagnostic code and reason", got)
	}

	tests := []struct {
		name string
		err  error
		want StatusCode
	}{
		{name: "nil", want: -1},
		{name: "non close", err: errors.New("transport"), want: -1},
		{name: "value", err: value, want: StatusGoingAway},
		{name: "pointer", err: pointer, want: StatusPolicyViolation},
		{name: "wrapped value", err: fmt.Errorf("outer: %w", value), want: StatusGoingAway},
		{
			name: "joined traversal order",
			err:  errors.Join(errors.New("first"), pointer, value),
			want: StatusPolicyViolation,
		},
		{
			name: "upstream type does not leak",
			err: coderwebsocket.CloseError{
				Code: coderwebsocket.StatusNormalClosure,
			},
			want: -1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := CloseStatus(tc.err); got != tc.want {
				t.Errorf("CloseStatus() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestNormalizeErrorHidesUpstreamCloseError(t *testing.T) {
	upstream := fmt.Errorf("read: %w", coderwebsocket.CloseError{
		Code:   coderwebsocket.StatusMessageTooBig,
		Reason: "private upstream text",
	})
	err := normalizeError(upstream)
	closeErr, ok := err.(CloseError)
	if !ok {
		t.Fatalf("normalizeError() type = %T, want CloseError", err)
	}
	if closeErr.Code != StatusMessageTooBig || closeErr.Reason != "private upstream text" {
		t.Errorf("normalizeError() = %+v", closeErr)
	}
	if _, ok := errors.AsType[coderwebsocket.CloseError](err); ok {
		t.Fatal("normalized error exposes upstream CloseError")
	}

	transport := errors.New("transport")
	if got := normalizeError(transport); got != transport {
		t.Fatal("normalizeError changed a non-close error")
	}
	tooBig := fmt.Errorf("read: %w", coderwebsocket.ErrMessageTooBig)
	if got := CloseStatus(normalizeError(tooBig)); got != StatusMessageTooBig {
		t.Errorf("normalized read-limit status = %d, want %d", got, StatusMessageTooBig)
	}
}

func TestConnCloseRejectsInvalidFrameWithoutUsingConnection(t *testing.T) {
	conn := &Conn{}
	tests := []struct {
		name   string
		code   StatusCode
		reason string
	}{
		{name: "reserved code", code: 1004},
		{name: "client only code", code: StatusMandatoryExtension},
		{name: "synthetic code", code: StatusAbnormalClosure},
		{name: "invalid UTF-8", code: StatusNormalClosure, reason: string([]byte{0xff})},
		{name: "reason too long", code: StatusNormalClosure, reason: strings.Repeat("x", 124)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := conn.Close(tc.code, tc.reason); err == nil {
				t.Fatal("Close() accepted invalid frame")
			}
		})
	}
}
