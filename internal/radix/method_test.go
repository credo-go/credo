package radix

import (
	"sync"
	"testing"
)

// saveAndRestore snapshots the global method state and returns a cleanup func.
func saveAndRestore(t *testing.T) {
	t.Helper()
	origMap := make(map[string]MethodTyp, len(methodMap))
	for k, v := range methodMap {
		origMap[k] = v
	}
	origBit := nextMethodBit
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		methodMap = origMap
		nextMethodBit = origBit
	})
}

func TestRegisterMethod_NoBitCollision(t *testing.T) {
	saveAndRestore(t)

	custom1 := registerMethod("PURGE")
	custom2 := registerMethod("LINK")

	// Must not collide with any standard method
	if custom1&mAny != 0 {
		t.Errorf("PURGE bit %b collides with standard methods %b", custom1, mAny)
	}
	if custom2&mAny != 0 {
		t.Errorf("LINK bit %b collides with standard methods %b", custom2, mAny)
	}
	// Must not collide with each other
	if custom1 == custom2 {
		t.Errorf("PURGE and LINK have same bit: %b", custom1)
	}
	// Must be the correct next bits
	if custom1 != 1<<10 {
		t.Errorf("PURGE = %d, want %d", custom1, 1<<10)
	}
	if custom2 != 1<<11 {
		t.Errorf("LINK = %d, want %d", custom2, 1<<11)
	}
}

func TestRegisterMethod_Idempotent(t *testing.T) {
	saveAndRestore(t)

	first := registerMethod("PURGE")
	second := registerMethod("PURGE")
	if first != second {
		t.Errorf("repeat registration returned different bit: %d vs %d", first, second)
	}
}

func TestRegisterMethod_StandardMethodReturnsExisting(t *testing.T) {
	got := registerMethod("GET")
	if got != MGet {
		t.Errorf("RegisterMethod(GET) = %d, want %d", got, MGet)
	}
}

func TestRegisterMethod_Concurrent(t *testing.T) {
	saveAndRestore(t)

	var wg sync.WaitGroup
	results := make([]MethodTyp, 10)
	for i := 0; i < 10; i++ {
		wg.Go(func() {
			results[i] = registerMethod("PURGE")
		})
	}
	wg.Wait()

	for i := 1; i < 10; i++ {
		if results[i] != results[0] {
			t.Errorf("goroutine %d got %d, goroutine 0 got %d", i, results[i], results[0])
		}
	}
}

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
	if len(all) != 9 {
		t.Fatalf("AllMethods() returned %d entries, want 9", len(all))
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
		{"all", mAny, []string{"CONNECT", "DELETE", "GET", "HEAD", "OPTIONS", "PATCH", "POST", "PUT", "TRACE"}},
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
