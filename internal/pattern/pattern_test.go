package pattern

import (
	"fmt"
	"slices"
	"testing"
)

func TestNextSegment(t *testing.T) {
	tests := []struct {
		name     string
		pattern  string
		wantKind Kind
		wantName string
		wantPfx  string
		wantSufx string
		wantErr  bool
	}{
		{name: "static only", pattern: "/users", wantKind: Static, wantPfx: "/users"},
		{name: "static only trailing slash", pattern: "/users/", wantKind: Static, wantPfx: "/users/"},
		{name: "named parameter", pattern: "{id}", wantKind: Param, wantName: "id"},
		{name: "named parameter with prefix", pattern: "/users/{id}", wantKind: Param, wantName: "id", wantPfx: "/users/"},
		{name: "named parameter with suffix", pattern: "{id}/posts", wantKind: Param, wantName: "id", wantSufx: "/posts"},
		{name: "regex parameter", pattern: "{id:[0-9]+}", wantKind: Regexp, wantName: "id"},
		{name: "regex parameter with prefix and suffix", pattern: "/users/{id:[0-9]+}/edit", wantKind: Regexp, wantName: "id", wantPfx: "/users/", wantSufx: "/edit"},
		{name: "catch-all parameter", pattern: "{path...}", wantKind: CatchAll, wantName: "path"},
		{name: "catch-all with prefix", pattern: "/files/{path...}", wantKind: CatchAll, wantName: "path", wantPfx: "/files/"},
		{name: "unclosed brace", pattern: "/users/{id", wantErr: true},
		{name: "empty parameter name", pattern: "/users/{}", wantErr: true},
		{name: "empty catch-all name", pattern: "/files/{...}", wantErr: true},
		{name: "empty regex", pattern: "/users/{id:}", wantErr: true},
		{name: "empty regex param name", pattern: "/users/{:[0-9]+}", wantErr: true},
		{name: "invalid regex", pattern: "/users/{id:[invalid}", wantErr: true},
		{name: "empty string", pattern: "", wantKind: Static},
		{name: "brace at end", pattern: "/test{", wantKind: Static, wantPfx: "/test{"},
		{name: "regex with quantifier braces", pattern: "{zip:[0-9]{5}}", wantKind: Regexp, wantName: "zip"},
		{name: "regex with quantifier range braces", pattern: "{code:[A-Z]{2,4}}/info", wantKind: Regexp, wantName: "code", wantSufx: "/info"},
		{name: "regex with nested quantifier and prefix", pattern: "/zip/{zip:[0-9]{5}}-{ext:[0-9]{4}}", wantKind: Regexp, wantName: "zip", wantPfx: "/zip/", wantSufx: "-{ext:[0-9]{4}}"},
		{name: "regex with escaped literal braces", pattern: "{token:\\{[0-9]+\\}}/raw", wantKind: Regexp, wantName: "token", wantSufx: "/raw"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			seg, err := NextSegment(tt.pattern)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error but got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if seg.Kind != tt.wantKind {
				t.Errorf("Kind = %d, want %d", seg.Kind, tt.wantKind)
			}
			if seg.Name != tt.wantName {
				t.Errorf("Name = %q, want %q", seg.Name, tt.wantName)
			}
			if seg.Prefix != tt.wantPfx {
				t.Errorf("Prefix = %q, want %q", seg.Prefix, tt.wantPfx)
			}
			if seg.Suffix != tt.wantSufx {
				t.Errorf("Suffix = %q, want %q", seg.Suffix, tt.wantSufx)
			}
			if tt.wantKind == Regexp && (seg.Regexp == nil || seg.RegexpSource == "") {
				t.Error("expected Regexp and RegexpSource to be set")
			}
			wantTail := byte(0)
			if tt.wantSufx != "" && tt.wantKind != CatchAll {
				wantTail = tt.wantSufx[0]
			}
			if seg.TailByte != wantTail {
				t.Errorf("TailByte = %q, want %q", seg.TailByte, wantTail)
			}
		})
	}
}

func TestFindMatchingBrace(t *testing.T) {
	tests := []struct {
		pattern string
		open    int
		want    int
	}{
		{"{id}", 0, 3},
		{"/a/{id:[0-9]{2,4}}/b", 3, 17},
		{"{t:\\{x\\}}", 0, 8},
		{"{c:[}]+}", 0, 7},
		{"{open", 0, -1},
	}
	for _, tt := range tests {
		if got := FindMatchingBrace(tt.pattern, tt.open); got != tt.want {
			t.Errorf("FindMatchingBrace(%q, %d) = %d, want %d", tt.pattern, tt.open, got, tt.want)
		}
	}
}

