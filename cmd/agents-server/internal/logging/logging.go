// Package logging is the server's one logging seam: where records go, at what
// level and in what format, plus how a subsystem reaches the logger without a
// handle threaded through every call.
//
// The logger is a plain *slog.Logger. slog.Handler is already the swap point,
// so there is no interface here to replace it with — changing the destination
// is a different handler passed to New, and nothing else moves.
package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"
)

// New builds the process logger. level is one of debug/info/warn/error and
// format is text or json; both are compared case-insensitively, and an
// unrecognized value is an error rather than a silent fallback — a typo in a
// flag must not quietly turn logging down.
func New(w io.Writer, level, format string) (*slog.Logger, error) {
	var lv slog.Level
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		lv = slog.LevelDebug
	case "info":
		lv = slog.LevelInfo
	case "warn", "warning":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		return nil, fmt.Errorf("unknown log level %q (debug, info, warn, error)", level)
	}
	opts := &slog.HandlerOptions{Level: lv, ReplaceAttr: shortTime}
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "text":
		return slog.New(slog.NewTextHandler(w, opts)), nil
	case "json":
		return slog.New(slog.NewJSONHandler(w, opts)), nil
	default:
		return nil, fmt.Errorf("unknown log format %q (text, json)", format)
	}
}

// shortTime prints the wall clock a person reads a terminal by, not the date
// they already know. JSON keeps the full timestamp: it is parsed, not read.
func shortTime(groups []string, a slog.Attr) slog.Attr {
	if len(groups) == 0 && a.Key == slog.TimeKey {
		if t, ok := a.Value.Any().(time.Time); ok {
			return slog.String(slog.TimeKey, t.Format(time.RFC3339))
		}
	}
	return a
}

type ctxKey struct{}

// Into returns a context carrying l, for everything derived from it to find.
func Into(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxKey{}, l)
}

// Ctx returns the logger carried by ctx. A context with none yields one that
// discards, so no call site has to check — a subsystem reached from a context
// nobody wired (a test, a bare background goroutine) stays silent rather than
// panicking or writing somewhere unasked.
func Ctx(ctx context.Context) *slog.Logger {
	if ctx != nil {
		if l, ok := ctx.Value(ctxKey{}).(*slog.Logger); ok && l != nil {
			return l
		}
	}
	return discard
}

var discard = slog.New(slog.DiscardHandler)
