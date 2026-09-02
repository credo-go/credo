package pattern

import (
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

func TestToRegexp(t *testing.T) {
	tests := []struct {
		pattern   string
		input     string
		wantMatch []string // nil = no match
		wantNames []string
		wantErr   bool
	}{
		{pattern: "/old/{id}", input: "/old/42", wantMatch: []string{"/old/42", "42"}, wantNames: []string{"", "id"}},
		{pattern: "/old/{id}", input: "/old/42/x", wantMatch: nil, wantNames: []string{"", "id"}},
		{pattern: "/files/{path...}", input: "/files/a/b/c", wantMatch: []string{"/files/a/b/c", "a/b/c"}, wantNames: []string{"", "path"}},
		{pattern: "/v{ver:[0-9]+}/{rest...}", input: "/v2/users", wantMatch: []string{"/v2/users", "2", "users"}, wantNames: []string{"", "ver", "rest"}},
		{pattern: "/a.b/{x}", input: "/aXb/1", wantMatch: nil, wantNames: []string{"", "x"}},
		{pattern: "/exact", input: "/exact", wantMatch: []string{"/exact"}, wantNames: []string{""}},
		{pattern: "/bad/{id", wantErr: true},
		{pattern: "/bad/{id:[}", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.pattern, func(t *testing.T) {
			re, names, err := ToRegexp(tt.pattern)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !slices.Equal(names, tt.wantNames) {
				t.Errorf("names = %v, want %v", names, tt.wantNames)
			}
			got := re.FindStringSubmatch(tt.input)
			if !slices.Equal(got, tt.wantMatch) {
				t.Errorf("match(%q) = %v, want %v", tt.input, got, tt.wantMatch)
			}
		})
	}
}
