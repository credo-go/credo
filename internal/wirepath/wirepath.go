// Package wirepath defines the canonical form in which the router compares a
// request path with registered patterns.
//
// net/http hands the router the path as the client spelled it
// ([net/url.URL.EscapedPath]): ASCII, with every byte the client escaped
// still percent-encoded. RFC 3986 decides which spellings are one URI: an
// escaped unreserved byte is the literal byte (section 2.3, so "/caf%C3%A9",
// "/caf%c3%a9" and "/%63af%C3%A9" are the same path and "%2D" is "-"), while
// an escaped reserved character is a different URI from the literal one
// (section 2.2, so "%3B" is not ";" and "%2F" is not a segment boundary).
// The canonical form therefore decodes every escape except those of the
// reserved characters and of the percent sign itself, which it keeps in
// upper-case hexadecimal. Registered static text is brought to the same form
// by escaping its literal percent signs; a decoded capture is then
// [net/url.PathUnescape] of the canonical candidate, so a captured "%3B" is
// the value ";".
package wirepath

import "strings"

const upperhex = "0123456789ABCDEF"

// Canonical returns the canonical form of an escaped wire path: every "%XX"
// escape is replaced by the byte it encodes, except the escapes of the RFC
// 3986 reserved characters ("/", "?", "#", "[", "]", ":", "@" and the
// sub-delimiters "!$&'()*+,;=") and of "%", which are normalized to
// upper-case hexadecimal. A path without escapes is returned as is, without
// allocating. Malformed escapes (which net/http never forwards) are copied
// verbatim.
func Canonical(escaped string) string {
	i := strings.IndexByte(escaped, '%')
	if i < 0 {
		return escaped
	}
	var b strings.Builder
	b.Grow(len(escaped))
	b.WriteString(escaped[:i])
	for i < len(escaped) {
		c := escaped[i]
		if c != '%' || i+2 >= len(escaped) {
			b.WriteByte(c)
			i++
			continue
		}
		hi, lo := unhex(escaped[i+1]), unhex(escaped[i+2])
		if hi < 0 || lo < 0 {
			b.WriteByte(c)
			i++
			continue
		}
		if v := byte(hi<<4 | lo); keepEscaped(v) {
			b.WriteByte('%')
			b.WriteByte(upperhex[v>>4])
			b.WriteByte(upperhex[v&15])
		} else {
			b.WriteByte(v)
		}
		i += 3
	}
	return b.String()
}

// keepEscaped reports whether an escape of c keeps its encoded form in the
// canonical path: the RFC 3986 reserved characters, whose encoded and
// literal spellings are different URIs, and the percent sign, which must not
// be decoded twice.
func keepEscaped(c byte) bool {
	switch c {
	case '%', '/', '?', '#', '[', ']', ':', '@',
		'!', '$', '&', '\'', '(', ')', '*', '+', ',', ';', '=':
		return true
	}
	return false
}

// Static returns the canonical form of literal pattern text: the text as the
// developer wrote it, with percent signs escaped so that a client's "%25"
// meets the same bytes. Slashes are pattern separators and stay; every other
// byte, a reserved character included, matches only its literal spelling.
func Static(text string) string {
	return strings.ReplaceAll(text, "%", "%25")
}

// CandidateEnd returns the length of the parameter candidate at the start of
// a canonical path: the text before the first slash or before the first
// occurrence of tail (the pattern byte after the parameter's closing brace),
// whichever comes first; a tail of 0 or '/' means the next slash alone. An
// escape is one unit that is never a boundary: "%2F" is not a slash, "%3B"
// is not the delimiter ";", and a "%" tail (spelled "%25" in a canonical
// pattern) is found only as that unit.
func CandidateEnd(path string, tail byte) int {
	if tail == '/' {
		tail = 0
	}
	if strings.IndexByte(path, '%') < 0 {
		end := strings.IndexByte(path, '/')
		if end < 0 {
			end = len(path)
		}
		if tail != 0 {
			if i := strings.IndexByte(path[:end], tail); i >= 0 {
				end = i
			}
		}
		return end
	}
	for i := 0; i < len(path); i++ {
		c := path[i]
		if c == '/' {
			return i
		}
		if c == '%' && i+2 < len(path) && unhex(path[i+1]) >= 0 && unhex(path[i+2]) >= 0 {
			if tail == '%' && path[i+1] == '2' && path[i+2] == '5' {
				return i
			}
			i += 2
			continue
		}
		if tail != 0 && c == tail {
			return i
		}
	}
	return len(path)
}

// Escape returns the wire (escaped) spelling of a canonical path: every byte
// that may not appear literally in a request path is percent-encoded, while
// the escapes the canonical form keeps pass through. The result is what
// [net/url.URL.EscapedPath] reports for a URL carrying it as RawPath, so a
// canonical path can be handed to net/http again. A path with nothing to
// escape is returned as is.
func Escape(canonical string) string {
	needs := false
	for i := 0; i < len(canonical); i++ {
		if shouldEscape(canonical[i]) {
			needs = true
			break
		}
	}
	if !needs {
		return canonical
	}
	var b strings.Builder
	b.Grow(len(canonical) + 8)
	for i := 0; i < len(canonical); i++ {
		c := canonical[i]
		if shouldEscape(c) {
			b.WriteByte('%')
			b.WriteByte(upperhex[c>>4])
			b.WriteByte(upperhex[c&15])
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// shouldEscape reports whether c may not appear literally in a request path
// as net/url validates one (RFC 3986 pchar plus "/"): letters, digits,
// "-._~", the sub-delimiters "!$&'()*+,;=", ":", "@", "[" and "]" stay; "%"
// stays because a canonical path only carries it inside an escape it keeps.
func shouldEscape(c byte) bool {
	switch {
	case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		return false
	}
	switch c {
	case '-', '_', '.', '~', '/', '%',
		'!', '$', '&', '\'', '(', ')', '*', '+', ',', ';', '=', ':', '@', '[', ']':
		return false
	}
	return true
}

func unhex(c byte) int {
	switch {
	case '0' <= c && c <= '9':
		return int(c - '0')
	case 'a' <= c && c <= 'f':
		return int(c-'a') + 10
	case 'A' <= c && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}
