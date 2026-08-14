package bridge

import (
	"context"
	"fmt"
	"strings"

	"github.com/rs/zerolog"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// StartWorkflowToolName is the tool an agent starts a sequence through.
const StartWorkflowToolName = "start_workflow"

type startWorkflowArgs struct {
	Name string `json:"name" jsonschema:"The name of the workflow to run, exactly as listed."`
	// The agent writes this, not the person: it read the conversation and the
	// workflow's session did not.
	Input string `json:"input" jsonschema:"A self-contained brief for the workflow: what to do and everything it needs to know. It runs in its own session and cannot see this conversation."`
}

// workflowTools is the agent's whole workflow surface, or nothing when this
// server has no workflows to offer.
func (r *Runner) workflowTools(ctx context.Context) []*agents.Tool {
	start := r.startWorkflowTool(ctx)
	if start == nil {
		return nil
	}
	// Paired deliberately: an agent that can start one has to be able to ask
	// what became of it, or it answers "is it done?" by doing the work again.
	return []*agents.Tool{start, r.workflowStatusTool()}
}

// startWorkflowTool offers the workflows this server has. It is built per run
// because the list changes without a restart, and it is absent entirely when
// there are none — an empty chooser is a tool that can only be called wrongly.
//
// It is the ONLY way a workflow starts. There is no button and no endpoint: the
// brief has to be written by something that read the conversation, and only the
// agent did.
func (r *Runner) startWorkflowTool(ctx context.Context) *agents.Tool {
	if r.Deps.Workflows == nil {
		return nil
	}
	offered, err := r.Deps.Workflows.List(ctx)
	if err != nil {
		zerolog.Ctx(ctx).Warn().Err(err).Msg("workflow: listing for the start tool")
		return nil
	}
	if len(offered) == 0 {
		return nil
	}
	var b strings.Builder
	b.WriteString("Run a predefined workflow: a fixed sequence of steps, each by a chosen agent, " +
		"executed in its own session in the background. It shares this session's working directory, " +
		"so its changes to files are real, but it cannot see this conversation — put everything it " +
		"needs in the input. You are told the result when it finishes. Available:\n")
	for _, w := range offered {
		fmt.Fprintf(&b, "- %s: %s\n", w.Name, w.Description)
	}

	return agents.NewTool(StartWorkflowToolName, b.String(),
		func(ctx context.Context, tc *agents.ToolContext, args startWorkflowArgs) (string, error) {
			sessionID, _ := tc.Context.(string)
			if sessionID == "" {
				return "No session to run a workflow for.", nil
			}
			want := strings.TrimSpace(args.Name)
			for i := range offered {
				if !strings.EqualFold(offered[i].Name, want) {
					continue
				}
				wr, err := r.StartWorkflow(ctx, offered[i].ID, sessionID, strings.TrimSpace(args.Input))
				if err != nil {
					// The refusal is the model's to read and relay — a budget
					// that is full or an agent that was deleted is something
					// the person can act on.
					return fmt.Sprintf("Could not start %q: %s", want, err.Error()), nil
				}
				return startedMessage(ctx, r, wr), nil
			}
			return fmt.Sprintf("No workflow named %q. Available: %s", want, workflowNames(offered)), nil
		})
}

// startedMessage tells the model what it just set going — including the one
// thing it cannot see and would otherwise find out about only from a failed
// step: whether this session has a working directory at all.
func startedMessage(ctx context.Context, r *Runner, wr *store.WorkflowRun) string {
	msg := fmt.Sprintf("Started %q (%d steps). It runs in the background; you will be told the result. "+
		"Do not repeat its work here.", wr.Name, len(wr.Steps))
	if sess, err := r.Deps.Sessions.Get(ctx, wr.ParentSessionID); err == nil && sess.SandboxID == "" {
		msg += " Note: this session has no sandbox bound, so the workflow has no file or command tools."
	}
	return msg
}

func workflowNames(list []store.Workflow) string {
	names := make([]string, 0, len(list))
	for i := range list {
		names = append(names, list[i].Name)
	}
	return strings.Join(names, ", ")
}

// WorkflowStatusToolName is how an agent asks what became of the sequences it
// started.
const WorkflowStatusToolName = "workflow_status"

// workflowStatusTool answers "is it done yet". The wake-up delivers the result
// eventually, but a person asks mid-flight — and an agent with no way to look
// concludes the workflow produced nothing and does the work over.
func (r *Runner) workflowStatusTool() *agents.Tool {
	return agents.NewTool(WorkflowStatusToolName,
		"Report the workflows running or finished for this conversation: which step each is on, "+
			"and the result of the ones that finished. Use it when asked whether a workflow is done.",
		func(ctx context.Context, tc *agents.ToolContext, _ struct{}) (string, error) {
			sessionID, _ := tc.Context.(string)
			if sessionID == "" || r.Deps.WorkflowRuns == nil {
				return "No workflows for this conversation.", nil
			}
			runs, err := r.Deps.WorkflowRuns.ListBySession(ctx, sessionID)
			if err != nil {
				// The model asked a question this cannot answer; telling it so
				// beats an error that ends the turn.
				return "Could not read the workflows: " + err.Error(), nil //nolint:nilerr
			}
			if len(runs) == 0 {
				return "No workflows have run for this conversation.", nil
			}
			return workflowStatusReport(runs), nil
		})
}

// statusRecentFinished is how many FINISHED executions the status report
// carries. Every live one is listed — the model must not redo work in flight —
// but a long conversation accumulates finished ones without limit, and a tool
// whose output grows forever eventually costs more than it says.
const statusRecentFinished = 3

// workflowStatusReport is the tool's answer: every live execution, the few most
// recent finished ones, and an honest count of what it left out.
func workflowStatusReport(runs []store.WorkflowRun) string {
	var b strings.Builder
	finished, omitted := 0, 0
	// runs arrive newest first, so the finished ones are kept in that order.
	for i := range runs {
		if runs[i].Status != store.WorkflowRunning {
			finished++
			if finished > statusRecentFinished {
				omitted++
				continue
			}
		}
		b.WriteString(workflowStatusLine(&runs[i]))
		b.WriteString("\n")
	}
	if omitted > 0 {
		fmt.Fprintf(&b, "(%d older finished workflow(s) not shown.)", omitted)
	}
	return strings.TrimRight(b.String(), "\n")
}

// workflowStatusLine is one execution, in one line the model can act on.
func workflowStatusLine(wr *store.WorkflowRun) string {
	idx := wr.StepIndex(wr.StepID)
	step := ""
	if idx >= 0 {
		step = fmt.Sprintf(" (step %d/%d", idx+1, len(wr.Steps))
		if name := wr.Steps[idx].Name; name != "" {
			step += ": " + name
		}
		step += ")"
	}
	line := fmt.Sprintf("%q is %s%s.", wr.Name, wr.Status, step)
	switch {
	case wr.Status == store.WorkflowRunning:
		line += " Still working — do not redo its work; you will be told when it finishes."
	case wr.Error != "":
		line += " It stopped with: " + wr.Error
	case wr.Result != "":
		line += "\n" + wr.Result
	}
	return line
}
