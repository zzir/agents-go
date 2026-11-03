package sandboxes

import (
	"context"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/sandbox"
)

// BackgroundCommand is sandbox.CodeToolConfig.Background: what exec_command
// hands an approved background call to. nil leaves the flag off the tool.
type BackgroundCommand = func(ctx context.Context, tc *agents.ToolContext, cmd *sandbox.Command) (agents.ToolResult, error)
