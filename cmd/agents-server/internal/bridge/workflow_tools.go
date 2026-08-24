package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/cmd/agents-server/internal/logging"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// The model's authoring surface for workflow DEFINITIONS: read one, save one.
// Running one stays spawn_task's (README invariant 30). The pair is opt-in per
// agent, chat-only, and every save is approved by a person — invariant 39.

// The pair's tool names.
const (
	WorkflowGetToolName  = "get_workflow"
	WorkflowSaveToolName = "save_workflow"
)

// workflowSpec is a definition as the model reads and writes it: steps, edges
// and agents by NAME, since the model has no ids. save_workflow takes it and
// get_workflow returns it, so a read can be edited and saved back.
type workflowSpec struct {
	Name        string         `json:"name" jsonschema:"The workflow's name. Saving under a name that exists replaces that definition (executions in flight keep their snapshot)"`
	Description string         `json:"description" jsonschema:"One line saying WHEN to run it: what spawn_task matches a request against"`
	Steps       []workflowStep `json:"steps" jsonschema:"The sequence, in order; at least one step"`
	Budget      workflowBudget `json:"budget" jsonschema:"Bounds every execution answers to; 0 = no bound (max_laps 0 = the default of 3)"`
}

type workflowStep struct {
	Name          string `json:"name" jsonschema:"Short unique step name; edges name steps by it (\"end\" is reserved)"`
	Agent         string `json:"agent" jsonschema:"Name of the agent that runs this step"`
	Prompt        string `json:"prompt" jsonschema:"What the step does, sent as the user turn that starts it. Earlier steps are already in the session: instruct, do not re-pass their output"`
	Gate          bool   `json:"gate" jsonschema:"true makes this a CHECK: its output ends with the pass or fail word, and the verdict picks the edge"`
	GatePass      string `json:"gate_pass" jsonschema:"The gate's pass word; empty = PASS. Only with gate true"`
	GateFail      string `json:"gate_fail" jsonschema:"The gate's fail word; empty = FAIL. Only with gate true"`
	PauseBefore   bool   `json:"pause_before" jsonschema:"true holds the sequence here until a person approves it"`
	CompactBefore bool   `json:"compact_before" jsonschema:"true folds the conversation into a summary before this step runs"`
	OnSuccess     string `json:"on_success" jsonschema:"Step name to run next on success (PASS for a gate); \"end\" stops there; empty = the next step in order, the last one ending"`
	OnFailure     string `json:"on_failure" jsonschema:"Step name to run on failure (FAIL for a gate); \"end\" stops there; empty fails the execution. Naming an earlier step is how a sequence loops"`
}

type workflowBudget struct {
	MaxSteps   int `json:"max_steps" jsonschema:"Step launches, retries included (at most 50)"`
	MaxTokens  int `json:"max_tokens" jsonschema:"Input plus output tokens of every model call on the execution's session"`
	MaxMinutes int `json:"max_minutes" jsonschema:"Minutes of step run time"`
	MaxLaps    int `json:"max_laps" jsonschema:"Times one backward edge may be taken; 0 = 3"`
}

type getWorkflowArgs struct {
	Name string `json:"name" jsonschema:"The workflow's name"`
}

// visibleWorkflows lists what ownerID may see (their own plus the global
// set); an empty ownerID — an internal caller with no user — sees everything.
func (r *Runner) visibleWorkflows(ctx context.Context, ownerID string) ([]store.Workflow, error) {
	if ownerID == "" {
		return r.Deps.Workflows.List(ctx)
	}
	return store.ListVisibleOf(ctx, r.Deps.Workflows.CrudStore, ownerID, false)
}

// matchWorkflow picks what name resolves to for ownerID: their own row over a
// global one sharing the name (spec §5.29). Nil when nothing matches.
func matchWorkflow(list []store.Workflow, ownerID, name string) *store.Workflow {
	var match *store.Workflow
	for i := range list {
		if !strings.EqualFold(list[i].Name, name) {
			continue
		}
		if list[i].OwnerID == ownerID && ownerID != "" {
			return &list[i]
		}
		if match == nil {
			match = &list[i]
		}
	}
	return match
}

