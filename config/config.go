package config

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"
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
//
// A Config is safe for concurrent reads. [Config.Reload] replaces the whole
// tree atomically, so concurrent readers observe either the previous or the
// new snapshot, never a mix.
type Config struct {
	mu   sync.RWMutex // guards data; the tree itself is never mutated after load
	data map[string]any
	opts options
	src  source // how the tree was built, replayed by Reload
}

// source records the inputs [Load] or [LoadBytes] used, so that
// [Config.Reload] can replay the same pipeline.
type source struct {
	bytes  []byte // non-nil for LoadBytes: the embedded document
	format string // LoadBytes format
	env    string // effective CREDO_ENV resolved at first load; fixed thereafter
}

// options holds configuration for loading behavior.
type options struct {
	files    []string // config file candidates (default: config.json, config.yaml, config.yml)
	prefix   string   // env var prefix (default: "CREDO_")
	explicit bool     // true when WithFiles was called (missing files become errors)

	dotenvPath     string       // override .env file path (takes precedence over CREDO_ENV_FILE)
	dotenvOptional bool         // true: missing explicit .env is a warning, not an error
	noProcessEnv   bool         // true: ignore the process environment entirely (merge layer + bootstrap keys)
	noDotenv       bool         // true: never read a .env file
	strict         bool         // true: reject unknown keys and disable weak type coercion
	logger         *slog.Logger // load-time warnings; nil means slog.Default()
}

// validate rejects contradictory option combinations before any I/O.
func (o *options) validate() error {
	if o.noDotenv && o.dotenvPath != "" {
		return fmt.Errorf("conflicting options: WithoutDotenv and WithDotenvPath")
	}
	return nil
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
//
// An empty prefix does not disable the environment source — it removes the
// filter, so every process environment variable is merged into the tree.
// To disable the source entirely, use [WithoutProcessEnv].
func WithPrefix(prefix string) Option {
	return func(o *options) { o.prefix = prefix }
}

// WithoutProcessEnv disables the process environment as a configuration
// source. No environment variables are merged into the tree (regardless of
// [WithPrefix]), and the env-sourced bootstrap keys are ignored too: CREDO_ENV
// is not read from the process environment and CREDO_ENV_FILE does not
// influence .env resolution. [WithDotenvPath] still works, and a CREDO_ENV
// entry inside an applicable .env file still drives env-specific file
// derivation — combine with [WithoutDotenv] for fully hermetic loading where
// only the config files (or [LoadBytes] document) are read.
func WithoutProcessEnv() Option {
	return func(o *options) { o.noProcessEnv = true }
}

// WithoutDotenv disables the .env file as a configuration source. No .env
// file is read at all: the default ".env" is not probed, CREDO_ENV_FILE is
// not consulted, and no CREDO_ENV bootstrap value can come from a .env file.
// Combining it with an explicit [WithDotenvPath] is contradictory and makes
// [Load] (and [LoadBytes]) return an error.
func WithoutDotenv() Option {
	return func(o *options) { o.noDotenv = true }
}

// WithStrictDecoding makes every typed decode from this Config strict:
// unknown keys — keys present in the configuration that do not map to a field
// of the target — become errors (nested sections included), and weak type
// coercion is disabled, so a string "123" no longer decodes into an int and a
// string "true" no longer decodes into a bool. Two deliberate coercions are
// retained: strings still decode through [encoding.TextUnmarshaler]
// implementations, and duration strings such as "5s" still decode into
// [time.Duration]. Numeric kind conversions (a JSON number into an int or
// float field) are native decoding, not weak coercion, and keep working.
//
// The policy rides on the Config instance: it applies to [Config.Unmarshal],
// [Config.Get], [Config.MustGet], the [Staged] candidate produced by
// [Config.Stage] (and therefore to reload subscriber validation), and to the
// framework's own decode of the "server" section when the Config is passed to
// credo.New — an unknown key under "server" then fails app construction.
//
// Strict decoding is designed for typed sources (config files, [LoadBytes]
// documents). Environment variables and .env entries are always strings, so
// string-to-number and string-to-bool overrides on typed fields fail to
// decode under strict mode — pair it with [WithoutProcessEnv] and
// [WithoutDotenv], or keep the default weak decoding when such overrides are
// needed. Under strict mode every typed view must also be complete: a struct
// that decodes a section (including narrow reload subscribers) must cover all
// of that section's keys.
//
// Unknown-key errors report key paths and never configuration values; type
// mismatch errors may quote the offending value.
func WithStrictDecoding() Option {
	return func(o *options) { o.strict = true }
}

// WithDotenvPath overrides the .env file path. Takes precedence over
// the CREDO_ENV_FILE environment variable. Useful for binary-relative
// deployments where the working directory differs from the project root.
//
// By default, an explicit path must exist: if the file is missing,
// [Load] returns an error. To make a missing file non-fatal, combine
// with [WithDotenvOptional]. Combining with [WithoutDotenv] is
// contradictory and makes [Load] return an error.
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
	c.mu.Lock()
	defer c.mu.Unlock()
	mergeMaps(m, c.data)
}

