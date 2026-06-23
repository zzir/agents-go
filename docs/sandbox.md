# Sandbox agents

The `sandbox` packages run **model-generated code** in an isolated environment and expose that capability to an agent as a tool. This is a Go-specific extension — the Python SDK's hosted "code interpreter" tool runs on OpenAI's servers, while these sandboxes run in *your* infrastructure under your controls.

```
agents.Agent ── CodeTool ──► sandbox.Sandbox (interface)
                               ├── sandbox.LocalSandbox      (dev only, no isolation)
                               ├── sandbox/docker.Sandbox    (ephemeral containers)
                               └── sandbox/ssh.Sandbox       (remote host over SSH)
```

The Docker and SSH backends are each a **separate Go module** (`sandbox/docker`, `sandbox/ssh`) so the core module stays dependency-light.

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

### SSH (remote host)

```go
import sshsb "github.com/zzir/agents-go/sandbox/ssh"

sb, err := sshsb.New(sshsb.Options{
	Addr: "dev-box:22",                                  // host[:port]; default port 22
	User: "sandbox",
	Auth: sshsb.AuthConfig{KeyFile: "~/.ssh/id_ed25519"}, // or Password / UseAgent / KeyBytes
})
```

Each `Exec` writes the request files to a fresh `/tmp/agents-sandbox-*` directory on the remote host via **SFTP**, runs the command in a new SSH session (`cd … && exec …`, every argument shell-quoted), and removes the directory afterwards. `Stdin` is supported; timeouts close the session, which terminates the remote process (`TimedOut=true`, exit `-1`).

Authentication methods are tried in order — SSH agent (`UseAgent`), private key (`KeyFile`/`KeyBytes`, optionally `Passphrase`), then `Password`. Host keys are verified against `~/.ssh/known_hosts` by default; `HostKey.InsecureIgnoreHostKey` disables this (dev/test only), and `HostKey.Callback`/`KnownHostsFile` customize it.

> ⚠️ **The SSH backend provides no isolation.** The command runs with the SSH user's full privileges and `sandbox.Limits` are **not** enforced (SSH has no cgroups). Point it at a disposable VM or an already-sandboxed host, never a machine you care about.

## The Sandbox interface

Implement it to add your own backend (Firecracker, Kubernetes, remote runners, …):

```go
type Sandbox interface {
	Exec(ctx context.Context, req ExecRequest) (*ExecResult, error)
	Close() error
}

type ExecRequest struct {
	Cmd            []string          // argv, run exactly as given
	Files          map[string]string // path (relative to the workdir) -> content
	Env            map[string]string
	Stdin          string            // local and SSH backends (docker rejects it)
	Timeout        time.Duration     // 0 = DefaultTimeout (30s)
	MaxOutputBytes int64             // per stream; 0 = DefaultMaxOutputBytes (1 MiB)
}

type ExecResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
	TimedOut bool
}
```

See [examples/sandbox](../examples/sandbox/main.go), [sandbox/docker/example](../sandbox/docker/example/main.go) and [sandbox/ssh/example](../sandbox/ssh/example/main.go) for runnable programs.