// errInvalidWorkflowSpec marks a save the model can fix. A store fault is not
// one — that distinction decides whether a person is asked (see workflowTools).
var errInvalidWorkflowSpec = errors.New("invalid workflow")

func invalidWorkflowf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errInvalidWorkflowSpec, fmt.Sprintf(format, args...))
}

// workflowTools builds the run's authoring pair. Per run, like spawnTool: the
// save tool's description names the agents on offer, which change without a
// restart.
func (r *Runner) workflowTools(ctx context.Context, ownerID string) []*agents.Tool {
	get := agents.NewTool(WorkflowGetToolName,
		"Read a workflow definition by name, in the shape save_workflow takes. Read one before changing it.",
		func(ctx context.Context, _ *agents.ToolContext, args getWorkflowArgs) (agents.ToolResult, error) {
			return r.getWorkflow(ctx, ownerID, args.Name)
		})
	get.ReadOnly = true

	save := agents.NewTool(WorkflowSaveToolName, r.saveWorkflowDescription(ctx, ownerID),
		func(ctx context.Context, _ *agents.ToolContext, spec workflowSpec) (agents.ToolResult, error) {
			return r.saveWorkflow(ctx, ownerID, spec)
		})
	// Approval-gated always: the card IS the review, and the tool decides
	// that itself rather than an agent's approve list, which a config could
	// leave out. A proposal that would NOT save skips the person: the call
	// runs at once and the model reads why. Anything else — a store fault
	// included — asks, so no write ever lands unapproved.
	save.NeedsApproval = true
	save.NeedsApprovalFunc = func(ctx context.Context, _ *agents.RunContext, argsJSON, _ string) (bool, error) {
		var spec workflowSpec
		err := json.Unmarshal([]byte(argsJSON), &spec)
		if err == nil {
			_, _, err = r.resolveWorkflowSpec(ctx, ownerID, spec)
		} else {
			err = invalidWorkflowf("%s", err.Error()) // undecodable: the call fails on the model, writing nothing
		}
		return !errors.Is(err, errInvalidWorkflowSpec), nil
	}
	return []*agents.Tool{get, save}
}

// saveWorkflowDescription is the save tool's description: what a definition
// is, what a save does, and the agents a step may name.
func (r *Runner) saveWorkflowDescription(ctx context.Context, ownerID string) string {
	var b strings.Builder
	b.WriteString("Create or update a workflow definition: a fixed sequence of steps, each an ordinary turn by a named agent, " +
		"that spawn_task(workflow=<name>) runs in the background. Saving under a name that exists replaces that definition " +
		"(executions in flight keep their snapshot). Steps, agents and edges are named, not numbered. " +
		"Every save is shown to the person for approval before it is written; a definition that would not save is refused to you at once instead.")
	if r.Deps.AgentConfigs != nil {
		list, err := r.visibleAgentConfigs(ctx, ownerID)
		if err != nil {
			logging.Ctx(ctx).Warn("save_workflow: listing agents for the description", "error", err)
		}
		if len(list) > 0 {
			names := make([]string, 0, len(list))
			for i := range list {
				names = append(names, list[i].Name)
			}
			slices.Sort(names)
			b.WriteString("\n\nAgents available for steps: " + strings.Join(names, ", ") + ".")
		}
	}
	return b.String()
}

// getWorkflow answers get_workflow: the definition in the model's shape, or
// what there is to choose from.
func (r *Runner) getWorkflow(ctx context.Context, ownerID, name string) (agents.ToolResult, error) {
	if r.Deps.Workflows == nil {
		return agents.TextResult("Workflows are not available on this server."), nil
	}
	list, err := r.visibleWorkflows(ctx, ownerID)
	if err != nil {
		return agents.ToolResult{}, err
	}
	name = strings.TrimSpace(name)
	if name != "" {
		if wf := matchWorkflow(list, ownerID, name); wf != nil {
			spec := r.specOfWorkflow(ctx, wf)
			out, err := json.MarshalIndent(spec, "", "  ")
			if err != nil {
				return agents.ToolResult{}, err
			}
			return agents.TextResult(string(out)), nil
		}
	}
	switch {
	case len(list) == 0:
		return agents.TextResult("This server has no workflows yet; save_workflow creates one."), nil
	case name == "":
		return agents.TextResult("Name the workflow to read. Available: " + workflowNames(list)), nil
	}
	return agents.TextResult(fmt.Sprintf("No workflow named %q. Available: %s", name, workflowNames(list))), nil
}

