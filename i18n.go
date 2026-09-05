package credo

import (
	"fmt"
	"io/fs"
	"os"

	internali18n "github.com/credo-go/credo/internal/i18n"
)

// MessageScope identifies the presentation context of a machine code. It lets
// applications choose their own i18n namespaces without Credo adding hidden
// prefixes to codes.
type MessageScope uint8

const (
	MessageScopeError MessageScope = iota
	MessageScopeValidation
	MessageScopeBind
)

// MessageRef is passed to a [MessageKeyResolver] when a value carries no
// explicit MessageKey.
type MessageRef struct {
	Scope MessageScope
	Code  string
}

// MessageKeyResolver maps a message scope and machine code to an exact i18n
// key. Returning an empty key is a programming error and fails closed through
// the error pipeline. With no resolver Credo uses the bare code as the key.
type MessageKeyResolver func(MessageRef) string

// I18nMessages maps exact message keys to string templates for the effective
// default language. UseI18n compiles and copies the map during setup.
type I18nMessages map[string]string

// I18nFields maps exact technical field paths to display names for the
// effective default language. UseI18n copies the map during setup.
type I18nFields map[string]string

// I18nConfig configures internationalization for the application.
type I18nConfig struct {
	// Dir is the filesystem path to the locale directory (e.g., "locales/").
	// Mutually exclusive with DirFS.
	Dir string

	// DirFS is an embed.FS or any fs.FS providing locale files.
	// Mutually exclusive with Dir.
	DirFS fs.FS

	// Default is the default language tag string (e.g., "en").
	// Falls back to "en" if empty.
	Default string

	// Detect returns the request's preferred language. It runs lazily, on the
	// first [Context.Locale] or translation access of a request (the error
	// pipeline's message translation included), and its result is memoized
	// for that request; requests that never touch locale or translation do
	// not invoke it. It sees the request state at that moment — headers, the
	// route when already matched, the user when authentication already ran —
	// and later changes do not re-run it. An empty or unresolvable result
	// selects Default. Calling Locale or a translation from inside Detect is
	// programming misuse and panics; a panic in Detect caches Default for the
	// request and is handled like any request panic. nil reads the
	// Accept-Language header.
	Detect func(ctx *Context) string

	// Messages provides exact message keys for the effective Default language.
	// It may be the only message source or a base overridden by Dir/DirFS.
	Messages I18nMessages

	// Fields provides display names for exact technical field paths in the
	// effective Default language. A message source is still required.
	Fields I18nFields

	// ResolveMessageKey optionally maps framework error, validation, and bind
	// codes to application-owned exact message keys. Credo never adds a prefix.
	ResolveMessageKey MessageKeyResolver
}

// i18nFeature is the installed internationalization feature.
type i18nFeature struct {
	bundle      *internali18n.Bundle
	defaultLang string
	detect      func(*Context) string
	// resolver maps scoped machine codes to application-owned exact i18n
	// keys. nil means the bare code is used.
	resolver MessageKeyResolver
}

