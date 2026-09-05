package wirepath

import (
	"net/url"
	"testing"
)

func TestCanonical(t *testing.T) {
	tests := []struct{ in, want string }{
		{"/users/42", "/users/42"},
		{"/caf%C3%A9/42", "/café/42"},
		{"/caf%c3%a9/42", "/café/42"},
		{"/%63af%C3%A9", "/café"},
		{"/files/a%2Fb", "/files/a%2Fb"},
		{"/files/a%2fb", "/files/a%2Fb"},
		{"/files/a%252Fb", "/files/a%25" + "2Fb"},
		{"/100%25", "/100%25"},
		{"/a%20b", "/a b"},
		{"/item/%FF", "/item/\xff"},
		{"/%", "/%"},
		{"/%4", "/%4"},
		{"/%zz", "/%zz"},
	}
	for _, tt := range tests {
		if got := Canonical(tt.in); got != tt.want {
			t.Errorf("Canonical(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
	// A path without escapes is returned without allocating.
	if n := testing.AllocsPerRun(100, func() { Canonical("/users/42/posts") }); n != 0 {
		t.Errorf("Canonical without escapes allocates %v times", n)
	}
}

func TestStatic(t *testing.T) {
	if got := Static("/café/"); got != "/café/" {
		t.Errorf("Static = %q", got)
	}
	if got := Static("/100%/"); got != "/100%25/" {
		t.Errorf("Static = %q", got)
	}
}

func TestEscape_RoundTripsThroughNetURL(t *testing.T) {
	tests := []struct{ canonical, want string }{
		{"/users/42", "/users/42"},
		{"/café/42", "/caf%C3%A9/42"},
		{"/files/a%2Fb", "/files/a%2Fb"},
		{"/100%25", "/100%25"},
		{"/a b/{x}", "/a%20b/%7Bx%7D"},
		{"/a!b/$&'()*+,;=:@[]", "/a!b/$&'()*+,;=:@[]"},
		{"/q?x#y", "/q%3Fx%23y"},
	}
	for _, tt := range tests {
		got := Escape(tt.canonical)
		if got != tt.want {
			t.Errorf("Escape(%q) = %q, want %q", tt.canonical, got, tt.want)
			continue
		}
		// net/url must accept the spelling verbatim: EscapedPath reports it
		// again, and the decoded Path is the fully unescaped canonical text.
		decoded, err := url.PathUnescape(got)
		if err != nil {
			t.Errorf("PathUnescape(%q): %v", got, err)
			continue
		}
		u := &url.URL{Path: decoded, RawPath: got}
		if ep := u.EscapedPath(); ep != got {
			t.Errorf("EscapedPath of %q = %q", got, ep)
		}
		if Canonical(got) != tt.canonical {
			t.Errorf("Canonical(Escape(%q)) = %q", tt.canonical, Canonical(got))
		}
	}
	if n := testing.AllocsPerRun(100, func() { Escape("/users/42/a%2Fb") }); n != 0 {
		t.Errorf("Escape without work allocates %v times", n)
	}
}
