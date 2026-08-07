package tasks

import (
	"context"
	"encoding/json"
)

// AgentResolver turns the agent name the model asked for into something
// runnable.
//
// It is injected because "what is an agent called and where does its
// configuration live" is the host's question, not the SDK's — a database row,
// a config file, a map in main().
type AgentResolver interface {
	Resolve(ctx context.Context, parentSessionID, agentName string) (Spec, error)
}

// AgentResolverFunc adapts a function to AgentResolver.
type AgentResolverFunc func(ctx context.Context, parentSessionID, agentName string) (Spec, error)

// Resolve implements AgentResolver.
func (f AgentResolverFunc) Resolve(ctx context.Context, parentSessionID, agentName string) (Spec, error) {
	return f(ctx, parentSessionID, agentName)
}

// Spec is a resolved agent, opaque to the SDK.
type Spec struct {
	// Inherit is snapshotted onto the task and handed back to the Launcher
	// verbatim, both for the task run and for the wake-up run later.
	Inherit json.RawMessage
	// DisplayName names the agent in tool results and UI.
	DisplayName string
}

// Launcher starts a run. The SDK does not care whether that is agents.Run
// directly or a request to a hub that owns run lifecycles.
type Launcher interface {
	// Launch starts the run and returns immediately. A task that blocked its
	// spawner would defeat the point.
	Launch(ctx context.Context, req LaunchRequest) error
}

// LauncherFunc adapts a function to Launcher.
type LauncherFunc func(ctx context.Context, req LaunchRequest) error

// Launch implements Launcher.
func (f LauncherFunc) Launch(ctx context.Context, req LaunchRequest) error { return f(ctx, req) }

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
}

// StopOutcome is what a host did with a stop request.
//
// "No error" is not enough to act on: a host asked to stop a run it has never
// heard of has nothing to report but success, and a Manager reading that as
// "the run will wind itself up" leaves a task running that it has just told
// someone was stopped. What the Manager does next depends on which of these
// happened, so the host says which.
type StopOutcome int

const (
	// StopUnknownRun means the host has no such run: it has not started yet —
	// a task claims its run before the host is told to launch it — or it is
	// long gone. Nothing was cancelled, so the ending is the Manager's to
	// record.
	StopUnknownRun StopOutcome = iota
	// StopCancelled means the run is not going to continue: it was cancelled,
	// or it had already finished. Either way nothing more will come from it.
	StopCancelled
	// StopAfterTurn means the run is still going and will stop at the end of
	// its current turn, reporting its own ending through OnRunFinished. Only a
	// graceful stop can be answered this way, and it is the one answer that
	// lets the Manager leave the terminal state to the run.
	StopAfterTurn
)

// Stopper cancels a running task. It is separate from Launcher because a host
// that only ever spawns need not implement it, and a Manager without one still
// finalizes a stopped task's row — it simply cannot interrupt the run.
type Stopper interface {
	// Stop cancels the run. graceful lets the current turn finish; only then
	// may StopAfterTurn be returned.
	Stop(ctx context.Context, runID string, graceful bool) (StopOutcome, error)
}

// StopperFunc adapts a function to Stopper.
type StopperFunc func(ctx context.Context, runID string, graceful bool) (StopOutcome, error)

// Stop implements Stopper.
func (f StopperFunc) Stop(ctx context.Context, runID string, graceful bool) (StopOutcome, error) {
	return f(ctx, runID, graceful)
}

// WakeGuard decides whether a parent session may be woken right now.
//
// An implementation MUST return false when it cannot answer — a failed query is
// "I cannot prove this is safe", and waking a session that turns out to be
// paused on a human decision races that human and burns a turn. Returning true
// on error would make every outage a source of spurious runs.
type WakeGuard interface {
	CanWake(ctx context.Context, parentSessionID string) bool
}

// WakeGuardFunc adapts a function to WakeGuard.
type WakeGuardFunc func(ctx context.Context, parentSessionID string) bool

// CanWake implements WakeGuard.
func (f WakeGuardFunc) CanWake(ctx context.Context, parentSessionID string) bool {
	return f(ctx, parentSessionID)
}

// AllGuards passes only when every guard does.
//
// The composition is AND rather than OR because each guard names a reason NOT
// to wake, and any one of them is sufficient. A nil guard is treated as a
// refusal for the same reason an errored one is: a guard that was supposed to
// be there and is not cannot be read as permission.
func AllGuards(gs ...WakeGuard) WakeGuard {
	return WakeGuardFunc(func(ctx context.Context, parentSessionID string) bool {
		for _, g := range gs {
			if g == nil || !g.CanWake(ctx, parentSessionID) {
				return false
			}
		}
		return true
	})
}
