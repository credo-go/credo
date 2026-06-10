package radix

import "testing"

func TestPatNextSegment(t *testing.T) {
	tests := []struct {
		name     string
		pattern  string
		wantType NodeTyp
		wantKey  string
		wantPfx  string
		wantSufx string
		wantErr  bool
	}{
		{
			name:     "static only",
			pattern:  "/users",
			wantType: NtStatic,
			wantPfx:  "/users",
		},
		{
			name:     "static only trailing slash",
			pattern:  "/users/",
			wantType: NtStatic,
			wantPfx:  "/users/",
		},
		{
			name:     "named parameter",
			pattern:  "{id}",
			wantType: NtParam,
			wantKey:  "id",
			wantPfx:  "",
		},
		{
			name:     "named parameter with prefix",
			pattern:  "/users/{id}",
			wantType: NtParam,
			wantKey:  "id",
			wantPfx:  "/users/",
		},
		{
			name:     "named parameter with suffix",
			pattern:  "{id}/posts",
			wantType: NtParam,
			wantKey:  "id",
			wantPfx:  "",
			wantSufx: "/posts",
		},
		{
			name:     "regex parameter",
			pattern:  "{id:[0-9]+}",
			wantType: NtRegexp,
			wantKey:  "id",
			wantPfx:  "",
		},
		{
			name:     "regex parameter with prefix and suffix",
			pattern:  "/users/{id:[0-9]+}/edit",
			wantType: NtRegexp,
			wantKey:  "id",
			wantPfx:  "/users/",
			wantSufx: "/edit",
		},
		{
			name:     "catch-all parameter",
			pattern:  "{path...}",
			wantType: NtCatchAll,
			wantKey:  "path",
			wantPfx:  "",
		},
		{
			name:     "catch-all with prefix",
			pattern:  "/files/{path...}",
			wantType: NtCatchAll,
			wantKey:  "path",
			wantPfx:  "/files/",
		},
		{
			name:    "unclosed brace",
			pattern: "/users/{id",
			wantErr: true,
		},
		{
			name:    "empty parameter name",
			pattern: "/users/{}",
			wantErr: true,
		},
		{
			name:    "empty catch-all name",
			pattern: "/files/{...}",
			wantErr: true,
		},
		{
			name:    "empty regex",
			pattern: "/users/{id:}",
			wantErr: true,
		},
		{
			name:    "empty regex param name",
			pattern: "/users/{:[0-9]+}",
			wantErr: true,
		},
		{
			name:    "invalid regex",
			pattern: "/users/{id:[invalid}",
			wantErr: true,
		},
		{
			name:     "empty string",
			pattern:  "",
			wantType: NtStatic,
			wantPfx:  "",
		},
		{
			name:     "brace at end",
			pattern:  "/test{",
			wantType: NtStatic,
			wantPfx:  "/test{",
		},
		{
			name:     "regex with quantifier braces",
			pattern:  "{zip:[0-9]{5}}",
			wantType: NtRegexp,
			wantKey:  "zip",
			wantPfx:  "",
		},
		{
			name:     "regex with quantifier range braces",
			pattern:  "{code:[A-Z]{2,4}}/info",
			wantType: NtRegexp,
			wantKey:  "code",
			wantPfx:  "",
			wantSufx: "/info",
		},
		{
			name:     "regex with nested quantifier and prefix",
			pattern:  "/zip/{zip:[0-9]{5}}-{ext:[0-9]{4}}",
			wantType: NtRegexp,
			wantKey:  "zip",
			wantPfx:  "/zip/",
			wantSufx: "-{ext:[0-9]{4}}",
		},
		{
			name:     "regex with escaped literal braces",
			pattern:  "{token:\\{[0-9]+\\}}/raw",
			wantType: NtRegexp,
			wantKey:  "token",
			wantPfx:  "",
			wantSufx: "/raw",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			seg, err := patNextSegment(tt.pattern)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error but got nil")
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if seg.Typ != tt.wantType {
				t.Errorf("Typ = %d, want %d", seg.Typ, tt.wantType)
			}

			if seg.ParamKey != tt.wantKey {
				t.Errorf("ParamKey = %q, want %q", seg.ParamKey, tt.wantKey)
			}

			if seg.Prefix != tt.wantPfx {
				t.Errorf("Prefix = %q, want %q", seg.Prefix, tt.wantPfx)
			}

			if seg.Suffix != tt.wantSufx {
				t.Errorf("Suffix = %q, want %q", seg.Suffix, tt.wantSufx)
			}

			// Verify regex is compiled for NtRegexp
			if tt.wantType == NtRegexp && seg.Regexp == nil {
				t.Error("expected Regexp to be compiled, got nil")
			}
		})
	}
}