// saveWorkflow answers save_workflow, after the approval: the same resolve
// the gate ran, then the write. A same-named definition created while the
// approval waited turns the create into an update of it.
func (r *Runner) saveWorkflow(ctx context.Context, ownerID string, spec workflowSpec) (agents.ToolResult, error) {
	wf, existing, err := r.resolveWorkflowSpec(ctx, ownerID, spec)
	if errors.Is(err, errInvalidWorkflowSpec) {
		return agents.TextResult("Nothing was saved: " + strings.TrimPrefix(err.Error(), errInvalidWorkflowSpec.Error()+": ") + "."), nil
	}
	if err != nil {
		return agents.ToolResult{}, err
	}
	if existing == nil {
		// A new definition is the saver's own (spec §5.29); an admin promotes
		// it over REST if the team should run it.
		wf.Scope, wf.OwnerID = store.ScopePrivate, ownerID
		err = r.Deps.Workflows.Create(ctx, wf)
		if _, dup := store.UniqueViolation(err); dup {
			if wf, existing, err = r.resolveWorkflowSpec(ctx, ownerID, spec); err == nil && existing == nil {
				err = errors.New("save_workflow: the name is taken but names no workflow")
			}
		}
	}
	if err == nil && existing != nil {
		// Editing a GLOBAL definition through the tool stays an admin's act —
		// the REST gate holds here too.
		if existing.Scope == store.ScopeGlobal && !ownerIsAdmin(ctx, r.Deps, ownerID) {
			return agents.TextResult(fmt.Sprintf("Nothing was saved: %q is a global workflow only an admin may change. Pick another name to save your own.", existing.Name)), nil
		}
		wf.Scope, wf.OwnerID = existing.Scope, existing.OwnerID
		err = r.Deps.Workflows.Update(ctx, existing.ID, wf)
	}
	if err != nil {
		return agents.ToolResult{}, err
	}
	r.auditWorkflowSave(ctx, ownerID, wf, existing == nil)
	details := map[string]any{"workflow_id": wf.ID, "created": existing == nil}
	if existing == nil {
		return agents.TextResult(fmt.Sprintf("Created workflow %q (%s). spawn_task(workflow=%q, input=...) can start it now.",
			wf.Name, stepCount(len(wf.Steps)), wf.Name)).
			WithSummary("Created · " + stepCount(len(wf.Steps))).
			WithDetails(details), nil
	}
	change := fmt.Sprintf("%d → %s", len(existing.Steps), stepCount(len(wf.Steps)))
	return agents.TextResult(fmt.Sprintf("Updated workflow %q (%s). Executions in flight keep the definition they started with; the next start uses this one.",
		wf.Name, change)).
		WithSummary("Updated · " + change).
		WithDetails(details), nil
}

// auditWorkflowSave writes the audit line for a save_workflow: the one
// write to shared configuration that happens through a tool rather than a
// request, attributed to the session's owner, who approved it.
func (r *Runner) auditWorkflowSave(ctx context.Context, ownerID string, wf *store.Workflow, isCreate bool) {
	detail := "tool=save_workflow updated"
	if isCreate {
		detail = "tool=save_workflow created"
	}
	r.auditAs(ctx, ownerID, "workflow.save", wf.ID, detail)
}

