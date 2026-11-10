package credo

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"

	"github.com/credo-go/credo/config"
)

// logSection is the framework's "log" section: the level and the format of
// the default logger. It is read as live, so a reload applies both keys, and
// only when no logger is given with [WithLogger].
const logSection config.Section[logConfig] = "log"

// logConfig is the "log" section. Both keys are optional and matched without
// regard to case, so an environment override such as CREDO_LOG__LEVEL=DEBUG
// reads as the file's "debug" does.
type logConfig struct {
	// Level is the lowest level the default logger writes: "debug", "info"
	// (the default), "warn" or "error".
	Level string `credo:"level"`

	// Format is the handler the default logger writes with: "text" (the
	// default) or "json".
	Format string `credo:"format"`
}

// Validate refuses a level or a format the section does not define. The
// configuration runs it on every decode, so a reload with an invalid value is
// aborted and the logger keeps what it had.
func (c logConfig) Validate() error {
	if _, err := c.level(); err != nil {
		return err
	}
	_, err := c.json()
	return err
}

// level is the slog level Level names.
func (c logConfig) level() (slog.Level, error) {
	switch strings.ToLower(c.Level) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("log.level %q is not a log level; use debug, info, warn or error", c.Level)
}

// json reports whether Format selects the JSON handler.
func (c logConfig) json() (bool, error) {
	switch strings.ToLower(c.Format) {
	case "", "text":
		return false, nil
	case "json":
		return true, nil
	}
	return false, fmt.Errorf("log.format %q is not a log format; use text or json", c.Format)
}

// logSectionRemedy ends the error of a "log" section that does not decode.
// The likeliest cause is an application section of the same name, so the
// error says whose section it is and both ways out.
const logSectionRemedy = `"log" is the framework's section, with the keys log.level and log.format: ` +
	"rename an application section of that name, or pass the logger with credo.WithLogger, which leaves the section unread"

// logSectionOutput is where the logger built from the "log" section writes:
// stderr, as the framework default logger does. Tests replace it.
var logSectionOutput io.Writer = os.Stderr

// resolveLogger returns the App's logger: the one given with [WithLogger],
// else one built from the "log" section when the configuration has it, else
// the framework default. With WithLogger the section is not read at all, as
// the TLS files are not read with WithTLSConfig.
//
// The section follows the "server" section's path: read through its
// descriptor when the configuration has the key, absent meaning the
// defaults. A *config.Config holds it as a live section, so a reload applies
// a new level and a new format; any other RawConfig cannot reload, and the
// section is decoded once.
func resolveLogger(o *appOptions) (*slog.Logger, error) {
	if o.logger != nil {
		return o.logger, nil
	}
	if !o.rawConfig.Exists(string(logSection)) {
		return defaultLogger, nil
	}
	if _, reloads := o.rawConfig.(*config.Config); !reloads {
		cfg, err := logSection.Decode(o.rawConfig)
		if err == nil {
			// A RawConfig of another type need not run Validate.
			err = cfg.Validate()
		}
		if err != nil {
			return nil, fmt.Errorf("credo: invalid log config: %w; %s", err, logSectionRemedy)
		}
		return slog.New(&sectionHandler{out: newLogOutput(logSectionOutput, cfg)}), nil
	}
	live, err := logSection.Live(o.rawConfig)
	if err != nil {
		return nil, fmt.Errorf("credo: invalid log config: %w; %s", err, logSectionRemedy)
	}
	out := newLogOutput(logSectionOutput, live.Load())
	live.OnChange(func(_ context.Context, _, next logConfig) error {
		out.apply(next)
		return nil
	})
	return slog.New(&sectionHandler{out: out}), nil
}

// logOutput is what every logger derived from the section's logger shares:
// the level, in a slog.LevelVar, and the handler the format selects, behind
// one atomic pointer. A reload sets the first and swaps the second.
type logOutput struct {
	w     io.Writer
	level slog.LevelVar
	base  atomic.Pointer[logBase]
}

// logBase is one generation of the output's handler.
type logBase struct {
	json    bool
	handler slog.Handler
}

// newLogOutput builds the output for cfg, which has been validated.
func newLogOutput(w io.Writer, cfg logConfig) *logOutput {
	o := &logOutput{w: w}
	o.apply(cfg)
	return o
}

// apply sets the level and, when the format changed, swaps the handler. cfg
// has been validated; a value that does not parse keeps what the output had.
func (o *logOutput) apply(cfg logConfig) {
	if level, err := cfg.level(); err == nil {
		o.level.Set(level)
	}
	json, err := cfg.json()
	if err != nil {
		return
	}
	if cur := o.base.Load(); cur != nil && cur.json == json {
		return
	}
	opts := &slog.HandlerOptions{Level: &o.level}
	base := &logBase{json: json, handler: slog.NewTextHandler(o.w, opts)}
	if json {
		base.handler = slog.NewJSONHandler(o.w, opts)
	}
	o.base.Store(base)
}

// sectionHandler is the slog.Handler of the logger built from the "log"
// section. It writes through the output's current handler, so a format
// change reaches every logger derived from it: a handler derived with
// WithAttrs or WithGroup keeps the steps that derived it and replays them on
// the new handler the first time it is used after a swap.
type sectionHandler struct {
	out *logOutput
	// steps are the WithAttrs and WithGroup calls that lead from the
	// output's handler to this one, in order. Never modified.
	steps []handlerStep
	// derived is the output's handler with steps applied, and the generation
	// it was derived from.
	derived atomic.Pointer[derivedHandler]
}

// handlerStep is one WithAttrs call (attrs) or one WithGroup call (group).
type handlerStep struct {
	attrs []slog.Attr
	group string
}

func (s handlerStep) apply(h slog.Handler) slog.Handler {
	if s.group != "" {
		return h.WithGroup(s.group)
	}
	return h.WithAttrs(s.attrs)
}

// derivedHandler is a sectionHandler's handler for one generation.
type derivedHandler struct {
	base    *logBase
	handler slog.Handler
}

// Enabled reports whether level is at or above the section's level.
func (h *sectionHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.out.level.Level()
}

// Handle writes the record with the output's current handler.
func (h *sectionHandler) Handle(ctx context.Context, r slog.Record) error {
	return h.current().handler.Handle(ctx, r)
}

// WithAttrs returns a handler whose records carry attrs.
func (h *sectionHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	return h.derive(handlerStep{attrs: attrs})
}

// WithGroup returns a handler whose later attributes are in the group name.
func (h *sectionHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return h.derive(handlerStep{group: name})
}

// derive returns the handler one step after h. The step is applied to h's
// current handler at once, so a With call costs what it costs slog's own
// handlers, and is replayed only after a format change.
func (h *sectionHandler) derive(step handlerStep) *sectionHandler {
	steps := make([]handlerStep, len(h.steps)+1)
	copy(steps, h.steps)
	steps[len(h.steps)] = step
	child := &sectionHandler{out: h.out, steps: steps}
	cur := h.current()
	child.derived.Store(&derivedHandler{base: cur.base, handler: step.apply(cur.handler)})
	return child
}

// current returns h's handler for the output's current generation, deriving
// it when the output's handler was swapped since h last wrote. Two goroutines
// that derive at once store equal handlers.
func (h *sectionHandler) current() *derivedHandler {
	base := h.out.base.Load()
	if d := h.derived.Load(); d != nil && d.base == base {
		return d
	}
	handler := base.handler
	for _, s := range h.steps {
		handler = s.apply(handler)
	}
	d := &derivedHandler{base: base, handler: handler}
	h.derived.Store(d)
	return d
}
