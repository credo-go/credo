package credo

import (
	jsonv1 "encoding/json"
	jsonv2 "encoding/json/v2"
)

// defaultJSONOptions is Credo's response encoding profile, applied by
// [Response.JSON] and to framework-owned default error bodies. It is
// encoding/json/v2's default behavior with two deliberate adjustments:
//
//   - [jsonv2.Deterministic] — map keys are sorted, so responses, golden-file
//     tests, and log diffs are byte-stable. encoding/json v1 always sorted;
//     v2 does not by default.
//   - FormatDurationAsNano — a [time.Duration] marshals as its integer
//     nanosecond count, as under encoding/json v1. json/v2 has no default
//     Duration representation and Go 1.27 ships without the `format:` struct
//     tag (removed before the v2 release, go.dev/issue/79071), so without
//     this option every response carrying a Duration would fail to encode.
//     It also mirrors [Request.BindBody], which decodes nanoseconds.
//
// Everything else is v2's default, which differs from v1 in ways that are
// visible on the wire: a nil slice encodes as [] and a nil map as {} (not
// null); `omitempty` drops JSON-empty values rather than Go zero values (use
// `omitzero` for the v1 meaning); `<`, `>`, and `&` are not escaped; and no
// trailing newline is written. [WithJSONOptions] overrides any of this per
// application.
var defaultJSONOptions = jsonv2.JoinOptions(
	jsonv2.Deterministic(true),
	jsonv1.FormatDurationAsNano(true),
)

// defaultErrorJSONOptions is the error-body profile for a nil App: the default
// profile with deterministic ordering re-imposed, derived exactly like
// App.errorJSONOpts so the two never drift.
var defaultErrorJSONOptions = jsonv2.JoinOptions(defaultJSONOptions, jsonv2.Deterministic(true))

// jsonOptions returns the application's response encoding profile, falling
// back to the framework default for a nil App (a Response built directly with
// [NewResponse], as tests do).
func (app *App) jsonOptions() jsonv2.Options {
	if app == nil || app.jsonOpts == nil {
		return defaultJSONOptions
	}
	return app.jsonOpts
}

// errorJSONOptions returns the encoding profile for framework error
// responses. Error bodies are a framework contract consumed by clients and
// tests, so deterministic map ordering is imposed on top of the application
// profile even when the application turned it off (later options win). The
// join happens once in [New]; every error response reads the stored value.
func (app *App) errorJSONOptions() jsonv2.Options {
	if app == nil || app.errorJSONOpts == nil {
		return defaultErrorJSONOptions
	}
	return app.errorJSONOpts
}
