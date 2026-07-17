package docker

import (
	"context"
	"errors"
	"testing"

	"github.com/zzir/agents-go/sandbox"
)

func TestOpenTerminal_RequiresPersistent(t *testing.T) {
	// client.New(FromEnv) constructs without dialing, and the Persistent check
	// runs before any daemon call, so no Docker daemon is needed here.
	sb, err := New(Options{Image: "python:3.12-slim"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = sb.OpenTerminal(context.Background(), sandbox.TerminalOptions{})
	if !errors.Is(err, sandbox.ErrTerminalUnsupported) {
		t.Errorf("err = %v, want ErrTerminalUnsupported", err)
	}
}
