package credo

import (
	"testing"

	internalhost "github.com/credo-go/credo/internal/host"
)

func TestNormalizeHostPattern(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"lowercase", "API.Example.COM", "api.example.com"},
		{"trailing dot", "example.com.", "example.com"},
		{"both", "Example.COM.", "example.com"},
		{"no change", "api.example.com", "api.example.com"},
		{"param preserved", "{tenant}.myapp.com", "{tenant}.myapp.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeHostPattern(tt.input)
			if got != tt.want {
				t.Errorf("normalizeHostPattern(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestNormalizeHostPattern_PortPanic(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for host pattern with port")
		}
	}()
	normalizeHostPattern("example.com:8080")
}

func TestHostPatternHasPort(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		want    bool
	}{
		{"no port", "example.com", false},
		{"with port", "example.com:8080", true},
		{"regex colon", "{id:[0-9]+}.example.com", false},
		{"port after regex", "{id:[0-9]+}.example.com:443", true},
		{"nested braces", "{id:[a-z]{2,4}}.example.com", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := internalhost.PatternHasPort(tt.pattern); got != tt.want {
				t.Errorf("PatternHasPort(%q) = %v, want %v", tt.pattern, got, tt.want)
			}
		})
	}
}

func TestNormalizeRequestHost(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"simple", "example.com", "example.com"},
		{"port strip", "example.com:8080", "example.com"},
		{"lowercase", "Example.COM", "example.com"},
		{"trailing dot", "example.com.", "example.com"},
		{"FQDN with port", "Example.COM.:443", "example.com"},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeRequestHost(tt.input)
			if got != tt.want {
				t.Errorf("normalizeRequestHost(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseHostPattern(t *testing.T) {
	tests := []struct {
		name      string
		pattern   string
		wantSegs  int
		wantKeys  []string
		wantTypes []hostSegmentType
	}{
		{
			name:     "exact",
			pattern:  "api.example.com",
			wantSegs: 3,
			wantKeys: nil,
			// reversed: com, example, api
			wantTypes: []hostSegmentType{hostSegStatic, hostSegStatic, hostSegStatic},
		},
		{
			name:     "single param",
			pattern:  "{tenant}.myapp.com",
			wantSegs: 3,
			wantKeys: []string{"tenant"},
			// reversed: com, myapp, {tenant}
			wantTypes: []hostSegmentType{hostSegStatic, hostSegStatic, hostSegParam},
		},
		{
			name:      "regex param",
			pattern:   "{org:[a-z]+}.platform.io",
			wantSegs:  3,
			wantKeys:  []string{"org"},
			wantTypes: []hostSegmentType{hostSegStatic, hostSegStatic, hostSegRegexp},
		},
		{
			name:      "regex param with star quantifier",
			pattern:   "{org:[a-z]*}.platform.io",
			wantSegs:  3,
			wantKeys:  []string{"org"},
			wantTypes: []hostSegmentType{hostSegStatic, hostSegStatic, hostSegRegexp},
		},
		{
			name:      "wildcard",
			pattern:   "*.acme.io",
			wantSegs:  3,
			wantKeys:  nil,
			wantTypes: []hostSegmentType{hostSegStatic, hostSegStatic, hostSegWildcard},
		},
		{
			name:     "multi param",
			pattern:  "{sub}.{domain}.com",
			wantSegs: 3,
			wantKeys: []string{"domain", "sub"},
			// reversed: com, {domain}, {sub}
			wantTypes: []hostSegmentType{hostSegStatic, hostSegParam, hostSegParam},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			segs, keys := parseHostPattern(tt.pattern)
			if len(segs) != tt.wantSegs {
				t.Fatalf("segments count = %d, want %d", len(segs), tt.wantSegs)
			}
			if len(keys) != len(tt.wantKeys) {
				t.Fatalf("keys = %v, want %v", keys, tt.wantKeys)
			}
			for i, k := range tt.wantKeys {
				if keys[i] != k {
					t.Errorf("keys[%d] = %q, want %q", i, keys[i], k)
				}
			}
			for i, wt := range tt.wantTypes {
				if segs[i].typ != wt {
					t.Errorf("segments[%d].typ = %d, want %d", i, segs[i].typ, wt)
				}
			}
		})
	}
}

func TestParseHostPattern_InvalidWildcardPanic(t *testing.T) {
	tests := []string{
		"api*.acme.io",
		"*foo.acme.io",
		"foo.*.io",
		"*.*.acme.io",
		"*.{tenant}.acme.io",
		"{tenant}.*.acme.io",
	}
	for _, pattern := range tests {
		t.Run(pattern, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Fatalf("expected panic for invalid wildcard host pattern %q", pattern)
				}
			}()
			parseHostPattern(normalizeHostPattern(pattern))
		})
	}
}

func TestHostPatternSemanticKey(t *testing.T) {
	key := func(pattern string) string {
		segs, _ := parseHostPattern(normalizeHostPattern(pattern))
		return hostPatternSemanticKey(segs)
	}

	if key("*.acme.io") != key("{tenant}.acme.io") {
		t.Fatal("wildcard and plain param should have identical host semantics")
	}
	if key("{a}.acme.io") != key("{b}.acme.io") {
		t.Fatal("plain host param names should not affect host semantics")
	}
	if key("{a:[a-z]+}.acme.io") != key("{b:[a-z]+}.acme.io") {
		t.Fatal("regex host param names should not affect host semantics")
	}
	if key("{org:[a-z]+}.acme.io") == key("{tenant}.acme.io") {
		t.Fatal("regex host param and plain host param should not have identical semantics")
	}
	if key("*.acme.io") == key("*.example.io") {
		t.Fatal("different static labels should affect host semantics")
	}
}

