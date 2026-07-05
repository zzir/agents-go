# Human-in-the-loop

Some tools should not run without a human's sign-off. Mark a tool as requiring approval and the run **pauses before executing it**: you get the pending calls back, record approve/reject decisions, and resume — in the same process or, via serialization, in a completely different one.

## Requiring approval

```go
deleteRepo := agents.NewFunctionTool("delete_repo", "Permanently delete a repository.",
	func(ctx context.Context, tc *agents.ToolContext, args deleteArgs) (string, error) {
		return doDelete(args.Name)
	})
deleteRepo.NeedsApproval = true
```

Or decide per call from the arguments:

```go
deleteRepo.NeedsApprovalFunc = func(ctx context.Context, rc *agents.RunContext, argsJSON string) (bool, error) {
	return strings.Contains(argsJSON, `"prod"`), nil // only prod deletions need approval
}
```

## The interrupt → decide → resume loop

When the model requests an approval-gated tool, `Run` returns **without executing anything from that turn** — so nothing runs twice after resumption:

```go
res, err := agents.Run(ctx, agent, "delete the prod repo", opts)
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
	res, err = agents.ResumeRun(ctx, res.State, opts)
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
- Streamed runs pause the same way: drain `Events()`, then check `FinalResult()` and resume with `ResumeRun`.

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
res, err := agents.ResumeRun(ctx, state, opts)
```

Because Go functions don't serialize, `RunStateFromJSON` needs a **registry** mapping agent names back to your `*Agent` values. The format round-trips Go↔Go only — it is not compatible with the Python SDK's `RunState` JSON.

The state also carries the original run's `MaxTurns`, so a run started with a raised budget (say 20) that pauses on turn 12 resumes under the same budget even in a fresh process — `ResumeRun` uses `opts.MaxTurns` when set, else the serialized budget, else the default. Note that on a resumed result, `NewItems` deserializes as generic raw items: `ItemType()` strings survive, but type assertions like `*MessageOutputItem` will not match.

## Sessions and approvals

When the run uses a [Session](sessions.md), nothing is saved at the interruption point; the user input and all generated items are persisted once the resumed run completes. Pass the same `Session` in `ResumeRun`'s options.
