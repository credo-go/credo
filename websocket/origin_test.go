package websocket

import "testing"

func TestOriginPolicyAuthorization(t *testing.T) {
	policy, err := compileOriginPolicy([]string{
		"https://app.example.com",
		"https://*.partner.example",
		"https://[2001:db8::1]:8443",
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name          string
		origins       []string
		requestScheme string
		requestHost   string
		wantErr       bool
	}{
		{name: "missing non-browser", requestScheme: "https", requestHost: "chat.example.com"},
		{
			name:          "same origin normalized",
			origins:       []string{"https://CHAT.example.com:443"},
			requestScheme: "https", requestHost: "chat.example.com",
		},
		{
			name:          "exact cross origin",
			origins:       []string{"https://app.example.com"},
			requestScheme: "https", requestHost: "chat.example.com",
		},
		{
			name:          "one-label wildcard",
			origins:       []string{"https://tenant.partner.example"},
			requestScheme: "https", requestHost: "chat.example.com",
		},
		{
			name:          "IPv6 exact",
			origins:       []string{"https://[2001:0db8::1]:8443"},
			requestScheme: "https", requestHost: "chat.example.com",
		},
		{
			name: "scheme mismatch", origins: []string{"http://chat.example.com"},
			requestScheme: "https", requestHost: "chat.example.com", wantErr: true,
		},
		{
			name: "wildcard apex", origins: []string{"https://partner.example"},
			requestScheme: "https", requestHost: "chat.example.com", wantErr: true,
		},
		{
			name: "wildcard multiple labels", origins: []string{"https://a.b.partner.example"},
			requestScheme: "https", requestHost: "chat.example.com", wantErr: true,
		},
		{
			name: "multiple headers", origins: []string{"https://app.example.com", "https://app.example.com"},
			requestScheme: "https", requestHost: "chat.example.com", wantErr: true,
		},
		{
			name: "null", origins: []string{"null"},
			requestScheme: "https", requestHost: "chat.example.com", wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotErr := policy.authorize(tc.origins, tc.requestScheme, tc.requestHost)
			if (gotErr != nil) != tc.wantErr {
				t.Errorf("authorize() error = %v, wantErr %v", gotErr, tc.wantErr)
			}
		})
	}

	insecure, err := compileOriginPolicy(nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := insecure.authorize([]string{"null", "https://evil.example"}, "bad", "bad"); err != nil {
		t.Errorf("explicit insecure bypass rejected Origin: %v", err)
	}
}

func TestCompileOriginPolicyRejectsMalformedPattern(t *testing.T) {
	if _, err := compileOriginPolicy([]string{"https://ok.example", "https://*example.com"}, false); err == nil {
		t.Fatal("malformed AllowedOrigins entry accepted")
	}
	if _, err := compileOriginPolicy([]string{"https://ok.example"}, true); err == nil {
		t.Fatal("InsecureSkipOriginCheck combined with AllowedOrigins accepted")
	}
}
