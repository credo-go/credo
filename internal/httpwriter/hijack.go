package httpwriter

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"net/http"
	"reflect"
)

const maxUnwrapDepth = 64

// Hijacker resolution errors are internal capability diagnostics. Callers can
// use errors.Is to distinguish an unsupported writer from a malformed chain.
var (
	ErrHijackerUnavailable = errors.New("httpwriter: http.Hijacker is unavailable")
	ErrNilWriter           = errors.New("httpwriter: response writer is nil")
	ErrNilUnwrap           = errors.New("httpwriter: Unwrap returned nil")
	ErrUnwrapCycle         = errors.New("httpwriter: Unwrap cycle detected")
	ErrUnwrapDepth         = errors.New("httpwriter: Unwrap depth exceeded")
)

type unwrapper interface {
	Unwrap() http.ResponseWriter
}

// ResolveHijacker finds the effective http.Hijacker behind w.
//
// Unwrap takes precedence over a forwarding Hijack method on the same dynamic
// writer. This prevents a wrapper from advertising Hijacker when its effective
// underlying writer cannot actually support it.
func ResolveHijacker(w http.ResponseWriter) (http.Hijacker, error) {
	if w == nil {
		return nil, ErrNilWriter
	}

	current := w
	seen := make(map[http.ResponseWriter]struct{})
	trackComparable(seen, current)

	for depth := 0; depth < maxUnwrapDepth; depth++ {
		if u, ok := current.(unwrapper); ok {
			next := u.Unwrap()
			if next == nil {
				return nil, fmt.Errorf("%w: %T", ErrNilUnwrap, current)
			}
			if isComparable(next) {
				if _, exists := seen[next]; exists {
					return nil, fmt.Errorf("%w: %T", ErrUnwrapCycle, next)
				}
				seen[next] = struct{}{}
			}
			current = next
			continue
		}

		if hijacker, ok := current.(http.Hijacker); ok {
			return hijacker, nil
		}
		return nil, fmt.Errorf("%w: %T", ErrHijackerUnavailable, current)
	}

	return nil, fmt.Errorf("%w: maximum %d", ErrUnwrapDepth, maxUnwrapDepth)
}

// Hijack resolves and invokes the effective http.Hijacker behind w.
func Hijack(w http.ResponseWriter) (net.Conn, *bufio.ReadWriter, error) {
	hijacker, err := ResolveHijacker(w)
	if err != nil {
		return nil, nil, err
	}
	return hijacker.Hijack()
}

func trackComparable(seen map[http.ResponseWriter]struct{}, w http.ResponseWriter) {
	if isComparable(w) {
		seen[w] = struct{}{}
	}
}

func isComparable(w http.ResponseWriter) bool {
	t := reflect.TypeOf(w)
	return t != nil && t.Comparable()
}
