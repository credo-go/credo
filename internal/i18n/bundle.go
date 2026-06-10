// Copyright (c) 2014 Nick Snyder https://github.com/nicksnyder/go-i18n
// License: MIT (https://github.com/nicksnyder/go-i18n/blob/main/LICENSE)
//
// Adapted for the Credo framework (github.com/credo-go/credo).
// Simplified: JSON-only loading, directory-per-locale structure,
// no RegisterUnmarshalFunc, no custom delimiters.

package i18n

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"golang.org/x/text/feature/plural"
	"golang.org/x/text/language"
)

// Bundle holds all loaded messages and field translations.
// It is safe for concurrent use after loading is complete.
type Bundle struct {
	defaultLang language.Tag
	messages    map[language.Tag]map[string]*messageTemplate // tag → id → compiled template
	fields      map[language.Tag]map[string]string           // tag → fieldName → displayName
	tags        []language.Tag
	matcher     language.Matcher
}

// NewBundle creates a new Bundle with the given default language.
func NewBundle(defaultLang language.Tag) *Bundle {
	return &Bundle{
		defaultLang: defaultLang,
		messages:    make(map[language.Tag]map[string]*messageTemplate),
		fields:      make(map[language.Tag]map[string]string),
	}
}

// DefaultLanguage returns the bundle's default language tag.
func (b *Bundle) DefaultLanguage() language.Tag {
	return b.defaultLang
}

// AddMessages registers messages for the given language tag.
// If a message with the same ID already exists, it is overwritten.
func (b *Bundle) AddMessages(tag language.Tag, msgs ...*Message) error {
	if _, ok := b.messages[tag]; !ok {
		b.messages[tag] = make(map[string]*messageTemplate)
	}
	for _, msg := range msgs {
		if msg.ID == "" {
			return fmt.Errorf("i18n: message ID must not be empty")
		}
		mt, err := newMessageTemplate(msg)
		if err != nil {
			return err
		}
		b.messages[tag][msg.ID] = mt
	}
	b.rebuildMatcher()
	return nil
}

// SetFields sets field name translations for the given language tag.
// Used for opt-in field name injection (Mode 2 in ADR-008).
func (b *Bundle) SetFields(tag language.Tag, fields map[string]string) {
	b.fields[tag] = maps.Clone(fields)
	b.rebuildMatcher()
}

// LanguageTags returns all language tags that have messages loaded.
func (b *Bundle) LanguageTags() []language.Tag {
	return slices.Clone(b.tags)
}

// LoadDir loads locale files from a filesystem directory.
// Expected structure: {dir}/{lang}/messages.json [+ fields.json]
func (b *Bundle) LoadDir(dir string) error {
	return b.LoadDirFS(os.DirFS(dir), ".")
}

// LoadDirFS loads locale files from an fs.FS.
// Expected structure: {dir}/{lang}/messages.json [+ fields.json]
func (b *Bundle) LoadDirFS(fsys fs.FS, dir string) error {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return fmt.Errorf("i18n: read dir %q: %w", dir, err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		langDir := entry.Name()
		tag, parseErr := language.Parse(langDir)
		if parseErr != nil {
			// Skip directories that aren't valid language tags.
			continue
		}

		langPath := filepath.Join(dir, langDir)

		// Load messages.json
		if err := b.loadMessages(fsys, langPath, tag); err != nil {
			return err
		}

		// Load fields.json (optional — missing file is OK, parse errors are not)
		if err := b.loadFields(fsys, langPath, tag); err != nil {
			return err
		}
	}

	b.rebuildMatcher()
	return nil
}

func (b *Bundle) loadMessages(fsys fs.FS, langPath string, tag language.Tag) error {
	msgPath := filepath.Join(langPath, "messages.json")
	data, err := fs.ReadFile(fsys, filepath.ToSlash(msgPath))
	if err != nil {
		return fmt.Errorf("i18n: read %q: %w", msgPath, err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("i18n: parse %q: %w", msgPath, err)
	}

	if _, ok := b.messages[tag]; !ok {
		b.messages[tag] = make(map[string]*messageTemplate)
	}

	for id, val := range raw {
		msg, err := messageFromJSON(id, val)
		if err != nil {
			return err
		}
		mt, err := newMessageTemplate(msg)
		if err != nil {
			return err
		}
		b.messages[tag][id] = mt
	}
	return nil
}

func (b *Bundle) loadFields(fsys fs.FS, langPath string, tag language.Tag) error {
	fieldsPath := filepath.Join(langPath, "fields.json")
	data, err := fs.ReadFile(fsys, filepath.ToSlash(fieldsPath))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil // optional file
		}
		return fmt.Errorf("i18n: read %q: %w", fieldsPath, err)
	}

	var fields map[string]string
	if err := json.Unmarshal(data, &fields); err != nil {
		return fmt.Errorf("i18n: parse %q: %w", fieldsPath, err)
	}

	b.fields[tag] = fields
	return nil
}

func (b *Bundle) rebuildMatcher() {
	tags := make([]language.Tag, 0, len(b.messages))
	for tag := range b.messages {
		tags = append(tags, tag)
	}
	// Sort for deterministic ordering; default language first.
	slices.SortFunc(tags, func(x, y language.Tag) int {
		switch {
		case x == b.defaultLang:
			return -1
		case y == b.defaultLang:
			return 1
		default:
			return cmp.Compare(x.String(), y.String())
		}
	})
	b.tags = tags
	b.matcher = language.NewMatcher(tags)
}

