package httpwriter

import "net/http"

// CanFlush reports whether flushing w reaches a writer that can flush. It
// resolves the chain as [http.ResponseController.Flush] does — the first
// writer with a FlushError or Flush method is the flusher, otherwise Unwrap
// is followed — without flushing anything. A FlushError method may still
// report [http.ErrNotSupported] when it is called. A nil writer, a nil
// Unwrap, a cycle and a chain deeper than the Unwrap bound report false.
func CanFlush(w http.ResponseWriter) bool {
	current := w
	var seen map[http.ResponseWriter]struct{}
	for range maxUnwrapDepth {
		switch t := current.(type) {
		case interface{ FlushError() error }, http.Flusher:
			return true
		case unwrapper:
			next := t.Unwrap()
			if next == nil {
				return false
			}
			if isComparable(next) {
				if seen == nil {
					seen = make(map[http.ResponseWriter]struct{})
					trackComparable(seen, current)
				}
				if _, exists := seen[next]; exists {
					return false
				}
				seen[next] = struct{}{}
			}
			current = next
		default:
			return false
		}
	}
	return false
}
