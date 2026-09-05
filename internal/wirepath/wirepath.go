// Package wirepath defines the canonical form in which the router compares a
// request path with registered patterns.
//
// net/http hands the router the path as the client spelled it
// ([net/url.URL.EscapedPath]): ASCII, with every byte the client escaped
// still percent-encoded. Two spellings of one path — "/caf%C3%A9",
// "/caf%c3%a9", "/%63af%C3%A9" — must reach the same route, while an encoded
// slash must stay data inside its segment. The canonical form therefore
// decodes every escape except "%2F" (a slash that is not a segment boundary)
// and "%25" (a percent sign that must not be decoded twice), which it keeps
// in upper-case hexadecimal. Registered static text is brought to the same
// form by escaping its literal percent signs; a decoded capture is then
// [net/url.PathUnescape] of the canonical candidate.
package wirepath

import "strings"

const upperhex = "0123456789ABCDEF"

// Canonical returns the canonical form of an escaped wire path: every "%XX"
// escape is replaced by the byte it encodes, except an encoded slash and an
// encoded percent sign, which are normalized to "%2F" and "%25". A path
// without escapes is returned as is, without allocating. Malformed escapes
// (which net/http never forwards) are copied verbatim.
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
		switch v := byte(hi<<4 | lo); v {
		case '/':
			b.WriteString("%2F")
		case '%':
			b.WriteString("%25")
		default:
			b.WriteByte(v)
		}
		i += 3
	}
	return b.String()
}

// Static returns the canonical form of literal pattern text: the text as the
// developer wrote it, with percent signs escaped so that a client's "%25"
// meets the same bytes. Slashes are pattern separators and stay.
func Static(text string) string {
	return strings.ReplaceAll(text, "%", "%25")
}

// Escape returns the wire (escaped) spelling of a canonical path: every byte
// that may not appear literally in a request path is percent-encoded, while
// the "%2F" and "%25" escapes pass through. The result is what
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
// stays because a canonical path only carries it as "%2F" or "%25".
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
