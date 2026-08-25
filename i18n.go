package credo

import (
	"fmt"
	"io/fs"
	"net/http"
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

	// Detect is a function that extracts the preferred language from an HTTP request.
	// Defaults to reading the Accept-Language header if nil.
	Detect func(r *http.Request) string

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

// UseI18n initializes i18n for the application. It loads locale files,
// stores the bundle, and adds a global middleware for locale detection.
//
// Behavior:
//   - No args or zero-value cfg: reads RawConfig "i18n" key; if absent, uses
//     defaults (dir="locales/", default="en").
//   - An implicitly discovered locales/ directory may be absent (inactive).
//   - An explicit Dir or DirFS must exist and contain at least one message.
//   - Messages/Fields provide one programmatic catalog for Default; external
//     files override it key by key.
//   - Malformed files: returns error.
//   - Valid files: loads bundle, adds locale detection middleware.
//
// Unlike registration-only setup APIs such as [App.UseHealth], UseI18n reads
// locale files from disk or an [fs.FS] — an external operation that can fail
// for reasons other than a programming mistake — so failures are returned as
// errors rather than panicking. It still panics if called after compile,
// like all configuration APIs.
func (app *App) UseI18n(cfgs ...I18nConfig) error {
	app.checkFrozen("UseI18n")
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
		return nil
	}

	app.i18nBundle = bundle
	app.messageKeyResolver = cfg.ResolveMessageKey
	app.logger.Info("credo: i18n loaded", "default", cfg.Default)

	// Add locale detection middleware.
	detect := cfg.Detect
	if detect == nil {
		detect = func(r *http.Request) string {
			return r.Header.Get("Accept-Language")
		}
	}

	app.GlobalMiddleware(func(next Handler) Handler {
		return func(ctx *Context) error {
			lang := detect(ctx.Request().Request)
			if lang != "" {
				ctx.locale = bundle.MatchLangString(lang)
			} else {
				ctx.locale = cfg.Default
			}
			return next(ctx)
		}
	})

	return nil
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
