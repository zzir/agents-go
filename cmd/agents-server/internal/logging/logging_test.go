package logging_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/zzir/agents-go/cmd/agents-server/internal/logging"
)

// A typo in --log-level must not quietly turn logging down: a server that
// silently logs less than asked hides exactly what the flag was set for.
func TestNewRejectsUnknownLevelAndFormat(t *testing.T) {
	if _, err := logging.New(&bytes.Buffer{}, "verbose", "text"); err == nil {
		t.Error("an unknown level must be an error, not a default")
	}
	if _, err := logging.New(&bytes.Buffer{}, "info", "logfmt"); err == nil {
		t.Error("an unknown format must be an error, not a default")
	}
	if _, err := logging.New(&bytes.Buffer{}, "  WARN ", "JSON"); err != nil {
		t.Errorf("level and format are case- and space-insensitive: %v", err)
	}
}

func TestLevelFloorsOutput(t *testing.T) {
	var buf bytes.Buffer
	log, err := logging.New(&buf, "warn", "text")
	if err != nil {
		t.Fatal(err)
	}
	log.Debug("chatter")
	log.Info("routine")
	log.Warn("trouble")
	out := buf.String()
	if strings.Contains(out, "chatter") || strings.Contains(out, "routine") {
		t.Errorf("below the floor must not print: %q", out)
	}
	if !strings.Contains(out, "trouble") {
		t.Errorf("at the floor must print: %q", out)
	}
}

func TestJSONFormat(t *testing.T) {
	var buf bytes.Buffer
	log, err := logging.New(&buf, "info", "json")
	if err != nil {
		t.Fatal(err)
	}
	log.Info("started", "addr", "127.0.0.1:9527")
	out := buf.String()
	if !strings.HasPrefix(out, "{") || !strings.Contains(out, `"addr":"127.0.0.1:9527"`) {
		t.Errorf("json handler produced %q", out)
	}
}

// A context nobody wired must be silent, not a panic and not a write to
// somewhere the caller never asked for.
func TestCtxWithoutALoggerDiscards(t *testing.T) {
	if l := logging.Ctx(context.Background()); l == nil {
		t.Fatal("Ctx must never return nil")
	}
	logging.Ctx(context.Background()).Error("nowhere")
	//nolint:staticcheck // a nil context is exactly the case under test
	logging.Ctx(nil).Error("nowhere")
}

func TestIntoAndCtxRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	want := slog.New(slog.NewTextHandler(&buf, nil))
	ctx := logging.Into(context.Background(), want)
	if got := logging.Ctx(ctx); got != want {
		t.Fatal("Ctx must return the logger Into stored")
	}
	// And it survives a derived context, which is how every subsystem
	// downstream of the request or the root context finds it.
	sub, cancel := context.WithCancel(ctx)
	defer cancel()
	logging.Ctx(sub).Info("derived")
	if !strings.Contains(buf.String(), "derived") {
		t.Errorf("a derived context lost the logger: %q", buf.String())
	}
}