// get returns the value at the given dotted key path and whether it is present.
// A key explicitly set to null returns (nil, true), distinct from a missing
// key's (nil, false) — matching lookup and Exists, so callers never mistake an
// explicit null for an absent key. Map values are deep-copied to prevent
// mutation of the config tree's internal state. An empty key returns the entire
// nested tree.
func (c *Config) get(key string) (any, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if key == "" {
		return copyMap(c.data), true
	}
	val, ok := lookup(c.data, key)
	if !ok {
		return nil, false
	}
	if m, ok := val.(map[string]any); ok {
		return copyMap(m), true
	}
	return val, true
}

// newDecoder creates a mapstructure decoder with Credo's standard settings.
//
// MapFieldName converts PascalCase struct field names to snake_case so that
// config keys like "max_open" automatically match fields like "MaxOpen"
// without explicit struct tags. Explicit "credo" tags always take precedence.
//
// Under [WithStrictDecoding] unknown keys are rejected and weak type coercion
// is off; the decode hooks (TextUnmarshaler, duration strings) stay active in
// both modes.
func (c *Config) newDecoder(dst any) (*mapstructure.Decoder, error) {
	return mapstructure.NewDecoder(&mapstructure.DecoderConfig{
		Result:           dst,
		WeaklyTypedInput: !c.opts.strict,
		ErrorUnused:      c.opts.strict,
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

// String returns a redacted, metadata-only description of the Config — the
// number of leaf keys, never any key names or values — so formatting a
// *Config with %v, %s, or %+v cannot leak secrets into logs or error
// messages. The methods are declared on *Config; formatting a dereferenced
// Config copy bypasses them (and copies a mutex, which go vet flags).
func (c *Config) String() string {
	if c == nil {
		return "config.Config(nil)"
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.data == nil {
		return "config.Config(uninitialized)"
	}
	return fmt.Sprintf("config.Config(%d keys, values redacted)", len(flatten(c.data)))
}

// GoString returns the same redacted description as [Config.String], so the
// %#v verb cannot dump the config tree either.
func (c *Config) GoString() string { return c.String() }

// LogValue implements [slog.LogValuer] with the same redacted description, so
// passing a *Config as an slog attribute value logs metadata only.
func (c *Config) LogValue() slog.Value { return slog.StringValue(c.String()) }

// initialized reports whether the Config holds a tree (built by Load or
// LoadBytes). Read under the lock so it cannot race a concurrent Reload swap.
func (c *Config) initialized() bool {
	if c == nil {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.data != nil
}

// Exists reports whether the given key path exists in the merged configuration.
// Dots in the key always act as path separators.
func (c *Config) Exists(key string) bool {
	if c == nil || key == "" {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
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
// By default, type coercion uses mapstructure's WeaklyTypedInput, which
// handles common conversions like string to int, string to bool, int to
// float64, etc. This is particularly useful when env vars (always strings)
// override typed YAML/JSON values. Under [WithStrictDecoding], weak coercion
// is off and unknown keys are rejected; see that option for the full policy.
//
// If dst implements Validate() error, validation is called automatically
// after a successful decode.
//
// A present but null value (YAML/JSON null) is a no-op: dst is left unchanged
// and no error is returned. A null overlays nothing — mirroring how a partial
// section overlays only the keys it contains — so it neither zeroes dst nor is
// misreported as missing (Exists still reports it as present). A null never
// resets dst: to clear a value, set it explicitly in the config or decode
// into a fresh zero-value target.
//
// Returns an error if the key does not exist or decoding fails.
func (c *Config) Unmarshal(key string, dst any) error {
	if !c.initialized() {
		return fmt.Errorf("config: instance not initialized")
	}
	val, ok := c.get(key)
	if !ok {
		return fmt.Errorf("config: key %q not found", key)
	}
	// A present but null value leaves dst unchanged: mapstructure treats a nil
	// input as "nothing to set" (ZeroFields is off). The key still Exists, so it
	// is not misreported as missing, and a null overlays nothing rather than
	// zeroing dst.

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

// Get decodes the value or sub-tree at the given dotted key path into a value
// of type T and returns it. It is the typed-snapshot form of [Config.Unmarshal]:
//
//	db, err := cfg.Get[DatabaseConfig]("database")
//	port, err := cfg.Get[int]("server.port")
//
// T may be a struct, map, slice, or primitive. The same rules as
// [Config.Unmarshal] apply: a missing key or decode failure returns an error,
// type coercion is weak by default and strict under [WithStrictDecoding], and
// a T implementing Validate() error is validated after decoding. On error the
// zero value of T is returned.
//
// Get is a bootstrap/composition-root helper. Prefer extracting typed config
// here and injecting it into services via DI over reading string keys inside
// business code.
func (c *Config) Get[T any](key string) (T, error) {
	var dst T
	if err := c.Unmarshal(key, &dst); err != nil {
		var zero T
		return zero, err
	}
	return dst, nil
}

// MustGet is like [Config.Get] but panics on error. It suits bootstrap and
// composition-root code where a missing or invalid required key should abort
// startup.
func (c *Config) MustGet[T any](key string) T {
	v, err := c.Get[T](key)
	if err != nil {
		panic(err)
	}
	return v
}
