package i18n

import (
	"math"
	"testing"

	"golang.org/x/text/feature/plural"
	"golang.org/x/text/language"
)

func TestPluralForm(t *testing.T) {
	en := language.English
	tr := language.Turkish
	pl := language.Polish

	tests := []struct {
		name  string
		tag   language.Tag
		count any
		want  plural.Form
	}{
		{"en int 1", en, 1, plural.One},
		{"en int 2", en, 2, plural.Other},
		{"en int 0", en, 0, plural.Other},
		{"en negative 1", en, -1, plural.One},
		{"en int64 1", en, int64(1), plural.One},
		{"en int8 1", en, int8(1), plural.One},
		{"en integral float 1.0", en, 1.0, plural.One},
		{"en integral float32 2.0", en, float32(2), plural.Other},
		{"en string 1", en, "1", plural.One},
		{"en string -1", en, "-1", plural.One},
		{"en string 1.5", en, "1.5", plural.Other},
		// "1.0" has a visible fraction digit, so the English rule
		// (i = 1 and v = 0) no longer applies.
		{"en string 1.0 visible fraction", en, "1.0", plural.Other},
		{"tr 1", tr, 1, plural.One},
		{"tr 5", tr, 5, plural.Other},
		{"pl 1", pl, 1, plural.One},
		{"pl 2", pl, 2, plural.Few},
		{"pl 5", pl, 5, plural.Many},
		{"pl 22", pl, 22, plural.Few},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := pluralForm(tt.tag, tt.count)
			if err != nil {
				t.Fatalf("pluralForm(%v, %v): %v", tt.tag, tt.count, err)
			}
			if got != tt.want {
				t.Errorf("pluralForm(%v, %v) = %v, want %v", tt.tag, tt.count, got, tt.want)
			}
		})
	}
}

func TestPluralForm_Invalid(t *testing.T) {
	tests := []struct {
		name  string
		count any
	}{
		{"non-integral float", 3.14},
		{"NaN", math.NaN()},
		{"empty string", ""},
		{"bare minus", "-"},
		{"non-numeric string", "abc"},
		{"exponent notation unsupported", "1e3"},
		{"unsupported type", struct{}{}},
		{"unsupported uint", uint(1)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := pluralForm(language.English, tt.count); err == nil {
				t.Errorf("pluralForm(en, %#v): expected error", tt.count)
			}
		})
	}
}
