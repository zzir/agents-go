package tasks

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
)

// newID mints an identifier for a task, run or session.
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("tasks: reading randomness: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// discardHandler drops every record, so a Manager without a logger costs
// nothing and no call site needs a nil check.
type discardHandler struct{}

func (discardHandler) Enabled(context.Context, slog.Level) bool  { return false }
func (discardHandler) Handle(context.Context, slog.Record) error { return nil }
func (h discardHandler) WithAttrs([]slog.Attr) slog.Handler      { return h }
func (h discardHandler) WithGroup(string) slog.Handler           { return h }
