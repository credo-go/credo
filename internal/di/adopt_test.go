package di_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/credo-go/credo/internal/di"
)

func TestHas(t *testing.T) {
	c := di.New()
	if c.Has[*SimpleService]() {
		t.Fatal("Has should be false before registration")
	}
	calls := 0
	c.MustProvide[*pgUserRepo](func() *pgUserRepo {
		calls++
		return NewPgUserRepo()
	})
	c.MustAlias[UserRepo, *pgUserRepo]()
	c.MustProvideValue[*SimpleService](&SimpleService{})

	if !c.Has[*pgUserRepo]() || !c.Has[UserRepo]() || !c.Has[*SimpleService]() {
		t.Fatal("Has should report direct registrations, aliases and values")
	}
	if calls != 0 {
		t.Fatalf("Has ran the constructor %d times", calls)
	}
	if c.IsProtected[*SimpleService]() {
		t.Fatal("Has must not protect a binding")
	}
}

func TestAdoptValue(t *testing.T) {
	t.Run("adopts and protects a prebuilt value", func(t *testing.T) {
		c := di.New()
		original := &SimpleService{Value: "prebuilt"}
		c.MustProvideValue[*SimpleService](original)

		validated := 0
		got, err := c.AdoptValue[*SimpleService](func(s *SimpleService) error {
			validated++
			if s != original {
				return errors.New("unexpected instance")
			}
			return nil
		})
		if err != nil || got != original || validated != 1 {
			t.Fatalf("AdoptValue = (%p, %v), validated %d; want original %p", got, err, validated, original)
		}
		if !c.IsProtected[*SimpleService]() {
			t.Fatal("successful adoption should protect the binding")
		}
		if _, _, err := c.Replace[*SimpleService](&SimpleService{}); err == nil {
			t.Fatal("Replace should reject an adopted binding")
		}
		// Adopting the same protected instance again succeeds.
		if again, err := c.AdoptValue[*SimpleService](nil); err != nil || again != original {
			t.Fatalf("second AdoptValue = (%p, %v)", again, err)
		}
	})

	t.Run("validation failure leaves binding repairable", func(t *testing.T) {
		c := di.New()
		var typedNil *SimpleService
		c.MustProvideValue[*SimpleService](typedNil)

		_, err := c.AdoptValue[*SimpleService](func(s *SimpleService) error {
			if s == nil {
				return errors.New("must not be nil")
			}
			return nil
		})
		if err == nil || !strings.Contains(err.Error(), "must not be nil") {
			t.Fatalf("AdoptValue = %v, want the validation error", err)
		}
		if c.IsProtected[*SimpleService]() {
			t.Fatal("failed validation must not protect the binding")
		}
		repaired := &SimpleService{Value: "repaired"}
		if _, _, err := c.Replace[*SimpleService](repaired); err != nil {
			t.Fatalf("repair through Replace: %v", err)
		}
		if got, err := c.AdoptValue[*SimpleService](nil); err != nil || got != repaired {
			t.Fatalf("AdoptValue after repair = (%p, %v), want repaired %p", got, err, repaired)
		}
	})

	t.Run("constructor binding is rejected without running it", func(t *testing.T) {
		c := di.New()
		calls := 0
		c.MustProvide[*SimpleService](func() *SimpleService {
			calls++
			return NewSimpleService()
		})
		_, err := c.AdoptValue[*SimpleService](nil)
		if err == nil || !strings.Contains(err.Error(), "constructor") {
			t.Fatalf("AdoptValue of a constructor binding = %v, want explanatory error", err)
		}
		if calls != 0 {
			t.Fatalf("constructor ran %d times during adoption", calls)
		}
		if c.IsProtected[*SimpleService]() {
			t.Fatal("rejected adoption must not protect the binding")
		}
		if _, _, err := c.Replace[*SimpleService](&SimpleService{}); err != nil {
			t.Fatalf("binding should stay repairable: %v", err)
		}
	})

	t.Run("not registered", func(t *testing.T) {
		c := di.New()
		if _, err := c.AdoptValue[*SimpleService](nil); err == nil || !strings.Contains(err.Error(), "not registered") {
			t.Fatalf("AdoptValue = %v, want not-registered error", err)
		}
	})

	t.Run("frozen", func(t *testing.T) {
		c := di.New()
		c.MustProvideValue[*SimpleService](&SimpleService{})
		seal(t, c)
		if _, err := c.AdoptValue[*SimpleService](nil); err == nil || !strings.Contains(err.Error(), "frozen") {
			t.Fatalf("AdoptValue after Seal = %v, want frozen error", err)
		}
	})

	t.Run("replacement during validation aborts adoption", func(t *testing.T) {
		c := di.New()
		original := &SimpleService{Value: "original"}
		replacement := &SimpleService{Value: "replacement"}
		c.MustProvideValue[*SimpleService](original)

		got, err := c.AdoptValue[*SimpleService](func(s *SimpleService) error {
			// A racing composition root swaps the binding while it is validated.
			if _, _, err := c.Replace[*SimpleService](replacement); err != nil {
				return err
			}
			return nil
		})
		if err == nil || !strings.Contains(err.Error(), "replaced") {
			t.Fatalf("AdoptValue = (%v, %v), want replaced-during-validation error", got, err)
		}
		if c.IsProtected[*SimpleService]() {
			t.Fatal("a stale read must not protect the replacement")
		}
		if got, err := c.AdoptValue[*SimpleService](nil); err != nil || got != replacement {
			t.Fatalf("adopting the replacement = (%p, %v), want %p", got, err, replacement)
		}
	})

	t.Run("finalize during validation aborts adoption", func(t *testing.T) {
		c := di.New()
		original := &SimpleService{Value: "original"}
		c.MustProvideValue[*SimpleService](original)

		_, err := c.AdoptValue[*SimpleService](func(*SimpleService) error {
			seal(t, c)
			return nil
		})
		if err == nil || !strings.Contains(err.Error(), "frozen") {
			t.Fatalf("AdoptValue = %v, want frozen error after a racing Seal", err)
		}
		if c.IsProtected[*SimpleService]() {
			t.Fatal("adoption that lost to Finalize must not protect")
		}
	})
}
