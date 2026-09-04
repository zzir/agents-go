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
	//
	// The logger's own handler sets the level floor. Most of what the SDK says
	// is Debug, so hand it a dedicated logger whose handler enables Debug —
	// slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level:
	// slog.LevelDebug})) — to see it without enabling Debug application-wide.
	Logger *slog.Logger

	// SensitiveData includes attributes marked with Sensitive: prompts, tool
	// arguments, tool results, model output.
	//
	// It is off by default and separate from the logger itself, so "log what
	// the SDK is doing" and "log what the user said" stay different decisions.
	// The one that leaks a conversation into a log aggregator has to be made on
	// purpose.
	SensitiveData bool
}

// sensitiveKey marks an attribute as carrying conversation content. It is a
// wrapper type rather than a naming convention because a convention gets
// broken silently and a type does not.
type sensitiveValue struct{ v any }

// LogValue implements slog.LogValuer so a sensitive attribute that reaches a
// handler WITHOUT going through the SDK's opt-in filter renders as a redaction
// marker, never the value. The only path that reveals the value is
// LogConfig.SensitiveData, which unwraps before the handler sees it — so
// passing a Sensitive attribute to your own slog.Logger is safe by default.
func (s sensitiveValue) LogValue() slog.Value { return slog.StringValue("«redacted»") }

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
}

// newRunLogger builds the logger for a run. A nil Logger yields one that does
// nothing, so call sites never have to check.
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

// enabled reports whether a record at this level would be emitted. Call sites
// that would have to build an expensive attribute check it first.
func (l *runLogger) enabled(ctx context.Context, level slog.Level) bool {
	return l != nil && l.log != nil && l.log.Enabled(ctx, level)
}

func (l *runLogger) emit(ctx context.Context, level slog.Level, msg string, attrs []slog.Attr) {
	if !l.enabled(ctx, level) {
		return
	}
	l.log.LogAttrs(ctx, level, msg, l.filter(attrs)...)
}

// filter drops sensitive attributes unless they were asked for, then unwraps them
// so a handler that ignores slog.LogValuer prints the value, not the wrapper.
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
