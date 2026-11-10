package config

import (
	"log/slog"
	"path/filepath"
	"slices"
)

// Sources names the inputs a [Config] was built from: the files the load
// found, the .env file it read and the process environment. It holds paths
// and the environment prefix, never a key or a value, so it is safe to log:
//
//	cfg, err := config.Load(config.WithFiles("app.yaml"))
//	...
//	logger.Info("configuration loaded", "sources", cfg.Sources())
//
// credo.New writes the same record for the configuration it loads by itself.
type Sources struct {
	// Files are the configuration files the load found and merged, in merge
	// order (a later file overrides an earlier one), as absolute paths. A
	// candidate that does not exist is not listed. Empty when no file was
	// found, and for a Config built with [LoadBytes].
	Files []string

	// Document is the format of the document a [LoadBytes] Config was built
	// from, as it was given ("json", "yaml"); empty for [Load].
	Document string

	// Dotenv is the absolute path of the .env file the load read; empty when
	// none was read, because it does not exist or [WithoutDotenv] was given.
	Dotenv string

	// ProcessEnv reports whether the process environment was a source; it is
	// false under [WithoutProcessEnv].
	ProcessEnv bool

	// EnvPrefix is the prefix of the environment variables the load merged
	// ("CREDO_" unless [WithPrefix] changed it). With ProcessEnv true, an
	// empty prefix means every variable was merged.
	EnvPrefix string
}

// Sources returns the inputs the Config's current tree was built from. A
// [Config.Reload] that publishes replaces them with what it read, so a file
// that appeared since the start is listed after it. The zero Sources is
// returned for a Config not built by [Load] or [LoadBytes].
func (c *Config) Sources() Sources {
	if c == nil || c.configState == nil {
		return Sources{}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	s := c.read
	s.Files = slices.Clone(s.Files)
	return s
}

// FromFile reports whether a file on disk was a source: a configuration file
// or a .env file. It is false when the load found only the process
// environment, or only the document it was handed.
func (s Sources) FromFile() bool {
	return len(s.Files) > 0 || s.Dotenv != ""
}

// LogValue implements [slog.LogValuer]: a group of files, dotenv and
// process_env, with document for a [LoadBytes] Config and env_prefix when the
// process environment was read. files is an empty list and dotenv an empty
// string when none was read, so the record always answers both.
func (s Sources) LogValue() slog.Value {
	files := s.Files
	if files == nil {
		files = []string{}
	}
	attrs := make([]slog.Attr, 0, 5)
	attrs = append(attrs, slog.Any("files", files))
	if s.Document != "" {
		attrs = append(attrs, slog.String("document", s.Document))
	}
	attrs = append(attrs, slog.String("dotenv", s.Dotenv), slog.Bool("process_env", s.ProcessEnv))
	if s.ProcessEnv {
		attrs = append(attrs, slog.String("env_prefix", s.EnvPrefix))
	}
	return slog.GroupValue(attrs...)
}

// absPath returns path as an absolute path, so a record of it names the
// working directory it was resolved against; path itself when the working
// directory cannot be determined.
func absPath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return abs
}
