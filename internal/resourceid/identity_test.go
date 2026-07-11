package resourceid

import (
	"math"
	"testing"
	"unsafe"
)

type identityProvider struct {
	token any
}

func (provider identityProvider) ResourceIdentity() any { return provider.token }

type panickingIdentityProvider struct{}

func (panickingIdentityProvider) ResourceIdentity() any { panic("boom") }

func TestOf(t *testing.T) {
	t.Run("same explicit pointer token", func(t *testing.T) {
		token := new(int)
		first, err := Of(identityProvider{token: token})
		if err != nil {
			t.Fatalf("Of(first) = %v", err)
		}
		second, err := Of(identityProvider{token: token})
		if err != nil {
			t.Fatalf("Of(second) = %v", err)
		}
		if first != second {
			t.Fatal("equal provider tokens produced different identities")
		}
	})

	t.Run("default pointer identity", func(t *testing.T) {
		value := new(int)
		first, err := Of(value)
		if err != nil {
			t.Fatalf("Of(first) = %v", err)
		}
		second, err := Of(value)
		if err != nil {
			t.Fatalf("Of(second) = %v", err)
		}
		if first != second {
			t.Fatal("same pointer produced different identities")
		}
	})

	t.Run("typed nil", func(t *testing.T) {
		var value *int
		if _, err := Of(value); err == nil {
			t.Fatal("Of(typed nil) should fail")
		}
	})

	t.Run("non-comparable", func(t *testing.T) {
		if _, err := Of([]int{1}); err == nil {
			t.Fatal("Of(slice) should fail")
		}
	})

	t.Run("non-reflexive", func(t *testing.T) {
		if _, err := Of(math.NaN()); err == nil {
			t.Fatal("Of(NaN) should fail")
		}
	})

	t.Run("invalid provider token", func(t *testing.T) {
		if _, err := Of(identityProvider{token: nil}); err == nil {
			t.Fatal("Of(provider returning nil) should fail")
		}
		if _, err := Of(identityProvider{token: []int{1}}); err == nil {
			t.Fatal("Of(provider returning slice) should fail")
		}
		if _, err := Of(identityProvider{token: unsafe.Pointer(nil)}); err == nil {
			t.Fatal("Of(provider returning nil unsafe.Pointer) should fail")
		}
	})

	t.Run("provider panic", func(t *testing.T) {
		if _, err := Of(panickingIdentityProvider{}); err == nil {
			t.Fatal("Of(panicking provider) should fail")
		}
	})
}