func TestHostPatternHasWildcard(t *testing.T) {
	tests := []struct {
		pattern string
		want    bool
	}{
		{"*.acme.io", true},
		{"*", true},
		{"{tenant}.acme.io", false},
		{"{tenant:[a-z]*}.acme.io", false},
		{"api.acme.io", false},
	}
	for _, tt := range tests {
		t.Run(tt.pattern, func(t *testing.T) {
			if got := hostPatternHasWildcard(tt.pattern); got != tt.want {
				t.Errorf("hostPatternHasWildcard(%q) = %v, want %v", tt.pattern, got, tt.want)
			}
		})
	}
}

func TestHostEntryMatch(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		host    string
		wantOK  bool
		wantKey string
		wantVal string
	}{
		{
			name:    "exact match",
			pattern: "api.example.com",
			host:    "api.example.com",
			wantOK:  true,
		},
		{
			name:    "exact mismatch",
			pattern: "api.example.com",
			host:    "www.example.com",
			wantOK:  false,
		},
		{
			name:    "case insensitive",
			pattern: "api.example.com",
			host:    "API.Example.COM",
			wantOK:  true,
		},
		{
			name:    "param extraction",
			pattern: "{tenant}.myapp.com",
			host:    "acme.myapp.com",
			wantOK:  true,
			wantKey: "tenant",
			wantVal: "acme",
		},
		{
			name:    "regex match",
			pattern: "{org:[a-z]+}.platform.io",
			host:    "alpha.platform.io",
			wantOK:  true,
			wantKey: "org",
			wantVal: "alpha",
		},
		{
			name:    "regex mismatch",
			pattern: "{org:[a-z]+}.platform.io",
			host:    "123.platform.io",
			wantOK:  false,
		},
		{
			name:    "segment count mismatch",
			pattern: "api.example.com",
			host:    "sub.api.example.com",
			wantOK:  false,
		},
		{
			name:    "wildcard match",
			pattern: "*.acme.io",
			host:    "api.acme.io",
			wantOK:  true,
		},
		{
			name:    "wildcard apex mismatch",
			pattern: "*.acme.io",
			host:    "acme.io",
			wantOK:  false,
		},
		{
			name:    "wildcard nested mismatch",
			pattern: "*.acme.io",
			host:    "a.b.acme.io",
			wantOK:  false,
		},
		{
			name:    "single wildcard match",
			pattern: "*",
			host:    "localhost",
			wantOK:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			segs, _ := parseHostPattern(normalizeHostPattern(tt.pattern))
			entry := &hostEntry{segments: segs}

			host := normalizeRequestHost(tt.host)
			labels := reverseLabels(host)

			params, ok := entry.match(labels)
			if ok != tt.wantOK {
				t.Fatalf("match = %v, want %v", ok, tt.wantOK)
			}
			if tt.wantKey != "" && params != nil {
				if params[tt.wantKey] != tt.wantVal {
					t.Errorf("params[%q] = %q, want %q", tt.wantKey, params[tt.wantKey], tt.wantVal)
				}
			}
		})
	}
}

func TestCompareHostEntries(t *testing.T) {
	// static > regex > param/wildcard
	static := makeEntry("api.example.com")
	regex := makeEntry("{org:[a-z]+}.example.com")
	param := makeEntry("{tenant}.example.com")
	wildcard := makeEntry("*.example.com")

	if c := compareHostEntries(static, regex); c >= 0 {
		t.Errorf("static vs regex = %d, want negative (static first)", c)
	}
	if c := compareHostEntries(regex, param); c >= 0 {
		t.Errorf("regex vs param = %d, want negative (regex first)", c)
	}
	if c := compareHostEntries(regex, wildcard); c >= 0 {
		t.Errorf("regex vs wildcard = %d, want negative (regex first)", c)
	}
	if c := compareHostEntries(static, param); c >= 0 {
		t.Errorf("static vs param = %d, want negative (static first)", c)
	}
	if c := compareHostEntries(param, wildcard); c != 0 {
		t.Errorf("param vs wildcard = %d, want equal specificity", c)
	}
}

// --- helpers ---

func reverseLabels(host string) []string {
	labels := splitDot(host)
	for i, j := 0, len(labels)-1; i < j; i, j = i+1, j-1 {
		labels[i], labels[j] = labels[j], labels[i]
	}
	return labels
}

func splitDot(s string) []string {
	if s == "" {
		return nil
	}
	return splitString(s, '.')
}

func splitString(s string, sep byte) []string {
	var parts []string
	for {
		i := indexByte(s, sep)
		if i < 0 {
			parts = append(parts, s)
			break
		}
		parts = append(parts, s[:i])
		s = s[i+1:]
	}
	return parts
}

func indexByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

func makeEntry(pattern string) *hostEntry {
	norm := normalizeHostPattern(pattern)
	segs, keys := parseHostPattern(norm)
	return &hostEntry{pattern: norm, segments: segs, paramKeys: keys, semantic: hostPatternSemanticKey(segs)}
}
