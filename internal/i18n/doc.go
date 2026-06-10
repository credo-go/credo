// Copyright (c) 2014 Nick Snyder https://github.com/nicksnyder/go-i18n
// License: MIT (https://github.com/nicksnyder/go-i18n/blob/main/LICENSE)
//
// Adapted for the Credo framework (github.com/credo-go/credo).

// Package i18n is the internal message lookup engine for the Credo framework.
// It provides Bundle-based message loading, Localizer for per-request lookup,
// and CLDR plural form selection (delegated to golang.org/x/text/feature/plural).
//
// This package is internal — the public API is exposed via the root credo package:
//   - app.UseI18n(...)             — setup
//   - ctx.Locale()                 — detected language
//   - ctx.T(key, data)             — translate (Other form)
//   - ctx.TPlural(key, count, data) — translate with plural selection
package i18n
