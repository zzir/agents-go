package tasks

import (
	"context"
	"encoding/json"
)

// AgentResolver turns the agent name the model asked for into a Spec.
type AgentResolver func(ctx context.Context, parentSessionID, agentName string) (Spec, error)

// Spec is a resolved agent, opaque to the SDK.
type Spec struct {
	// Inherit is snapshotted onto Task.Inherit and handed to the Launcher verbatim.
	Inherit json.RawMessage
	// DisplayName names the agent in tool results and UI.
	DisplayName string
}

// Launcher starts a run and returns without waiting for it.
type Launcher func(ctx context.Context, req LaunchRequest) error

// LaunchRequest describes a task's run to start.
type LaunchRequest struct {
	// TaskID, Kind and State are the job's, as the Task carries them.
	TaskID    string
	Kind      string
	State     json.RawMessage
	RunID     string
	SessionID string
	Input     string
	Inherit   json.RawMessage
	// Retry marks a run started by Retry: Input is then the retry prompt, and a
	// host whose job carries its own stage instruction re-issues it with it.
	Retry bool
}

// StopOutcome is what a host did with a stop request — see spec §2.13.
type StopOutcome int

const (
	// StopUnknownRun means the host has no such run (not launched yet, or long
	// gone); nothing was cancelled.
	StopUnknownRun StopOutcome = iota
	// StopCancelled means this call cancelled the run.
	StopCancelled
	// StopAlreadyFinished means the run ended on its own; its outcome arrives
	// through OnRunFinished.
	StopAlreadyFinished
	// StopAfterTurn means the run stops after its current turn and reports
	// through OnRunFinished; only a graceful stop may answer so.
	StopAfterTurn
)

// Stopper cancels a running task; graceful lets the current turn finish. It
// must tolerate a run already ended. Optional: without one, Stop finalizes the
// row but cannot interrupt the run.
type Stopper func(ctx context.Context, runID string, graceful bool) (StopOutcome, error)
