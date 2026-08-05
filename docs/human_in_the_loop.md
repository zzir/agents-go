# Human-in-the-loop

Some tools should not run without a human's sign-off. Mark a tool as requiring approval and the run **pauses before executing it**: you get the pending calls back, record approve/reject decisions, and resume — in the same process or, via serialization, in a completely different one.

## Requiring approval

```go
deleteRepo := agents.NewTool("delete_repo", "Permanently delete a repository.",
	func(ctx context.Context, tc *agents.ToolContext, args deleteArgs) (string, error) {
		return doDelete(args.Name)
	})
deleteRepo.NeedsApproval = true
```

Or decide per call from the arguments:

```go
deleteRepo.NeedsApprovalFunc = func(ctx context.Context, rc *agents.RunContext, argsJSON, callID string) (bool, error) {
	return strings.Contains(argsJSON, `"prod"`), nil // only prod deletions need approval
}
```

## The interrupt → decide → resume loop

When the model requests an approval-gated tool, `Run` returns **without executing anything from that turn** — so nothing runs twice after resumption:

```go
res, err := agents.RunSync(ctx, agent, "delete the prod repo", opts)
if err != nil {
	log.Fatal(err)
}

for len(res.Interruptions) > 0 {
	for _, item := range res.Interruptions {
		fmt.Printf("approve %s(%s)? ", item.ToolName, item.Arguments)
		if askHuman() {
			res.State.Approve(item, false)
		} else {
			res.State.Reject(item, false, "denied by operator")
		}
	}
	res, err = agents.ResumeRunSync(ctx, res.State, opts)
	if err != nil {
		log.Fatal(err)
	}
}
fmt.Println(res.FinalOutputString())
```

- `RunResult.Interruptions` lists the pending calls (`ToolName`, `CallID`, raw `Arguments`).
- `RunResult.State` is the resumable `*RunState`; record decisions on it with `Approve` / `Reject`.
- `Reject`'s message (default: `"Tool execution was not approved."`) is sent to the model as the tool output so it can adapt.
- Pass `always=true` to `Approve`/`Reject` to apply the decision to **every future call of that tool** in the run.
- A resumed run can pause again (new approval-gated calls), hence the loop. The turn budget continues counting from where the run paused.
- Streams pause the same way: range to the end, read `Interruptions`/`State` off the `*RunCompletedEvent`'s result, and resume with `ResumeRun` (a stream) or `ResumeRunSync` (the result alone). The resumed stream does not re-emit the paused turn's own items — it picks up with the approved tools' outputs and every later turn.
- Once a call has an explicit approve/reject decision, resuming does **not** re-invoke `NeedsApprovalFunc` for it — the checker's side effects and errors cannot re-fire for an already-resolved call.

## Pre-approval guardrails

By default a tool's [input guardrails](guardrails.md#placement-decides-scope) run only after approval, right before execution. `RunOptions.Exec.PreApprovalToolInputGuardrails` also runs them **before** the approval interruption is surfaced:

```go
res, err := agents.RunSync(ctx, agent, input, agents.RunOptions{
	Exec: agents.ExecOptions{PreApprovalToolInputGuardrails: true},
	// ...
})
```

If a guardrail rejects the call, its message is returned to the model as the tool output — no approval request is emitted and the tool never runs, sparing the human a pointless round-trip. Calls that pass still re-run the same guardrails immediately before execution after approval, so time-sensitive checks are revalidated on resume.

## Approvals across processes

`RunState` serializes to JSON, so the approval can happen hours later in another process (a ticket queue, a Slack button, …):

```go
// Process A: pause and persist
data, _ := json.Marshal(res.State)
store.Save(runID, data)

// Process B: restore, decide, resume
state, err := agents.RunStateFromJSON(data, map[string]*agents.Agent{
	"assistant": assistant, // every agent that participated, by name
})
if err != nil { … }
state.Approve(state.Interruptions[0], false)
res, err := agents.ResumeRunSync(ctx, state, opts)
```

Because Go functions don't serialize, `RunStateFromJSON` needs a **registry** mapping agent names back to your `*Agent` values. The format round-trips within this SDK only; it is not an interchange format with any other agents SDK.

The state round-trips whole: input queued through `RunControl` before the pause, deferred tools already disclosed to the model, and the server-conversation cursor (`UsePreviousResponseID` / `ConversationID` deltas) all survive the JSON trip, so a cross-process resume behaves exactly like an in-process one.

The state also carries the original run's `MaxTurns`, so a run started with a raised budget (say 20) that pauses on turn 12 resumes under the same budget even in a fresh process — `ResumeRun` uses `opts.MaxTurns` when set, else the serialized budget, else the default. Note that on a resumed result, `NewItems` items carry their replayed input form rather than the original model item: `Kind` and `Display()` survive, `Raw` is nil.

## Sessions and approvals

When the run uses a [Session](sessions.md), the user input and every completed turn are already persisted by the time the run pauses; only the pending, output-less tool calls are held back (they would break replay) and saved together with their outputs once the resumed run continues. Pass the same `Session` in `ResumeRun`'s options.
