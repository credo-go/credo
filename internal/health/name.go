package health

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// ValidateName validates the canonical identifier shared by named and store
// health checks. Names are never normalized: padded input is rejected so
// duplicate detection remains an exact, deterministic comparison.
func ValidateName(name string) error {
	if name == "" {
		return errors.New("name must not be empty")
	}
	if strings.TrimSpace(name) != name {
		return fmt.Errorf("name %q must not have leading or trailing whitespace", name)
	}
	if strings.HasPrefix(name, "credo.") {
		return fmt.Errorf("name %q uses the reserved credo. prefix", name)
	}
	for _, char := range name {
		if unicode.IsControl(char) {
			return fmt.Errorf("name %q must not contain control characters", name)
		}
	}
	return nil
}