// UseI18n initializes i18n for the application. It loads locale files and
// installs the bundle; the request locale is then resolved lazily through
// [I18nConfig.Detect] on the first [Context.Locale] or translation access of
// each request — there is no locale middleware.
//
// Behavior:
//   - No args or zero-value cfg: reads RawConfig "i18n" key; if absent, uses
//     defaults (dir="locales/", default="en").
//   - An implicitly discovered locales/ directory may be absent (inactive).
//   - An explicit Dir or DirFS must exist and contain at least one message.
//   - Messages/Fields provide one programmatic catalog for Default; external
//     files override it key by key.
//   - Malformed files: returns error.
//   - Valid files: installs the bundle.
//
// Unlike registration-only setup APIs such as [App.UseHealth], UseI18n reads
// locale files from disk or an [fs.FS] — an external operation that can fail
// for reasons other than a programming mistake — so failures are returned as
// errors rather than panicking. It still panics if called after preparation
// or shutdown, like all configuration APIs, and when called twice: a
// completed call — an inactive conventional setup included — consumes the
// slot, while a call that returned an error leaves it free.
func (app *App) UseI18n(cfgs ...I18nConfig) error {
	app.checkFrozen("App.UseI18n")
	if app.i18nRegistered {
		panic("credo: App.UseI18n called twice")
	}
	if len(cfgs) > 1 {
		return fmt.Errorf("credo: UseI18n accepts at most one config")
	}

	var cfg I18nConfig
	if len(cfgs) > 0 {
		cfg = cfgs[0]
	}
	dirExplicit := cfg.Dir != ""
	programmatic := len(cfg.Messages) > 0 || len(cfg.Fields) > 0

	// Apply scalar defaults from RawConfig when no external source was supplied.
	if cfg.Dir == "" && cfg.DirFS == nil {
		if app.rawConfig != nil && app.rawConfig.Exists("i18n") {
			var rc struct {
				Dir     string `credo:"dir"`
				Default string `credo:"default"`
			}
			if err := app.rawConfig.Unmarshal("i18n", &rc); err != nil {
				return fmt.Errorf("credo: invalid i18n config: %w", err)
			}
			if rc.Dir != "" {
				cfg.Dir = rc.Dir
				dirExplicit = true
			}
			if rc.Default != "" && cfg.Default == "" {
				cfg.Default = rc.Default
			}
		}
		// Programmatic catalogs are self-contained. Conventional discovery only
		// applies when no catalog source was supplied.
		if cfg.Dir == "" && cfg.DirFS == nil && !programmatic {
			cfg.Dir = "locales/"
		}
	}
	if cfg.Dir != "" && cfg.DirFS != nil {
		return fmt.Errorf("credo: i18n Dir and DirFS are mutually exclusive")
	}

	if cfg.Default == "" {
		cfg.Default = "en"
	}

	// Build the complete bundle off to the side. Nothing is published until
	// every source has been validated and merged successfully.
	bundle, err := internali18n.NewBundleFromString(cfg.Default)
	if err != nil {
		return err
	}
	err = bundle.AddStringMessages(cfg.Default, map[string]string(cfg.Messages))
	if err != nil {
		return err
	}
	err = bundle.AddFields(cfg.Default, map[string]string(cfg.Fields))
	if err != nil {
		return err
	}

	if cfg.Dir != "" && !dirExplicit {
		if _, statErr := os.Stat(cfg.Dir); statErr != nil {
			if os.IsNotExist(statErr) {
				app.logger.Warn("credo: i18n inactive, locale directory not found or empty")
				app.markI18nRegistered()
				return nil
			}
			return fmt.Errorf("credo: inspect conventional i18n directory: %w", statErr)
		}
	}

	externalMessages, err := loadI18nSource(bundle, cfg)
	if err != nil {
		return err
	}
	externalExplicit := dirExplicit || cfg.DirFS != nil
	if externalExplicit && externalMessages == 0 {
		return fmt.Errorf("credo: explicit i18n source contains no messages")
	}
	if !bundle.HasMessages() {
		if len(cfg.Fields) > 0 {
			return fmt.Errorf("credo: i18n Fields require at least one message")
		}
		app.logger.Warn("credo: i18n inactive, locale directory not found or empty")
		app.markI18nRegistered()
		return nil
	}

	detect := cfg.Detect
	if detect == nil {
		detect = detectAcceptLanguage
	}
	f := &i18nFeature{
		bundle:      bundle,
		defaultLang: cfg.Default,
		detect:      detect,
		resolver:    cfg.ResolveMessageKey,
	}
	app.installFeature("App.UseI18n", func() {
		if app.i18nRegistered {
			panic("credo: App.UseI18n called twice")
		}
		app.i18nRegistered = true
		app.i18n = f
	})
	app.logger.Info("credo: i18n loaded", "default", cfg.Default)
	return nil
}

// markI18nRegistered consumes the i18n slot for an inactive setup.
func (app *App) markI18nRegistered() {
	app.installFeature("App.UseI18n", func() {
		if app.i18nRegistered {
			panic("credo: App.UseI18n called twice")
		}
		app.i18nRegistered = true
	})
}

// detectAcceptLanguage is the default locale detector.
func detectAcceptLanguage(ctx *Context) string {
	return ctx.request.Header.Get("Accept-Language")
}

func loadI18nSource(bundle *internali18n.Bundle, cfg I18nConfig) (int, error) {
	switch {
	case cfg.DirFS != nil:
		return bundle.LoadDirFSSource(cfg.DirFS, ".")
	case cfg.Dir != "":
		return bundle.LoadDirSource(cfg.Dir)
	default:
		return 0, nil
	}
}