func TestParamName(t *testing.T) {
	for in, want := range map[string]string{"id": "id", "id:[0-9]+": "id", "path...": "path", "x:a:b": "x"} {
		if got := ParamName(in); got != want {
			t.Errorf("ParamName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParamNames(t *testing.T) {
	tests := []struct {
		pattern string
		want    []string
	}{
		{"/static", nil},
		{"/users/{id}", []string{"id"}},
		{"/users/{id:[0-9]+}/posts/{slug}/{rest...}", []string{"id", "slug", "rest"}},
		{"/zip/{zip:[0-9]{5}}-{ext:[0-9]{4}}", []string{"zip", "ext"}},
		{"/users/{id", nil},
		{"/a/{x}/{", []string{"x"}},
	}
	for _, tt := range tests {
		if got := ParamNames(tt.pattern); !slices.Equal(got, tt.want) {
			t.Errorf("ParamNames(%q) = %v, want %v", tt.pattern, got, tt.want)
		}
	}
}

func TestCheckNames(t *testing.T) {
	tests := []struct {
		pattern string
		want    string // the repeated name; "" for no error
	}{
		{"/static", ""},
		{"/a/{id}/b/{name}", ""},
		{"/a/{id}/b/{id}", "id"},
		{"/a/{id}/{id...}", "id"},
		{"/a/{id:[0-9]+}/{id}", "id"},
		{"/{a}/{b}/{b}/{a}", "b"},
		// A malformed parameter ends the scan; NextSegment reports it.
		{"/a/{id}/b/{id", ""},
	}
	for _, tt := range tests {
		err := CheckNames(tt.pattern)
		if tt.want == "" {
			if err != nil {
				t.Errorf("CheckNames(%q) = %v, want nil", tt.pattern, err)
			}
			continue
		}
		want := fmt.Sprintf("pattern: duplicate parameter name %q in %q", tt.want, tt.pattern)
		if err == nil || err.Error() != want {
			t.Errorf("CheckNames(%q) = %v, want %q", tt.pattern, err, want)
		}
	}
}

func TestCheckCatchAll(t *testing.T) {
	tests := []struct {
		pattern string
		want    string // "" for no error
	}{
		{"/static", ""},
		{"/a/{p...}", ""},
		{"/a/{id}/{p...}", ""},
		{"/{p...}", ""},
		{"/a/{p...}/x", `pattern: text "/x" after the catch-all {p...} in "/a/{p...}/x": a catch-all takes the rest of the path, so end the pattern with it`},
		{"/a/{p...}/", `pattern: text "/" after the catch-all {p...} in "/a/{p...}/": a catch-all takes the rest of the path, so end the pattern with it`},
		{"/a/{p...}/{id}", `pattern: text "/{id}" after the catch-all {p...} in "/a/{p...}/{id}": a catch-all takes the rest of the path, so end the pattern with it`},
		{"/a/{p...}/{q...}", `pattern: text "/{q...}" after the catch-all {p...} in "/a/{p...}/{q...}": a catch-all takes the rest of the path, so end the pattern with it`},
		// A malformed parameter ends the scan; NextSegment reports it.
		{"/a/{id/{p...}/x", ""},
	}
	for _, tt := range tests {
		err := CheckCatchAll(tt.pattern)
		if tt.want == "" {
			if err != nil {
				t.Errorf("CheckCatchAll(%q) = %v, want nil", tt.pattern, err)
			}
			continue
		}
		if err == nil || err.Error() != tt.want {
			t.Errorf("CheckCatchAll(%q) = %v, want %q", tt.pattern, err, tt.want)
		}
	}
}

// TestMatcher pins that a Matcher matches a canonical wire path as the
// router matches a route: literal text in canonical form, a parameter that
// ends at its tail byte or the next slash with every escape one unit, a
// constraint on the decoded value, and a catch-all over the rest.
func TestMatcher(t *testing.T) {
	tests := []struct {
		pattern string
		input   string
		want    []string // nil = no match
		names   []string
	}{
		{pattern: "/old/{id}", input: "/old/42", want: []string{"42"}, names: []string{"id"}},
		{pattern: "/old/{id}", input: "/old/42/x", names: []string{"id"}},
		{pattern: "/old/{id}", input: "/old/", names: []string{"id"}},
		{pattern: "/files/{path...}", input: "/files/a/b/c", want: []string{"a/b/c"}, names: []string{"path"}},
		{pattern: "/files/{path...}", input: "/files/", want: []string{""}, names: []string{"path"}},
		{pattern: "/v{ver:[0-9]+}/{rest...}", input: "/v2/users", want: []string{"2", "users"}, names: []string{"ver", "rest"}},
		{pattern: "/a.b/{x}", input: "/aXb/1", names: []string{"x"}},
		{pattern: "/exact", input: "/exact", want: []string{}, names: nil},
		{pattern: "/exact", input: "/exact/", names: nil},
		// An escaped slash is data inside a segment.
		{pattern: "/old/{id}", input: "/old/a%2Fb", want: []string{"a%2Fb"}, names: []string{"id"}},
		{pattern: "/old/{a}/{b}", input: "/old/a%2Fb", names: []string{"a", "b"}},
		{pattern: "/files/{path...}", input: "/files/a/b%2Fc", want: []string{"a/b%2Fc"}, names: []string{"path"}},
		// A parameter ends at its tail byte, as in the router.
		{pattern: "/{a}-{b}", input: "/x-y-z", want: []string{"x", "y-z"}, names: []string{"a", "b"}},
		{pattern: "/{name}.json", input: "/a/b.json", names: []string{"name"}},
		// An escape is one unit: "%2F" is not a "F" tail, "%3B" not a ";" tail.
		{pattern: "/x/{a}F", input: "/x/b%2F", names: []string{"a"}},
		{pattern: "/x/{a}F", input: "/x/b%2FF", want: []string{"b%2F"}, names: []string{"a"}},
		{pattern: "/t/{id};v", input: "/t/a%3Bb;v", want: []string{"a%3Bb"}, names: []string{"id"}},
		// A literal "%" is its "%25" unit, in text and as a tail.
		{pattern: "/100%/{x}", input: "/100%25/1", want: []string{"1"}, names: []string{"x"}},
		{pattern: "/{id}%done", input: "/a%2Fb%25done", want: []string{"a%2Fb"}, names: []string{"id"}},
		// A regex parameter is one segment, its constraint checked on the
		// whole decoded value.
		{pattern: "/p/{v:a;b}", input: "/p/a%3Bb", want: []string{"a%3Bb"}, names: []string{"v"}},
		{pattern: "/u/{id:[0-9]+}", input: "/u/12x", names: []string{"id"}},
		{pattern: "/p/{rest:.+}", input: "/p/a/b", names: []string{"rest"}},
		{pattern: "/p/{rest:.+}", input: "/p/a%2Fb", want: []string{"a%2Fb"}, names: []string{"rest"}},
		{pattern: "/p/{v:a/b}", input: "/p/a%2Fb", want: []string{"a%2Fb"}, names: []string{"v"}},
		// A capture that decodes to invalid UTF-8 is no match.
		{pattern: "/old/{id}", input: "/old/%FF", names: []string{"id"}},
		{pattern: "/files/{path...}", input: "/files/a/%FF", names: []string{"path"}},
	}
	for _, tt := range tests {
		t.Run(tt.pattern+" "+tt.input, func(t *testing.T) {
			m, err := NewMatcher(tt.pattern)
			if err != nil {
				t.Fatalf("NewMatcher: %v", err)
			}
			if !slices.Equal(m.Names(), tt.names) {
				t.Errorf("names = %v, want %v", m.Names(), tt.names)
			}
			got, ok := m.Match(tt.input)
			if ok != (tt.want != nil) || !slices.Equal(got, tt.want) {
				t.Errorf("Match(%q) = %q, %v; want %q", tt.input, got, ok, tt.want)
			}
		})
	}
}

func TestNewMatcher_Errors(t *testing.T) {
	tests := []struct{ pattern, want string }{
		{"/bad/{id", `pattern: unclosed parameter brace in "/bad/{id"`},
		{"/bad/{id:[}", `pattern: unclosed parameter brace in "/bad/{id:[}"`},
		{"/bad/{id}/x/{id}", `pattern: duplicate parameter name "id" in "/bad/{id}/x/{id}"`},
		{"/a/{p...}/x", `pattern: text "/x" after the catch-all {p...} in "/a/{p...}/x": a catch-all takes the rest of the path, so end the pattern with it`},
	}
	for _, tt := range tests {
		_, err := NewMatcher(tt.pattern)
		if err == nil || err.Error() != tt.want {
			t.Errorf("NewMatcher(%q) error = %v, want %q", tt.pattern, err, tt.want)
		}
	}
}