// messageTemplates returns the message map for the given tag.
func (b *Bundle) messageTemplates(tag language.Tag) map[string]*messageTemplate {
	return b.messages[tag]
}

// fieldName looks up a translated field name for the given tag and raw field name.
func (b *Bundle) fieldName(tag language.Tag, raw string) string {
	if fields, ok := b.fields[tag]; ok {
		if name, ok := fields[raw]; ok {
			return name
		}
	}
	return raw
}

// --- String-based public APIs for root package ---

// matchTag resolves lang (a BCP 47 tag or Accept-Language header value) to one
// of the bundle's registered tags. ok is false when lang is empty/unparseable
// or the bundle has no matcher yet, in which case callers fall back to the
// default language.
//
// The match is selected by index (b.tags[idx]) rather than the tag returned by
// the matcher: x/text may fold the caller's extensions into the returned tag,
// which would then miss the registered key in b.messages.
func (b *Bundle) matchTag(lang string) (language.Tag, bool) {
	tags, _, err := language.ParseAcceptLanguage(lang)
	if err != nil || len(tags) == 0 || b.matcher == nil {
		return language.Und, false
	}
	_, idx, _ := b.matcher.Match(tags...)
	if idx < 0 || idx >= len(b.tags) {
		return language.Und, false
	}
	return b.tags[idx], true
}

// MatchLangString resolves a language string (BCP 47 tag or Accept-Language header)
// against the bundle's available tags and returns the matched tag as a string.
// Returns the default language if no match is found.
func (b *Bundle) MatchLangString(lang string) string {
	if tag, ok := b.matchTag(lang); ok {
		return tag.String()
	}
	return b.defaultLang.String()
}

// TranslateForLang looks up a message by key for the given language string.
// Returns the translated string and true if found, or ("", false) if not.
// The lang parameter can be a BCP 47 tag or Accept-Language header value.
// The message's Other plural form is always used; for count-based plural
// selection use [Bundle.TranslatePluralForLang].
func (b *Bundle) TranslateForLang(lang, key string, data map[string]any) (string, bool) {
	tag, ok := b.matchTag(lang)
	if !ok {
		tag = b.defaultLang
	}
	return b.translateForTag(tag, key, data)
}

// TranslatePluralForLang is like TranslateForLang but renders the CLDR
// cardinal plural form selected for count (any integer kind, an integral
// float, or a decimal string such as "1.5"). count is also exposed to the
// template as {{.count}}. When count cannot be interpreted as a number,
// the Other form is rendered.
func (b *Bundle) TranslatePluralForLang(lang, key string, count any, data map[string]any) (string, bool) {
	tag, ok := b.matchTag(lang)
	if !ok {
		tag = b.defaultLang
	}

	// Merge count into a copy to avoid mutating the caller's map.
	merged := make(map[string]any, len(data)+1)
	maps.Copy(merged, data)
	merged["count"] = count

	for _, t := range b.lookupChain(tag) {
		mt, ok := b.messageTemplates(t)[key]
		if !ok {
			continue
		}
		// The form depends on the language whose message is rendered.
		form, err := pluralForm(t, count)
		if err != nil {
			form = plural.Other
		}
		s, err := mt.execute(form, merged)
		if err != nil {
			return "", false
		}
		return s, true
	}
	return "", false
}

func (b *Bundle) translateForTag(tag language.Tag, key string, data map[string]any) (string, bool) {
	for _, t := range b.lookupChain(tag) {
		if mt, ok := b.messageTemplates(t)[key]; ok {
			s, err := mt.execute(plural.Other, data)
			if err != nil {
				return "", false
			}
			return s, true
		}
	}
	return "", false
}

// lookupChain returns the message lookup order for tag: the tag itself,
// then the default language as fallback when different.
func (b *Bundle) lookupChain(tag language.Tag) []language.Tag {
	if tag == b.defaultLang {
		return []language.Tag{tag}
	}
	return []language.Tag{tag, b.defaultLang}
}

// FieldNameForLang returns the translated field display name for the given
// language string and raw field name. Returns raw if no translation exists.
func (b *Bundle) FieldNameForLang(lang, raw string) string {
	tag, ok := b.matchTag(lang)
	if !ok {
		tag = b.defaultLang
	}
	return b.fieldName(tag, raw)
}

// HasMessages returns true if the bundle has any loaded messages.
func (b *Bundle) HasMessages() bool {
	return len(b.messages) > 0
}

// DefaultLang returns the default language as a string.
func (b *Bundle) DefaultLang() string {
	return b.defaultLang.String()
}

// ParseTag parses a BCP 47 language tag string into a language.Tag.
// Exported for use by the root package to construct bundles.
func ParseTag(s string) (language.Tag, error) {
	return language.Parse(s)
}

// NewBundleFromString creates a new Bundle with the default language parsed
// from a BCP 47 string. Returns an error if the string is not a valid tag.
func NewBundleFromString(defaultLang string) (*Bundle, error) {
	tag, err := language.Parse(defaultLang)
	if err != nil {
		return nil, fmt.Errorf("i18n: invalid default language %q: %w", defaultLang, err)
	}
	return NewBundle(tag), nil
}