// auditAs records an act the server performed on a session owner's behalf —
// a tool's write, a trigger's fire — attributed to that owner. Nothing is
// recorded without an audit sink.
func (r *Runner) auditAs(ctx context.Context, ownerID, action, resource, detail string) {
	if r.Deps.Audit == nil {
		return
	}
	actor := protocol.UserInfo{ID: ownerID}
	if r.Deps.Users != nil && ownerID != "" {
		if u, err := r.Deps.Users.ByID(ctx, ownerID); err == nil {
			actor = protocol.UserInfo{ID: u.ID, Email: u.Email, Role: u.Role}
		}
	}
	r.Deps.Audit(context.WithoutCancel(ctx), protocol.AuditRecord{
		Actor: actor, Action: action, Resource: resource, Detail: detail,
	})
}

func stepCount(n int) string {
	if n == 1 {
		return "1 step"
	}
	return fmt.Sprintf("%d steps", n)
}

// resolveWorkflowSpec turns the model's spec into a stored definition: agents
// and edges named become ids, and on an update every step that keeps its name
// keeps its id (what a retry and an execution in flight name). existing is the
// definition the name already denotes, nil for a new one. A fixable problem
// is an errInvalidWorkflowSpec; anything else is a store fault.
func (r *Runner) resolveWorkflowSpec(ctx context.Context, ownerID string, spec workflowSpec) (wf, existing *store.Workflow, err error) {
	if r.Deps.Workflows == nil {
		return nil, nil, errors.New("workflows are not available on this server")
	}
	name := strings.TrimSpace(spec.Name)
	if name == "" {
		return nil, nil, invalidWorkflowf("name is required")
	}
	list, err := r.visibleWorkflows(ctx, ownerID)
	if err != nil {
		return nil, nil, err
	}
	existing = matchWorkflow(list, ownerID, name)

	// The names the existing steps go by — a nameless one by its position, as
	// get_workflow reported it — so a read saved back keeps every id.
	var existingNames []string
	if existing != nil {
		existingNames = stepNames(existing.Steps)
	}
	ids := make(map[string]string, len(spec.Steps)) // lower-cased step name → id
	steps := make(store.WorkflowSteps, 0, len(spec.Steps))
	for i, s := range spec.Steps {
		stepName := strings.TrimSpace(s.Name)
		if stepName == "" {
			return nil, nil, invalidWorkflowf("step %d: name is required", i+1)
		}
		if strings.EqualFold(stepName, store.WorkflowStepEnd) {
			return nil, nil, invalidWorkflowf("step %d: %q is reserved for an edge that stops there", i+1, store.WorkflowStepEnd)
		}
		key := strings.ToLower(stepName)
		if _, dup := ids[key]; dup {
			return nil, nil, invalidWorkflowf("step %d: duplicate step name %q", i+1, stepName)
		}
		id := ""
		for j, en := range existingNames {
			if strings.EqualFold(en, stepName) {
				id = existing.Steps[j].ID
				break
			}
		}
		if id == "" {
			id = store.NewID()
		}
		ids[key] = id

		agentName := strings.TrimSpace(s.Agent)
		if agentName == "" {
			return nil, nil, invalidWorkflowf("step %d (%s): agent is required", i+1, stepName)
		}
		ac, aerr := r.agentConfigByName(ctx, ownerID, agentName)
		if errors.Is(aerr, errNoSuchAgent) {
			return nil, nil, invalidWorkflowf("step %d (%s): %s", i+1, stepName, aerr.Error())
		}
		if aerr != nil {
			return nil, nil, aerr
		}
		if !s.Gate && (strings.TrimSpace(s.GatePass) != "" || strings.TrimSpace(s.GateFail) != "") {
			return nil, nil, invalidWorkflowf("step %d (%s): gate_pass / gate_fail need gate true", i+1, stepName)
		}
		step := store.WorkflowStep{
			ID: id, Name: stepName, AgentConfigID: ac.ID, Prompt: s.Prompt,
			CompactBefore: s.CompactBefore, PauseBefore: s.PauseBefore,
		}
		if s.Gate {
			step.Gate = &store.StepGate{Pass: s.GatePass, Fail: s.GateFail}
		}
		steps = append(steps, step)
	}
	// Edges after every id is known, so one may name a step in either direction.
	for i := range spec.Steps {
		for _, e := range []struct {
			field, target string
			dst           *string
		}{
			{"on_success", spec.Steps[i].OnSuccess, &steps[i].OnSuccess},
			{"on_failure", spec.Steps[i].OnFailure, &steps[i].OnFailure},
		} {
			target := strings.TrimSpace(e.target)
			switch {
			case target == "":
			case strings.EqualFold(target, store.WorkflowStepEnd):
				*e.dst = store.WorkflowStepEnd
			default:
				id, ok := ids[strings.ToLower(target)]
				if !ok {
					return nil, nil, invalidWorkflowf("step %d (%s): %s names %q, which is not a step of this workflow", i+1, steps[i].Name, e.field, target)
				}
				*e.dst = id
			}
		}
	}

	wf = &store.Workflow{
		Name:        name,
		Description: spec.Description,
		Steps:       steps,
		Budget: store.WorkflowBudget{
			MaxSteps: spec.Budget.MaxSteps, MaxTokens: spec.Budget.MaxTokens,
			MaxMinutes: spec.Budget.MaxMinutes, MaxLaps: spec.Budget.MaxLaps,
		},
	}
	if existing != nil {
		wf.ID = existing.ID
	}
	if err := store.NormalizeWorkflow(wf); err != nil {
		return nil, nil, invalidWorkflowf("%s", err.Error())
	}
	return wf, existing, nil
}

