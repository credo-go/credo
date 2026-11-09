package config

import (
	"errors"
	"fmt"
)

// Section names a configuration section and the type it decodes into, once:
//
//	var AppSection = config.Section[AppConfig]("app")
//
// The key is a dotted path, as [Config.Unmarshal] takes. A Section is a
// named string type, so a declaration is a conversion and an untyped string
// constant converts to it wherever a Section is expected:
// app.GetConfig[AppConfig]("app") names the same section as
// app.GetConfig(AppSection).
//
// The descriptor is the receiver of the call that reads it. Its [Section.Decode]
// method value is an ordinary constructor over the configuration, so binding a
// section in the container needs no App method:
//
//	app.Provide(AppSection.Decode) // a snapshot of the section, bound in DI
//
// A section that only one constructor reads, such as the section a database
// is opened from, is decoded in that constructor and not bound; two sections
// that decode into one type are each decoded where they are used, or bound
// under types of their own.
type Section[T any] string

// Decode decodes the section from src into a T, a snapshot that a reload
// does not change. It decodes through src's Unmarshal, so a *Config decodes
// strictly unless it was loaded with [WithWeakDecoding], and a T that
// implements Validate() error is validated. A missing section, a decode
// failure and a failed validation are errors that name the section's key; so
// is an empty key, which would otherwise decode the whole tree.
func (s Section[T]) Decode(src RawConfig) (T, error) {
	var v T
	if s == "" {
		return v, errEmptySectionKey
	}
	if err := src.Unmarshal(string(s), &v); err != nil {
		var zero T
		return zero, fmt.Errorf("config: section %q: %w", string(s), err)
	}
	return v, nil
}

// errEmptySectionKey is returned for a Section declared with an empty key.
var errEmptySectionKey = errors.New(
	`config: Section.Decode: empty section key; declare the section with its dotted key, as config.Section[T]("app") does`)
