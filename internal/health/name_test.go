package health

import "testing"

func TestValidateName(t *testing.T) {
	for _, name := range []string{"primary", "database.primary", "analytics-db"} {
		t.Run("valid_"+name, func(t *testing.T) {
			if err := ValidateName(name); err != nil {
				t.Fatalf("ValidateName(%q) = %v", name, err)
			}
		})
	}

	for _, name := range []string{"", " primary", "primary ", "credo.primary", "primary\nreplica"} {
		t.Run("invalid_"+name, func(t *testing.T) {
			if err := ValidateName(name); err == nil {
				t.Fatalf("ValidateName(%q) should fail", name)
			}
		})
	}
}
