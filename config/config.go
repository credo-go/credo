package config

import (
	"fmt"
	"log/slog"
	"strings"
	"unicode"

	"github.com/go-viper/mapstructure/v2"
)

// keyDelim is the fixed key path delimiter used throughout the config package.
const keyDelim = "."

// RawConfig provides low-level access to the merged configuration.
// This is a bootstrap mechanism — application code should use typed config
// structs injected via DI instead of calling RawConfig directly.
//
// Unmarshal decodes both struct sections and primitive values:
//
//	var port int
//	rawCfg.Unmarshal("server.port", &port)
//
//	var dbCfg DatabaseConfig
//	rawCfg.Unmarshal("databases.default", &dbCfg)
type RawConfig interface {
	// Unmarshal decodes the value or sub-tree at the given dotted key path
	// into dst. dst must be a pointer (to a struct, map, slice, or primitive).
	// Returns an error if the key does not exist or decoding fails.
	Unmarshal(key string, dst any) error

	// Exists reports whether the given key path exists in the merged configuration.
	Exists(key string) bool
}

// Compile-time interface satisfaction check.
var _ RawConfig = (*Config)(nil)

// Config holds configuration loaded from files, .env, and environment
// variables, merged into a single nested map. Create with [Load] or
// [LoadBytes], then use [Config.Unmarshal] to extract typed values. Pass an
// empty key to decode the entire configuration tree.
type Config struct {
	data map[string]any
	opts options
}

// options holds configuration for loading behavior.
type options struct {
	files    []string // config file candidates (default: config.json, config.yaml, config.yml)
	prefix   string   // env var prefix (default: "CREDO_")
	explicit bool     // true when WithFiles was called (missing files become errors)

	dotenvPath     string       // override .env file path (takes precedence over CREDO_ENV_FILE)
	dotenvOptional bool         // true: missing explicit .env is a warning, not an error
	logger         *slog.Logger // load-time warnings; nil means slog.Default()
}

// Option configures the loading behavior of a Config instance.
type Option func(*options)

// WithFiles overrides the default config file discovery list.
// All found files are loaded and merged in order (later files override
// earlier ones for overlapping keys).
//
// Unlike the default discovery list, explicitly specified files are
// required: if none of the listed files exist, [Load] returns an error.
//
// When CREDO_ENV is set (via process env or .env file), env-specific files
// are derived from each listed file by inserting ".{env}" before the
// extension (e.g., "myapp.yaml" becomes "myapp.production.yaml"). Derived
// files are optional — missing derived files are silently skipped.
//
// Pass an empty list to explicitly disable file loading.
func WithFiles(files ...string) Option {
	return func(o *options) {
		o.files = files
		o.explicit = true
	}
}

// WithPrefix overrides the default environment variable prefix ("CREDO_").
func WithPrefix(prefix string) Option {
	return func(o *options) { o.prefix = prefix }
}

// WithDotenvPath overrides the .env file path. Takes precedence over
// the CREDO_ENV_FILE environment variable. Useful for binary-relative
// deployments where the working directory differs from the project root.
//
// By default, an explicit path must exist: if the file is missing,
// [Load] returns an error. To make a missing file non-fatal, combine
// with [WithDotenvOptional].
func WithDotenvPath(path string) Option {
	return func(o *options) { o.dotenvPath = path }
}

// WithDotenvOptional makes a missing explicit .env file a warning
// instead of an error. This only affects explicit paths set via
// [WithDotenvPath] or the CREDO_ENV_FILE environment variable.
// The default implicit ".env" is always optional regardless of this
// setting.
func WithDotenvOptional() Option {
	return func(o *options) { o.dotenvOptional = true }
}

// WithLogger sets the logger used for load-time warnings (such as a
// missing optional .env file). Defaults to [slog.Default].
func WithLogger(l *slog.Logger) Option {
	return func(o *options) { o.logger = l }
}

// newConfig creates an independent Config instance with the given options.
func newConfig(opts ...Option) *Config {
	o := options{
		files:  []string{"config.json", "config.yaml", "config.yml"},
		prefix: "CREDO_",
	}
	for _, opt := range opts {
		opt(&o)
	}
	return &Config{
		data: make(map[string]any),
		opts: o,
	}
}

// logger returns the configured load-time logger, defaulting to slog.Default.
func (c *Config) logger() *slog.Logger {
	if c.opts.logger != nil {
		return c.opts.logger
	}
	return slog.Default()
}

