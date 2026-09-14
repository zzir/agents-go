package tasks

import (
	"context"
	"encoding/json"
)

// AgentResolver turns the agent name the model asked for into something
// runnable. It is injected because where an agent's configuration lives is the
// host's question, not the SDK's.
type AgentResolver func(ctx context.Context, parentSessionID, agentName string) (Spec, error)

// Spec is a resolved agent, opaque to the SDK.
type Spec struct {
	// Inherit is snapshotted onto the task and handed back to the Launcher
	// verbatim, both for the task run and for the wake-up run later.
	Inherit json.RawMessage
	// DisplayName names the agent in tool results and UI.
	DisplayName string
}

// Launcher starts a run and returns immediately — a task that blocked its
// spawner would defeat the point. The SDK does not care whether that is
// agents.Run directly or a request to a hub that owns run lifecycles.
type Launcher func(ctx context.Context, req LaunchRequest) error

// LaunchRequest describes a task's run to start.
type LaunchRequest struct {
	// TaskID, Kind and State name the job this run belongs to, as the Task
	// carries them — a host launching a multi-run job reads State for where it
	// stands.
	TaskID    string
	Kind      string
	State     json.RawMessage
	RunID     string
	SessionID string
	Input     string
	Inherit   json.RawMessage
	// Retry marks a run started by Retry: Input is then the retry prompt, and a
	// host whose job carries its own instruction for the current stage (a
	// workflow step) re-issues that instruction with it.
	Retry bool
}

// StopOutcome is what a host did with a stop request; "no error" is not
// enough to act on, and the Manager's next step depends on it — spec §2.13.
type StopOutcome int

const (
	// StopUnknownRun means the host has no such run: not started yet (a task
	// claims its run before the host launches it), or long gone. Nothing was
	// cancelled, so the Manager records the ending.
	StopUnknownRun StopOutcome = iota
	// StopCancelled means this call cancelled the run.
	StopCancelled
	// StopAlreadyFinished means the run ended on its own before the stop
	// arrived, its outcome on its way through OnRunFinished; recording a
	// cancellation over it would bury a real outcome.
	StopAlreadyFinished
	// StopAfterTurn means the run will stop at the end of its current turn and
	// report its own ending through OnRunFinished. Only a graceful stop can be
	// answered this way.
	StopAfterTurn
)

// Stopper cancels a running task; graceful lets the current turn finish, and
// only then may StopAfterTurn be returned. It is optional — a Manager without
// one still finalizes a stopped task's row, but cannot interrupt the run.
type Stopper func(ctx context.Context, runID string, graceful bool) (StopOutcome, error)
