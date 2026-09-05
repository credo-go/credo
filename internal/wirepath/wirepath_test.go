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
		{"/a%7Bb%7D", "/a{b}"},
		{"/a%2Db%7Ec", "/a-b~c"},
		{"/a%3Bb", "/a%3Bb"},
		{"/a%3bb", "/a%3Bb"},
		{"/%3A%40%21%24%26%27%28%29%2A%2B%2C%3D%3F%23%5B%5D", "/%3A%40%21%24%26%27%28%29%2A%2B%2C%3D%3F%23%5B%5D"},
		{"/%3a%40%2b%3d", "/%3A%40%2B%3D"},
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
		{"/a%3Bb/%3F", "/a%3Bb/%3F"},
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
	// A literal reserved delimiter that cannot be on the wire (pattern text
	// such as "/q?x") is escaped, and the escape then stays: it is not a
	// canonical spelling, so it does not round-trip to the literal.
	if got := Escape("/q?x#y"); got != "/q%3Fx%23y" || Canonical(got) != got {
		t.Errorf("Escape(\"/q?x#y\") = %q, Canonical = %q", got, Canonical(got))
	}
}

func TestCandidateEnd(t *testing.T) {
	tests := []struct {
		path string
		tail byte
		want int
	}{
		{"42/posts", 0, 2},
		{"42", 0, 2},
		{"a%2Fb/c", 0, 5},
		{"a%2Fb/c", '/', 5},
		{"name.json", '.', 4},
		{"a%2Eb.json", '.', 5}, // "%2E" never reaches a canonical path, but is still one unit
		{"a%3Bb;v", ';', 5},
		{"a;b;v", ';', 1},
		{"a%3Bb", ';', 5},
		{"%2F%25done", '%', 3},
		{"7%25done", '%', 1},
		{"a%25b%25done", '%', 1},
		{"a%3Bb%25done", '%', 5},
		{"abc", '%', 3},
		{"a%b", '%', 1}, // a malformed escape is a literal byte
	}
	for _, tt := range tests {
		if got := CandidateEnd(tt.path, tt.tail); got != tt.want {
			t.Errorf("CandidateEnd(%q, %q) = %d, want %d", tt.path, tt.tail, got, tt.want)
		}
	}
	if n := testing.AllocsPerRun(100, func() { CandidateEnd("a%3Bb;v", ';') }); n != 0 {
		t.Errorf("CandidateEnd allocates %v times", n)
	}
}
