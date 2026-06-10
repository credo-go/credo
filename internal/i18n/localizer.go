// Copyright (c) 2014 Nick Snyder https://github.com/nicksnyder/go-i18n
// License: MIT (https://github.com/nicksnyder/go-i18n/blob/main/LICENSE)
//
// Adapted for the Credo framework (github.com/credo-go/credo).
// Simplified: no Must* variants, no custom template parser.

package i18n

import (
	"fmt"
	"maps"
	"sync"

	"golang.org/x/text/feature/plural"
	"golang.org/x/text/language"
)

// MessageNotFoundErr is returned when a message cannot be found in the bundle.
type MessageNotFoundErr struct {
	ID  string
	Tag language.Tag
}

func (e *MessageNotFoundErr) Error() string {
	return fmt.Sprintf("i18n: message %q not found for %q", e.ID, e.Tag)
}

// Localizer provides per-request message lookup and rendering.
type Localizer struct {
	bundle      *Bundle
	tags        []language.Tag
	matched     []language.Tag // cached result of computeMatchedTags
	matchedOnce sync.Once
}

// NewLocalizer creates a Localizer that will look up messages in the bundle
// for the given language preferences (BCP 47 strings, e.g. "en", "tr", "en-US").
func NewLocalizer(bundle *Bundle, langs ...string) *Localizer {
	tags := make([]language.Tag, 0, len(langs))
	for _, lang := range langs {
		t, err := language.Parse(lang)
		if err == nil {
			tags = append(tags, t)
		}
	}
	return &Localizer{bundle: bundle, tags: tags}
}

// Localize looks up a message by ID and renders it with the given data.
// Returns MessageNotFoundErr if the message is not found in any language.
func (l *Localizer) Localize(id string, data map[string]any) (string, error) {
	s, _, err := l.LocalizeWithTag(id, data)
	return s, err
}

// LocalizeWithTag is like Localize but also returns the matched language tag.
func (l *Localizer) LocalizeWithTag(id string, data map[string]any) (string, language.Tag, error) {
	// Try each preferred language, then fall back to default.
	for _, tag := range l.matchedTags() {
		msgs := l.bundle.messageTemplates(tag)
		if msgs == nil {
			continue
		}
		mt, ok := msgs[id]
		if !ok {
			continue
		}
		s, err := mt.execute(plural.Other, data)
		if err != nil {
			return "", tag, err
		}
		return s, tag, nil
	}
	return "", language.Und, &MessageNotFoundErr{ID: id, Tag: l.primaryTag()}
}

// LocalizePlural looks up a message by ID and renders the correct plural form
// based on count. The count value is also available as {{.count}} in templates.
// count may be any integer kind, an integral float, or a decimal string;
// non-integral floats must be pre-formatted into a string (e.g. "1.5").
func (l *Localizer) LocalizePlural(id string, count any, data map[string]any) (string, error) {
	// Validate count up front so an invalid value fails regardless of
	// which language ends up serving the message.
	if _, err := pluralForm(language.English, count); err != nil {
		return "", fmt.Errorf("i18n: invalid count %v: %w", count, err)
	}

	// Merge count into a copy to avoid mutating caller's map.
	merged := make(map[string]any, len(data)+1)
	maps.Copy(merged, data)
	merged["count"] = count

	for _, tag := range l.matchedTags() {
		msgs := l.bundle.messageTemplates(tag)
		if msgs == nil {
			continue
		}
		mt, ok := msgs[id]
		if !ok {
			continue
		}

		// Determine the plural form for the language being rendered.
		form, err := pluralForm(tag, count)
		if err != nil {
			form = plural.Other
		}

		s, err := mt.execute(form, merged)
		if err != nil {
			return "", err
		}
		return s, nil
	}

	return "", &MessageNotFoundErr{ID: id, Tag: l.primaryTag()}
}

// matchedTags returns the preferred tags matched against the bundle's available tags.
// The result is cached because the bundle and tags are immutable after construction.
func (l *Localizer) matchedTags() []language.Tag {
	l.matchedOnce.Do(func() {
		l.matched = l.computeMatchedTags()
	})
	return l.matched
}

// computeMatchedTags performs the actual tag matching computation.
func (l *Localizer) computeMatchedTags() []language.Tag {
	result := make([]language.Tag, 0, len(l.tags)+1)
	seen := make(map[language.Tag]bool, len(l.tags)+1)

	for _, tag := range l.tags {
		matched, _, _ := l.bundle.matcher.Match(tag)
		if !seen[matched] {
			result = append(result, matched)
			seen[matched] = true
		}
	}

	// Always try default language as final fallback.
	if !seen[l.bundle.defaultLang] {
		result = append(result, l.bundle.defaultLang)
	}

	return result
}

func (l *Localizer) primaryTag() language.Tag {
	if len(l.tags) > 0 {
		return l.tags[0]
	}
	return l.bundle.defaultLang
}