// merge incorporates a string-keyed map layer into the config tree; values in m
// override existing ones, maps merge recursively.
func (c *Config) merge(m map[string]any) {
	mergeMaps(m, c.data)
}

// get returns the value at the given dotted key path, or nil if not found.
// Map values are deep-copied to prevent mutation of the config tree's internal
// state. An empty key returns the entire nested tree.
func (c *Config) get(key string) any {
	if key == "" {
		return copyMap(c.data)
	}
	val, ok := lookup(c.data, key)
	if !ok || val == nil {
		return nil
	}
	if m, ok := val.(map[string]any); ok {
		return copyMap(m)
	}
	return val
}

// newDecoder creates a mapstructure decoder with Credo's standard settings.
//
// MapFieldName converts PascalCase struct field names to snake_case so that
// config keys like "max_open" automatically match fields like "MaxOpen"
// without explicit struct tags. Explicit "credo" tags always take precedence.
func (c *Config) newDecoder(dst any) (*mapstructure.Decoder, error) {
	return mapstructure.NewDecoder(&mapstructure.DecoderConfig{
		Result:           dst,
		WeaklyTypedInput: true,
		TagName:          "credo",
		MapFieldName:     toSnakeCase,
		DecodeHook: mapstructure.ComposeDecodeHookFunc(
			mapstructure.StringToTimeDurationHookFunc(),
			mapstructure.TextUnmarshallerHookFunc(),
		),
	})
}

// toSnakeCase converts a PascalCase or camelCase string to snake_case.
//
// Examples:
//
//	MaxOpen      → max_open
//	SSLMode      → ssl_mode
//	ReadTimeout  → read_timeout
//	APIKey       → api_key
//	HTMLParser   → html_parser
//	ID           → id
//	UserID       → user_id
func toSnakeCase(s string) string {
	runes := []rune(s)
	n := len(runes)
	var b strings.Builder
	b.Grow(n + n/3) // estimate: ~1 underscore per 3 chars

	for i, r := range runes {
		if unicode.IsUpper(r) {
			if i > 0 {
				prev := runes[i-1]
				if unicode.IsLower(prev) || unicode.IsDigit(prev) {
					// camelCase boundary: maxO → max_o
					b.WriteByte('_')
				} else if unicode.IsUpper(prev) && i+1 < n && unicode.IsLower(runes[i+1]) {
					// acronym boundary: SSLMode → ssl_m (S|M boundary)
					b.WriteByte('_')
				}
			}
			b.WriteRune(unicode.ToLower(r))
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Exists reports whether the given key path exists in the merged configuration.
// Dots in the key always act as path separators.
func (c *Config) Exists(key string) bool {
	if c == nil || key == "" {
		return false
	}
	_, ok := lookup(c.data, key)
	return ok
}

// Unmarshal decodes the value or sub-tree at the given dotted key path
// into dst. dst must be a pointer to a struct, map, slice, or primitive
// (e.g., *int, *string, *bool, *float64, *time.Duration).
//
// Pass an empty key ("") to decode the entire configuration tree into dst.
// Dots in the key always act as path separators.
//
// Type coercion uses mapstructure's WeaklyTypedInput, which handles common
// conversions like string to int, string to bool, int to float64, etc.
// This is particularly useful when env vars (always strings) override
// typed YAML/JSON values.
//
// If dst implements Validate() error, validation is called automatically
// after a successful decode.
//
// Returns an error if the key does not exist or decoding fails.
func (c *Config) Unmarshal(key string, dst any) error {
	if c == nil || c.data == nil {
		return fmt.Errorf("config: instance not initialized")
	}
	val := c.get(key)
	if val == nil {
		return fmt.Errorf("config: key %q not found", key)
	}
	// Guard against empty configuration for full-tree unmarshal. Without this,
	// mapstructure would silently decode an empty map into zero-value fields.
	if key == "" {
		if m, ok := val.(map[string]any); ok && len(m) == 0 {
			return fmt.Errorf("config: configuration is empty")
		}
	}
	dec, err := c.newDecoder(dst)
	if err != nil {
		return err
	}
	if err := dec.Decode(val); err != nil {
		return err
	}
	if v, ok := dst.(interface{ Validate() error }); ok {
		if err := v.Validate(); err != nil {
			return fmt.Errorf("config: validation: %w", err)
		}
	}
	return nil
}