// specOfWorkflow is a stored definition in the model's shape: ids become
// names. A step with no name is called by its position ("Step 2"), which is
// what the hub shows and what a save round-trip then stores; an agent that no
// longer exists keeps its id, so saving it back fails loudly rather than
// silently re-pointing the step.
func (r *Runner) specOfWorkflow(ctx context.Context, wf *store.Workflow) workflowSpec {
	names := stepNames(wf.Steps)
	byID := make(map[string]string, len(wf.Steps))
	for i, s := range wf.Steps {
		byID[s.ID] = names[i]
	}
	edge := func(target string) string {
		if n, ok := byID[target]; ok {
			return n
		}
		return target // "" or end
	}
	agentNames := map[string]string{}
	spec := workflowSpec{
		Name: wf.Name, Description: wf.Description,
		Steps: make([]workflowStep, 0, len(wf.Steps)),
		Budget: workflowBudget{
			MaxSteps: wf.Budget.MaxSteps, MaxTokens: wf.Budget.MaxTokens,
			MaxMinutes: wf.Budget.MaxMinutes, MaxLaps: wf.Budget.MaxLaps,
		},
	}
	for i, s := range wf.Steps {
		agent, ok := agentNames[s.AgentConfigID]
		if !ok {
			agent = s.AgentConfigID
			if ac, err := r.Deps.AgentConfigs.Get(ctx, s.AgentConfigID); err == nil {
				agent = ac.Name
			}
			agentNames[s.AgentConfigID] = agent
		}
		step := workflowStep{
			Name: names[i], Agent: agent, Prompt: s.Prompt,
			PauseBefore: s.PauseBefore, CompactBefore: s.CompactBefore,
			OnSuccess: edge(s.OnSuccess), OnFailure: edge(s.OnFailure),
		}
		if s.Gate != nil {
			step.Gate = true
			step.GatePass, step.GateFail = s.Gate.Pass, s.Gate.Fail
		}
		spec.Steps = append(spec.Steps, step)
	}
	return spec
}

// stepNames names every step of a stored definition, uniquely: its own name,
// or "Step N" for a nameless one (suffixed if that collides with a name).
func stepNames(steps store.WorkflowSteps) []string {
	used := make(map[string]bool, len(steps))
	names := make([]string, len(steps))
	for i, s := range steps {
		if n := strings.TrimSpace(s.Name); n != "" {
			names[i] = n
			used[strings.ToLower(n)] = true
		}
	}
	for i := range steps {
		if names[i] != "" {
			continue
		}
		n := fmt.Sprintf("Step %d", i+1)
		for k := 2; used[strings.ToLower(n)]; k++ {
			n = fmt.Sprintf("Step %d (%d)", i+1, k)
		}
		names[i] = n
		used[strings.ToLower(n)] = true
	}
	return names
}
