package credential

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFromHeader(t *testing.T) {
	tests := []struct {
		values []string
		prefix string
		want   string
		ok     bool
	}{
		{[]string{"Bearer abc"}, "Bearer", "abc", true},
		{[]string{"bearer\tabc "}, "Bearer", "abc", true},
		{[]string{"Basic x", "Bearer abc"}, "Bearer", "abc", true},
		{[]string{"Bearerabc"}, "Bearer", "", false},
		{[]string{"Bearer "}, "Bearer", "", false},
		{[]string{"  key  "}, "", "key", true},
		{[]string{"   "}, "", "", false},
	}
	for _, tt := range tests {
		r := httptest.NewRequest("GET", "/", nil)
		for _, v := range tt.values {
			r.Header.Add("Authorization", v)
		}
		got, ok := FromHeader(r, "Authorization", tt.prefix)
		if got != tt.want || ok != tt.ok {
			t.Errorf("FromHeader(%q, %q) = %q, %v; want %q, %v", tt.values, tt.prefix, got, ok, tt.want, tt.ok)
		}
	}
	if _, ok := FromHeader(httptest.NewRequest("GET", "/", nil), "", "Bearer"); ok {
		t.Error("FromHeader with no header name found a credential")
	}
}

func TestFromQueryAndCookie(t *testing.T) {
	r := httptest.NewRequest("GET", "/?token=+abc+&empty=", nil)
	r.AddCookie(&http.Cookie{Name: "jwt", Value: "def"})

	if got, ok := FromQuery(r, "token"); got != "abc" || !ok {
		t.Errorf("FromQuery(token) = %q, %v", got, ok)
	}
	if _, ok := FromQuery(r, "empty"); ok {
		t.Error("FromQuery(empty) found a credential")
	}
	if _, ok := FromQuery(r, ""); ok {
		t.Error("FromQuery with no name found a credential")
	}
	if got, ok := FromCookie(r, "jwt"); got != "def" || !ok {
		t.Errorf("FromCookie(jwt) = %q, %v", got, ok)
	}
	if _, ok := FromCookie(r, "missing"); ok {
		t.Error("FromCookie(missing) found a credential")
	}
}
