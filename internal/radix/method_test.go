package radix

import (
	"testing"
)

func TestLookupMethod(t *testing.T) {
	tests := []struct {
		method string
		want   MethodTyp
		wantOK bool
	}{
		{"CONNECT", MConnect, true},
		{"DELETE", MDelete, true},
		{"GET", MGet, true},
		{"HEAD", MHead, true},
		{"OPTIONS", MOptions, true},
		{"PATCH", MPatch, true},
		{"POST", MPost, true},
		{"PUT", MPut, true},
		{"TRACE", MTrace, true},
		{"QUERY", MQuery, true},
		{"NONEXISTENT", 0, false},
		{"PURGE", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.method, func(t *testing.T) {
			got, ok := LookupMethod(tt.method)
			if ok != tt.wantOK {
				t.Errorf("LookupMethod(%q) ok = %v, want %v", tt.method, ok, tt.wantOK)
			}
			if got != tt.want {
				t.Errorf("LookupMethod(%q) = %d, want %d", tt.method, got, tt.want)
			}
		})
	}
}

func TestAllMethods_ReturnsCopy(t *testing.T) {
	all := AllMethods()
	if len(all) != 10 {
		t.Fatalf("AllMethods() returned %d entries, want 10", len(all))
	}
	// Mutating the copy must not affect the global
	all["FAKEMUTATE"] = 999
	_, ok := LookupMethod("FAKEMUTATE")
	if ok {
		t.Error("mutating AllMethods() copy affected the global map")
	}
}

func TestMethodTypToString(t *testing.T) {
	tests := []struct {
		name string
		mtyp MethodTyp
		want []string
	}{
		{"single", MGet, []string{"GET"}},
		{"multiple sorted", MGet | MPost | MDelete, []string{"DELETE", "GET", "POST"}},
		{"all", mAny, []string{"CONNECT", "DELETE", "GET", "HEAD", "OPTIONS", "PATCH", "POST", "PUT", "QUERY", "TRACE"}},
		{"zero", 0, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MethodTypToString(tt.mtyp)
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for i, m := range got {
				if m != tt.want[i] {
					t.Errorf("methods[%d] = %q, want %q", i, m, tt.want[i])
				}
			}
		})
	}
}
