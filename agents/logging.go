package agents

import (
	"context"
	"log/slog"
)

// LogConfig controls the SDK's own structured logging; off by default — see
// spec §2.11c.
type LogConfig struct {
	// Logger receives the records; nil disables SDK logging. Its handler sets
	// the level floor, and most of what the SDK says is Debug.
	Logger *slog.Logger

	// SensitiveData includes attributes marked with Sensitive (prompts, tool
	// arguments and results, model output). Off by default.
	SensitiveData bool
}

// sensitiveValue wraps an attribute carrying conversation content.
type sensitiveValue struct{ v any }

// LogValue renders a sensitive attribute that bypassed the SDK's filter as a
// redaction marker, never the value.
func (s sensitiveValue) LogValue() slog.Value { return slog.StringValue("«redacted»") }

// Sensitive marks a log attribute as conversation content, dropped unless
// LogConfig.SensitiveData is set.
//
//	log.Debug("calling tool", slog.String("tool", name), agents.Sensitive("arguments", argsJSON))
func Sensitive(key string, value any) slog.Attr {
	return slog.Any(key, sensitiveValue{value})
}

// runLogger tags every record with its component and filters sensitive attributes.
type runLogger struct {
	log       *slog.Logger
	sensitive bool
}

// newRunLogger builds the logger for a run; a nil Logger yields a no-op one.
func newRunLogger(cfg LogConfig) *runLogger {
	if cfg.Logger == nil {
		return &runLogger{}
	}
	return &runLogger{log: cfg.Logger, sensitive: cfg.SensitiveData}
}

// component returns a logger tagged with the subsystem emitting the record.
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

// enabled reports whether a record at this level would be emitted.
func (l *runLogger) enabled(ctx context.Context, level slog.Level) bool {
	return l != nil && l.log != nil && l.log.Enabled(ctx, level)
}

func (l *runLogger) emit(ctx context.Context, level slog.Level, msg string, attrs []slog.Attr) {
	if !l.enabled(ctx, level) {
		return
	}
	l.log.LogAttrs(ctx, level, msg, l.filter(attrs)...)
}

// filter drops sensitive attributes unless asked for, unwrapping the kept ones.
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
