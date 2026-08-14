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

// LaunchRequest describes a run to start.
type LaunchRequest struct {
	RunID     string
	SessionID string
	Input     string
	Inherit   json.RawMessage
	// Wake marks the parent's notification run rather than the task itself.
	// A host may treat the two differently — different tools, no task tools on
	// a task run — and cannot tell them apart otherwise.
	Wake bool
	// ParentRunID, on a Wake launch, is the run that spawned the task(s) being
	// delivered (the first one carrying it when several drained at once). It is
	// the run's LINEAGE, handed to the host at launch so it can be recorded on
	// the run's own durable output (traces) rather than re-derived later from
	// task rows or notification text — which a fork or a fold does not carry.
	ParentRunID string
}

// StopOutcome is what a host did with a stop request. "No error" is not enough
// to act on: a host asked to stop a run it never heard of can only report
// success, which a Manager must not read as "the run will wind itself up". What
// it does next depends on which outcome this is.
type StopOutcome int

const (
	// StopUnknownRun means the host has no such run: not started yet (a task
	// claims its run before the host launches it), or long gone. Nothing was
	// cancelled, so the Manager records the ending.
	StopUnknownRun StopOutcome = iota
	// StopCancelled means this call cancelled the run.
	StopCancelled
	// StopAlreadyFinished means the run had ended on its own before the stop
	// arrived, its outcome on its way through OnRunFinished. Separate from
	// StopCancelled because the ending is not this call's — recording a
	// cancellation over it would bury a real outcome, and cost a failure its retry.
	StopAlreadyFinished
	// StopAfterTurn means the run is still going and will stop at the end of
	// its current turn, reporting its own ending through OnRunFinished. Only a
	// graceful stop can be answered this way, and it is the one answer that
	// lets the Manager leave the terminal state to the run.
	StopAfterTurn
)

// Stopper cancels a running task; graceful lets the current turn finish, and
// only then may StopAfterTurn be returned. It is optional — a Manager without
// one still finalizes a stopped task's row, but cannot interrupt the run.
type Stopper func(ctx context.Context, runID string, graceful bool) (StopOutcome, error)
