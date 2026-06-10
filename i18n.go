package credo

import (
	"errors"
	"fmt"
	"io/fs"
	"net/http"

	internali18n "github.com/credo-go/credo/internal/i18n"
)

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
}

// UseI18n initializes i18n for the application. It loads locale files,
// stores the bundle, and adds a global middleware for locale detection.
//
// Behavior:
//   - No args or zero-value cfg: reads RawConfig "i18n" key; if absent, uses
//     defaults (dir="locales/", default="en").
//   - Dir doesn't exist or is empty: returns nil (i18n inactive, no middleware).
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

	var cfg I18nConfig
	if len(cfgs) > 0 {
		cfg = cfgs[0]
	}

	// Apply defaults from RawConfig if zero-value config.
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
			}
			if rc.Default != "" && cfg.Default == "" {
				cfg.Default = rc.Default
			}
		}
		// Still nothing? Use defaults.
		if cfg.Dir == "" && cfg.DirFS == nil {
			cfg.Dir = "locales/"
		}
	}

	if cfg.Default == "" {
		cfg.Default = "en"
	}

	// Load bundle.
	bundle, err := loadI18nBundle(cfg)
	if err != nil {
		return err
	}
	if bundle == nil {
		// Dir doesn't exist or empty — i18n inactive.
		app.logger.Warn("credo: i18n inactive, locale directory not found or empty")
		return nil
	}

	app.i18nBundle = bundle
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

// loadI18nBundle creates and loads a Bundle from the config.
// Returns nil if the directory doesn't exist (i18n inactive).
func loadI18nBundle(cfg I18nConfig) (*internali18n.Bundle, error) {
	bundle, err := internali18n.NewBundleFromString(cfg.Default)
	if err != nil {
		return nil, err
	}

	switch {
	case cfg.DirFS != nil:
		if err := bundle.LoadDirFS(cfg.DirFS, "."); err != nil {
			return nil, err
		}
	case cfg.Dir != "":
		if err := bundle.LoadDir(cfg.Dir); err != nil {
			// If dir doesn't exist, i18n is inactive.
			if errors.Is(err, fs.ErrNotExist) {
				return nil, nil
			}
			return nil, err
		}
	}

	if !bundle.HasMessages() {
		return nil, nil
	}

	return bundle, nil
}
