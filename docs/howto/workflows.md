# Workflows — the workbench

A workflow is a fixed, ordered sequence of steps run on one session: each step
names the agent that runs it and the prompt that starts its turn, so plan →
exec → verify can be three agents on three models. An execution is a
background task (`kind: "workflow"`) whose runs are the steps
([invariant 29](../explanation/workbench-invariants.md)). The refusal rules,
the verdict parsing and the row shapes are in
[the wire surface](../reference/protocol.md#workflows--apiv1workflows); this
page is the doing.

## Define one

Sidebar → **Workflows** → Definitions → New. Or over the API:

```bash
curl -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{
    "name": "ship",
    "description": "Implement a change, test it, fix until green.",
    "steps": [
      {"name": "exec",  "agent_config_id": "<coder>",    "prompt": "Implement the brief."},
      {"name": "test",  "agent_config_id": "<tester>",   "prompt": "Run the tests and report.",
       "gate": {}, "on_success": "end", "on_failure": "fix"},
      {"name": "fix",   "agent_config_id": "<coder>",    "prompt": "Fix what the test step found.",
       "on_success": "test"},
      {"name": "deploy","agent_config_id": "<deployer>", "prompt": "Deploy.", "pause_before": true}
    ],
    "budget": {"max_steps": 12, "max_minutes": 30, "max_laps": 3}
  }' http://localhost:9527/api/v1/workflows
```

- `description` is required — it is what an agent matches a request against
  when `spawn_task` lists the workflows on offer.
- A step is an ordinary run on the execution's session: later steps read what
  earlier ones did, with no data plumbing between them.
- `on_success` / `on_failure` name a step or `end`; their empty defaults are
  the plain list. Naming an earlier step is how a sequence loops.
- `gate: {pass?, fail?}` makes a step's verdict choose the edge: end the
  output with `PASS` or `FAIL` (or the words you set), or answer in structured
  output. A gate that reports neither fails the execution — a check that
  forgot to report is a broken step, not a coin flip.
- `pause_before` holds the sequence until a person approves the step from the
  conversation that asked ([invariant 37](../explanation/workbench-invariants.md));
  rejecting cancels the execution.
- `compact_before` folds the transcript into a summary before the step runs,
  with the step's own agent's compaction settings.
- `budget` — `max_steps`, `max_tokens`, `max_minutes` (each `0` = no bound)
  and `max_laps` (`0` = 3): checked before every step launch and retry, never
  mid-run.

Steps carry stable ids the server assigns, so inserting one above another
renumbers nothing a run in flight is naming. Editing a workflow never steers
an execution already running: each snapshots its definition.

## Run it from a conversation

Three ways, all the same start:

- **You**: type `/workflow ship <brief>` in the composer (typing `/` offers
  the commands; arrow keys walk them), or **Run…** on the definition in the
  hub, into a conversation of your choice.
- **The model**: `spawn_task(workflow="ship", input=<brief>)` — the one tool
  that starts any background work; the workflow is a parameter, not a fifth
  tool. It asks after it with `task_status(task_id)`, which reports the step
  it is on (`progress: step 2/3 (verify)`).
- **The API**: `POST /workflows/:id/runs {session_id, input, project_id?}`.

The brief (`input`) leads the first step's turn and is what the sequence works
from — the execution runs off the conversation that asked
([invariant 30](../explanation/workbench-invariants.md)), so write what a
colleague picking up the job would need. A start nobody's run asked for
leaves a `workflow_started` note on the conversation; the result comes back as
a task notification, like any task's.

A step may use tools or hand off, but not spawn tasks or start another
workflow. A busy session, or one at its background-task cap
(`max_tasks_per_session`), refuses the start with `409`.

## Run it on a schedule or from a webhook

A **trigger** starts work with no conversation asking. Hub → Triggers → Add,
or:

```bash
curl -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"kind":"cron","schedule":"0 9 * * 1-5","target":"workflow","workflow_id":"<id>",
       "session_id":"<session>","brief":"Ship whatever is on the board.","enabled":true}' \
  http://localhost:9527/api/v1/triggers
```

- `kind: cron` takes five fields (minute, hour, day of month, month, day of
  week) or a descriptor — `@hourly`, `@daily`, `@every 30m` (no seconds
  field; `@every` no shorter than a minute). Schedules tick in the **server's
  local time zone**, which `GET /api/v1/server` reports as `timezone`; an
  enabled cron trigger reports its `next_fire_at`. Ticks missed while the
  process was down are not replayed.
- `target: workflow` (`workflow_id`) starts an execution into `session_id`;
  `target: agent` (`agent_config_id`) sends the brief as a message of that
  conversation instead, run by that agent — the scheduled question, its reply
  the next turn, with a `trigger_fired` note before it.
- `kind: webhook` fires on `POST /hooks/<trigger id>` (outside `/api/v1`, no
  token). The create response carries the `secret` **once** — the hub shows it
  in a box with a signing example; **Rotate secret** mints another and retires
  the old one at once. A delivery proves itself with two headers:

  ```bash
  TS=$(date +%s)
  BODY='{"event":"example"}'
  SIG=$(printf '%s.%s' "$TS" "$BODY" | openssl dgst -sha256 -hmac "$SECRET" | sed 's/^.* //')
  curl -X POST "$BASE/hooks/$TRIGGER_ID" -H "X-Timestamp: $TS" -H "X-Signature-256: $SIG" -d "$BODY"
  ```

  `X-Timestamp` is UNIX seconds within five minutes of the server's clock and
  `X-Signature-256` is hex HMAC-SHA256 of `timestamp + "." + body` under the
  secret; the body (64 KB at most) is appended to the brief as the payload.
  What is refused, and when a resend counts as a replay, is in
  [the wire surface](../reference/protocol.md#workflows--apiv1workflows).
- **Fire now** (`POST /triggers/:id/fire`, with an optional `payload`) runs a
  trigger by hand, as a tick would. Enable / disable, edit and delete are on
  the same row in the hub; a trigger's `last_error` says why the last fire
  started nothing.

Deleting the session or the workflow deletes its triggers; a deleted agent
leaves its triggers standing, failing with the reason, to be re-pointed.

## Watch it

- **Hub → Runs** lists every execution across your conversations, live; a row
  opens its conversation with the execution in the Inspector.
- In a conversation, the top bar's **Tasks** lens lists that session's
  background work; an execution opens to its steps — the launch log with how
  each step's run ended ([invariant 31](../explanation/workbench-invariants.md)),
  the brief, and the child session's transcript. A step waiting on
  `pause_before` is an approval card there and in the conversation.
- A failed execution can be retried (**Retry** on the row, `task_retry` from
  the model, `POST /tasks/:id/retry`): it re-runs the step it stopped at, on
  the same session, so the work already done is kept. Completed and cancelled
  executions do not retry — that would repeat their side effects. **Stop**
  cancels the current step's run and ends the execution.
- The trace panel labels the result's wake-up run by what started it
  (`▶ ship (you)`, `▶ ship (cron @daily)`).

## Let an agent write workflows

Off by default. On the agent (Settings → Agents → Behavior), turn on
**workflow authoring**; its chat runs then carry two tools:

- `get_workflow(name)` reads a definition.
- `save_workflow({name, description, steps: [{name, agent, prompt, gate,
  gate_pass, gate_fail, pause_before, compact_before, on_success,
  on_failure}], budget})` creates or updates one — agents and edges by NAME,
  never by id; the tool's description lists the agents on offer.

Every save is **approved first** — the approval card in the conversation is the
review, the definition drawn as in the hub and, on an update, diffed against
the stored one line by line
([invariant 39](../explanation/workbench-invariants.md)). Saving under an
existing name replaces that definition, and a saved workflow can be started in
the same turn. Who may save under which name is
[Authorization](../reference/protocol.md#authorization).
