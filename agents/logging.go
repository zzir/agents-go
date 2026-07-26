package agents

import (
	"context"
	"log/slog"
)

// LogConfig controls the SDK's own structured logging.
//
// Logging is off by default. An SDK that logs to the process default the
// moment it is imported is one that shows up uninvited in somebody's
// production output; a caller who wants records asks for them.
type LogConfig struct {
	// Logger receives the records. Nil disables SDK logging entirely.
	Logger *slog.Logger

	// SensitiveData includes attributes marked with Sensitive: prompts, tool
	// arguments, tool results, model output.
	//
	// It is off by default and separate from the logger itself, so "log what
	// the SDK is doing" and "log what the user said" stay different decisions.
	// The one that leaks a conversation into a log aggregator has to be made on
	// purpose.
	SensitiveData bool

	// Level, when non-nil, overrides the logger's own minimum level for SDK
	// records. Most of what the SDK has to say is Debug, and a caller usually
	// wants that without turning Debug on for their whole application.
	Level *slog.Level
}

// sensitiveKey marks an attribute as carrying conversation content. It is a
// wrapper type rather than a naming convention because a convention gets
// broken silently and a type does not.
type sensitiveValue struct{ v any }

// LogValue implements slog.LogValuer so a sensitive attribute that somehow
// reaches an unfiltered handler still renders, rather than printing a Go
// struct.
func (s sensitiveValue) LogValue() slog.Value { return slog.AnyValue(s.v) }

// Sensitive marks a log attribute as conversation content, dropped unless
// LogConfig.SensitiveData is set.
//
//	log.Debug("calling tool", slog.String("tool", name), agents.Sensitive("arguments", argsJSON))
func Sensitive(key string, value any) slog.Attr {
	return slog.Any(key, sensitiveValue{value})
}

// runLogger is the SDK's internal logger: it tags every record with the
// component that emitted it and drops sensitive attributes unless they were
// asked for.
type runLogger struct {
	log       *slog.Logger
	sensitive bool
	level     slog.Level
	hasLevel  bool
}

// newRunLogger builds the logger for a run. A nil Logger yields one that does
// nothing, so call sites never have to check.
func newRunLogger(cfg LogConfig) *runLogger {
	if cfg.Logger == nil {
		return &runLogger{}
	}
	l := &runLogger{log: cfg.Logger, sensitive: cfg.SensitiveData}
	if cfg.Level != nil {
		l.level, l.hasLevel = *cfg.Level, true
	}
	return l
}

// component returns a logger tagged with the subsystem emitting the record, so
// a caller can filter the SDK's chatter by where it came from without every
// call site repeating the attribute.
func (l *runLogger) component(name string) *runLogger {
	if l == nil || l.log == nil {
		return l
	}
	c := *l
	c.log = l.log.With(slog.String("component", name))
	return &c
}

// with returns a logger carrying attrs on every subsequent record.
func (l *runLogger) with(attrs ...slog.Attr) *runLogger {
	if l == nil || l.log == nil || len(attrs) == 0 {
		return l
	}
	c := *l
	args := make([]any, 0, len(attrs))
	for _, a := range attrs {
		args = append(args, a)
	}
	c.log = l.log.With(args...)
	return &c
}

func (l *runLogger) Debug(ctx context.Context, msg string, attrs ...slog.Attr) {
	l.emit(ctx, slog.LevelDebug, msg, attrs)
}

func (l *runLogger) Info(ctx context.Context, msg string, attrs ...slog.Attr) {
	l.emit(ctx, slog.LevelInfo, msg, attrs)
}

func (l *runLogger) Warn(ctx context.Context, msg string, attrs ...slog.Attr) {
	l.emit(ctx, slog.LevelWarn, msg, attrs)
}

func (l *runLogger) Error(ctx context.Context, msg string, attrs ...slog.Attr) {
	l.emit(ctx, slog.LevelError, msg, attrs)
}

// enabled reports whether a record at this level would be emitted. Call sites
// that would have to build an expensive attribute check it first.
func (l *runLogger) enabled(ctx context.Context, level slog.Level) bool {
	if l == nil || l.log == nil {
		return false
	}
	if l.hasLevel && level < l.level {
		return false
	}
	return l.log.Enabled(ctx, level)
}

func (l *runLogger) emit(ctx context.Context, level slog.Level, msg string, attrs []slog.Attr) {
	if !l.enabled(ctx, level) {
		return
	}
	l.log.LogAttrs(ctx, level, msg, l.filter(attrs)...)
}

// filter drops sensitive attributes when they were not asked for, and unwraps
// them when they were.
//
// Unwrapping matters: a caller who opts in wants the argument JSON, not a
// wrapper type's Go representation, and a handler that does not know about
// slog.LogValuer would print the latter.
func (l *runLogger) filter(attrs []slog.Attr) []slog.Attr {
	kept := attrs[:0:0]
	for _, a := range attrs {
		s, isSensitive := a.Value.Any().(sensitiveValue)
		switch {
		case !isSensitive:
			kept = append(kept, a)
		case l.sensitive:
			kept = append(kept, slog.Any(a.Key, s.v))
		}
	}
	return kept
}
