# Sandbox agents

The `sandbox` packages run **model-generated code** in an isolated environment and expose that capability to an agent as a tool. This is a Go-specific extension — the Python SDK's hosted "code interpreter" tool runs on OpenAI's servers, while these sandboxes run in *your* infrastructure under your controls.

```
agents.Agent ── CodeTool ──► sandbox.Sandbox (interface)
                               ├── sandbox.LocalSandbox      (dev only, no isolation)
                               ├── sandbox/docker.Sandbox    (ephemeral containers)
                               └── sandbox/k8s.Sandbox       (one Kubernetes Job per exec)
```

The Docker and Kubernetes backends are **separate Go modules** (`sandbox/docker`, `sandbox/k8s`) so the core module stays dependency-light.

## Quickstart

```go
import (
	"github.com/zzir/agents-go/sandbox"
	docker "github.com/zzir/agents-go/sandbox/docker"
)

sb, err := docker.New(docker.Options{Image: "python:3.12-slim"})
if err != nil { … }
defer sb.Close()

codeTool := sandbox.CodeTool(sb, sandbox.CodeToolConfig{})

agent := &agents.Agent{
	Name:         "data analyst",
	Instructions: agents.StaticInstructions("Write and run Python code to answer the question. Iterate until the output is correct."),
	Tools:        []agents.Tool{codeTool},
}
```

The model writes code, `CodeTool` executes it in the sandbox, and the combined `exit_code` / `stdout` / `stderr` go back to the model so it can fix its own mistakes. Execution failures (non-zero exit, timeouts) are normal tool output; *infrastructure* failures (daemon down, missing image) abort the run.

## CodeTool configuration

```go
sandbox.CodeToolConfig{
	Name:           "run_python",                    // default "run_code"
	Description:    "Execute Python in a sandbox.",
	Filename:       "main.py",                       // where the code is written
	RunCmd:         []string{"python", "main.py"},   // how it is executed
	Timeout:        30 * time.Second,                // per execution (sandbox.DefaultTimeout)
	MaxOutputBytes: 8192,                            // per-stream truncation toward the model
}
```

## Backends

### Local (development only)

```go
sb := sandbox.NewLocal()
```

Runs commands directly on the host in a temp directory — **no isolation**. By default the child sees only `PATH`, `HOME` and `TMPDIR` (plus request env), so host secrets cannot leak into model code; `sandbox.NewLocalWithOptions(sandbox.LocalOptions{InheritHostEnv: true})` restores full inheritance. Timeouts kill the whole process group, including backgrounded grandchildren.

### Docker

```go
sb, err := docker.New(docker.Options{
	Image:   "python:3.12-slim",          // must be pulled already
	Network: false,                       // default: no network
	Limits:  sandbox.Limits{MemoryBytes: 256 << 20, CPUs: 0.5, PIDs: 128},
})
```

Each `Exec` creates a locked-down container and removes it afterwards: no network, read-only root filesystem, all capabilities dropped, `no-new-privileges`, runs as `nobody`, writable `work` dir and `/tmp` (tmpfs), memory/CPU/PID limits, hard timeout (container killed). The command runs as the container entrypoint verbatim — image `ENTRYPOINT`/`CMD` never interfere.

### Kubernetes

```go
import k8s "github.com/zzir/agents-go/sandbox/k8s"

sb, err := k8s.New(k8s.Options{
	Image:        "python:3.12-slim",
	Namespace:    "sandbox",
	Limits:       sandbox.Limits{MemoryBytes: 256 << 20, CPUs: 0.5},
	StartTimeout: 2 * time.Minute, // image pull/scheduling budget, separate from exec timeout
})
```

Each `Exec` creates a ConfigMap (request files) plus a one-shot Job: non-root, read-only root fs, dropped capabilities, no service-account token, resource limits, TTL cleanup, and the ConfigMap owned by the Job so crashes leak nothing. Files are projected via per-file subPath mounts (nested paths and dotfiles work; no shell needed in the image). Pod logs return stdout+stderr combined; stdin is not supported.

> Network isolation on Kubernetes requires a deny-all NetworkPolicy (or sandboxed runtimeClass) in the namespace — the SDK cannot enforce it from the Job spec.

## The Sandbox interface

Implement it to add your own backend (Firecracker, remote runners, …):

```go
type Sandbox interface {
	Exec(ctx context.Context, req ExecRequest) (*ExecResult, error)
	Close() error
}

type ExecRequest struct {
	Cmd            []string          // argv, run exactly as given
	Files          map[string]string // path (relative to the workdir) -> content
	Env            map[string]string
	Stdin          string            // local backend only
	Timeout        time.Duration     // 0 = DefaultTimeout (30s)
	MaxOutputBytes int64             // per stream; 0 = DefaultMaxOutputBytes (1 MiB)
}

type ExecResult struct {
	ExitCode int
	Stdout   string
	Stderr   string // (k8s: combined into Stdout)
	TimedOut bool
}
```

See [examples/sandbox](../examples/sandbox/main.go), [sandbox/docker/example](../sandbox/docker/example/main.go) and [sandbox/k8s/example](../sandbox/k8s/example/main.go) for runnable programs.
