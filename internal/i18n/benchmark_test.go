package i18n

import (
	"testing"

	"golang.org/x/text/language"
)

var (
	benchmarkTranslationSink string
	benchmarkFoundSink       bool
)

func newBenchmarkBundle(b *testing.B) *Bundle {
	b.Helper()
	bundle := NewBundle(language.English)
	err := bundle.AddMessages(
		language.English,
		&Message{ID: "required", Other: "is required"},
	)
	if err != nil {
		b.Fatal(err)
	}
	err = bundle.AddMessages(
		language.Turkish,
		&Message{ID: "required", Other: "zorunludur"},
	)
	if err != nil {
		b.Fatal(err)
	}
	return bundle
}

// BenchmarkBundleTranslate separates repeated language-string matching from
// lookup and template execution with an already resolved language tag.
func BenchmarkBundleTranslate(b *testing.B) {
	bundle := newBenchmarkBundle(b)

	b.Run("CanonicalString", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			benchmarkTranslationSink, benchmarkFoundSink = bundle.TranslateForLang("tr", "required", nil)
		}
	})

	b.Run("ResolvedTag", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			benchmarkTranslationSink, benchmarkFoundSink = bundle.translateForTag(language.Turkish, "required", nil)
		}
	})
}
