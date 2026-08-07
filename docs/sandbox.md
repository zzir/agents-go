# Sandbox agents

The `sandbox` packages run **model-generated code** in an isolated environment and expose that capability to an agent as a tool. Unlike a provider-hosted "code interpreter", which runs on someone else's servers, these sandboxes run in *your* infrastructure under your controls.

```
agents.Agent ── CodeTool  ──► sandbox.Sandbox (interface)
             ── FileTools ──►   ├── sandbox.LocalSandbox      (dev only, no isolation)
                                ├── sandbox/docker.Sandbox    (ephemeral / persistent containers)
                                └── sandbox/ssh.Sandbox       (remote host over SSH)
```

The Docker and SSH backends are each a **separate Go module** (`sandbox/docker`, `sandbox/ssh`) so the core module stays dependency-light.

## Restricting what may run

`CodeToolConfig.Policy` filters commands **before** they reach a human or the
sandbox:

```go
sandbox.CodeTool(sb, sandbox.CodeToolConfig{
	Policy: sandbox.Policy{
		Allow: []string{`^git `, `^go (build|test)\b`},
		Deny:  []string{`git push`, `rm -rf`},
	},
})
```

Before approval, deliberately: a person asked to judge forty commands an hour
stops reading them, so what was never going to be allowed should not reach the
prompt. `Deny` is checked after `Allow`, so a deny always wins — "allow `git .*`,
deny `git push`" means what it looks like.

A refusal reaches the model as a **result**, naming the rule, not as an error.
Told only "not allowed" a model tries variations; told which rule stopped it, it
can ask for something else or explain why it cannot proceed.

The zero value allows everything, and a policy whose patterns do not compile
refuses everything — falling open would turn a configuration typo into no
protection at all, silently.

A policy filters approval noise; it is **not a security boundary**. A pattern
matches the text of a command, and a shell spells one command in unbounded
ways: denying `rm -rf` stops `rm -rf /` and steps aside for `rm -fr /`, for
`rm  -rf /` with a second space, and for `eval $(echo cm0gLXJm | base64 -d)`,
which is not the command until bash expands it. Naming a path fares no better —
a rule denying `rm -rf /home/alice` never sees `rm -rf $HOME`. Containment comes
from the sandbox the command executes in: choose a backend whose isolation you
trust.

## Persistent shells

By default each command runs in a fresh shell, which a model experiences as its
`cd` being ignored: it runs `cd build`, then `make`, and make runs in the wrong
place. The workaround it reaches for on its own — chaining everything into one
enormous `&&` line — is worse to read, worse to fail, and loses the output
boundaries.

```go
sandbox.CodeTool(sb, sandbox.CodeToolConfig{Sessions: true})
```

The model then passes a `session_id`, and that named shell is held open between
calls, so `cd`, exported variables, an activated virtualenv and a started
background process all survive.

Completion is detected with a **sentinel**: after each command the session
prints a random per-session token and the exit status, and output is read until
the token appears. There is no other reliable signal on a PTY — a prompt is
configurable, silence means nothing, and a command that prints nothing looks
exactly like one still running.

The token is random (a fixed one is one a command could print — `echo __DONE__`
would end the read early and hand back a truncated result) and is written to the
shell **in two halves**, as separate `printf` arguments. A PTY echoes its input,
so a command line carrying the whole token would come back in the output and be
indistinguishable from the real thing; the read would then stop one command
early, forever after. Only the output ever contains the halves joined.

A session that times out is **closed**, not reused: the command may still be
running, and its output arriving in the middle of the next one is worse than a
shell startup.

Requires a backend with interactive terminal support (Docker, SSH).

Off by default, because a held-open shell is a resource with a lifetime and a
caller that never closes one leaks it.

## Quickstart

```go
import (
	"github.com/zzir/agents-go/sandbox"
	docker "github.com/zzir/agents-go/sandbox/docker"
)

sb, err := docker.New(docker.Options{Image: "python:3.12-slim", Persistent: true})
if err != nil { … }
defer sb.Close()

// CodeTool runs shell commands; FileTools gives the model native read_file,
// write_file and list_files — no shell needed.
tools := []*agents.Tool{sandbox.CodeTool(sb, sandbox.CodeToolConfig{})}
tools = append(tools, sandbox.FileTools(sb, sandbox.FileToolConfig{})...)

agent := &agents.Agent{
	Name:         "data analyst",
	Instructions: agents.StaticInstructions("Write and run Python code to answer the question. Iterate until the output is correct."),
	Tools:        tools,
}
```

The model writes code, `CodeTool` executes it in the sandbox, and the combined `exit_code` / `stdout` / `stderr` go back to the model so it can fix its own mistakes. `FileTools` adds `read_file`, `write_file` and `list_files` — native file operations backed by the sandbox's `ReadFile`/`WriteFile`/`ListDir` methods, so the model can manipulate files without piping through shell commands. Execution failures (non-zero exit, timeouts) and malformed arguments are normal tool output the model can correct; *infrastructure* failures (daemon down, missing image) abort the run.

String arguments (`workdir`, `session_id`) decode leniently: a model running on a backend that does not enforce strict schemas sometimes fills an unused field with `0` or `null` instead of `""`, and those decode as `"0"` / `""` rather than failing the call.

## CodeTool configuration

`CodeTool` exposes one tool that runs a shell command (`bash -c <cmd>`) in the sandbox; the model picks the command, an optional `timeout_seconds` (capped at `MaxTimeout`) and an optional `workdir`.

```go
sandbox.CodeToolConfig{
	Name:           "run_python",       // default "exec_command"
	Description:    "Execute shell commands in a Python sandbox.",
	Timeout:        30 * time.Second,   // default per execution (sandbox.DefaultTimeout)
	MaxTimeout:     10 * time.Minute,   // cap for model-requested timeout_seconds
	MaxOutputBytes: 8192,               // per-stream truncation toward the model
}
```

## Backends

### Local (development only)

```go
sb := sandbox.NewLocal()
```

Runs commands directly on the host in a temp directory — **no isolation**. By default the child sees only `PATH`, `HOME` and `TMPDIR` (plus request env), so host secrets cannot leak into model code; `sandbox.NewLocalWithOptions(sandbox.LocalOptions{InheritHostEnv: true})` restores full inheritance. Timeouts kill the whole process group, including backgrounded grandchildren.

Set `WorkDir` to enable file operations (`ReadFile`/`WriteFile`/`ListDir`):

```go
sb := sandbox.NewLocalWithOptions(sandbox.LocalOptions{WorkDir: "/path/to/workspace"})
```

### Docker

```go
sb, err := docker.New(docker.Options{
	Image:   "python:3.12-slim",          // must be pulled already
	Network: false,                       // default: no network
	Limits:  sandbox.Limits{MemoryBytes: 256 << 20, CPUs: 0.5, PIDs: 128},
})
```

Each `Exec` creates a locked-down container and removes it afterwards: no network, read-only root filesystem, all capabilities dropped, `no-new-privileges`, runs as `nobody`, writable `work` dir and `/tmp` (tmpfs), memory/CPU/PID limits, hard timeout (container killed). The command runs as the container entrypoint verbatim — image `ENTRYPOINT`/`CMD` never interfere. Container stdout/stderr is additionally capped on the daemon side (`json-file` log driver, `max-size=10m`), so output floods cannot fill the host disk.

With `Persistent: true` a single container is reused across `Exec` calls (state and installed files survive between calls) and the root filesystem is writable. The default user is still `65534:65534` (nobody), which **cannot install packages**; set `UserUnset: true` to run as the image's default user, or set `User` explicitly. Timeouts are enforced per exec: when the deadline passes the attached connection is closed and the exec's process tree is killed best-effort (exec processes are tagged with an `AGENTS_SANDBOX_EXEC` environment marker and matched via `/proc`; a process that re-execs itself with a scrubbed environment can evade the sweep — the container's PID/memory limits are the backstop).

### SSH (remote host)

```go
import sshsb "github.com/zzir/agents-go/sandbox/ssh"

sb, err := sshsb.New(sshsb.Options{
	Addr: "dev-box:22",                                  // host[:port]; default port 22
	User: "sandbox",
	Auth: sshsb.AuthConfig{KeyFile: "~/.ssh/id_ed25519"}, // or Password / UseAgent / KeyBytes
})
```

Each `Exec` writes the request files to a fresh `/tmp/agents-sandbox-*` directory on the remote host via **SFTP**, runs the command in a new SSH session (`cd … && exec …`, every argument shell-quoted), and removes the directory afterwards. `Stdin` is supported; timeouts close the SSH session and return `TimedOut=true` with exit `-1` — whether the remote process actually dies is **best-effort** (it depends on the sshd implementation and configuration; the command may keep running on the remote host after a timeout).

Authentication methods are tried in order — SSH agent (`UseAgent`), private key (`KeyFile`/`KeyBytes`, optionally `Passphrase`), then `Password`. Host keys are verified against `~/.ssh/known_hosts` by default; `HostKey.InsecureIgnoreHostKey` disables this (dev/test only), and `HostKey.Callback`/`KnownHostsFile` customize it.

> ⚠️ **The SSH backend provides no isolation.** The command runs with the SSH user's full privileges and `sandbox.Limits` are **not** enforced (SSH has no cgroups). Point it at a disposable VM or an already-sandboxed host, never a machine you care about.

## FileTools configuration

```go
sandbox.FileToolConfig{
	Timeout:        10 * time.Second, // per file operation (default: sandbox.DefaultTimeout)
	MaxOutputBytes: 8192,             // truncation for read_file output
}
```

File operations require a **persistent working directory** (`WorkDir`). Backends without one (bare `sandbox.NewLocal()`, ephemeral Docker without `WorkDir`) return `sandbox.ErrNoWorkDir`.

**Path resolution follows shell semantics, the same view `exec_command` has** ([spec §5.14](spec.md)): a relative path resolves under the working directory, an absolute path is used as-is — the model learns real paths from `pwd`/`ls` output and both spellings reach the same file. The sandbox, not the working directory, is the isolation boundary; the file tools do not pretend to a narrower view than exec already has. The one exception is **docker bind-mount mode**, whose file operations run on the *host* side of the mount: they are confined to `WorkDir` via `os.Root`, absolute paths must lie under the in-container mount point `/workspace` (translated to the host directory), and anything else fails with `sandbox.ErrOutsideWorkDir` (rendered to the model as "outside the working directory").

`ReadFile` is size-capped on every backend: files larger than the backend's `MaxReadFileBytes` option (0 = `sandbox.DefaultMaxReadFileBytes`, 8 MiB) fail with `sandbox.ErrReadLimitExceeded` instead of being read into memory — model code cannot OOM the host by creating a huge file and reading it back. Errors returned to the model contain only the requested relative path and the error kind, never host or remote absolute paths.

## The Sandbox interface

Implement it to add your own backend (Firecracker, Kubernetes, remote runners, …):

```go
type Sandbox interface {
	Exec(ctx context.Context, req ExecRequest) (*ExecResult, error)
	ReadFile(ctx context.Context, path string) ([]byte, error)
	WriteFile(ctx context.Context, path string, content []byte) error
	// CreateExclusive atomically creates path, failing with fs.ErrExist if it
	// already exists; apply_patch's Add/Move rely on it to reject a clobber
	// race-free. Added in this version — a source-breaking change: external
	// backends must implement it (atomic O_EXCL / shell-noclobber create).
	CreateExclusive(ctx context.Context, path string, content []byte) error
	ListDir(ctx context.Context, path string) ([]DirEntry, error)
	RemoveFile(ctx context.Context, path string) error
	Rename(ctx context.Context, oldPath, newPath string) error
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

type DirEntry struct {
	Name  string `json:"name"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size"`
}
```

A backend that assembles `sh -c` command lines should pass every interpolated value — path, argument, environment entry — through `sandbox.ShellQuote`, the same helper the Docker and SSH backends use, so the escaping has one definition rather than one copy per backend.

## ExecStreamer (optional)

Backends can optionally implement `ExecStreamer` to stream command output as it arrives:

```go
type ExecStreamer interface {
	ExecStream(ctx context.Context, req ExecRequest, stdout, stderr io.Writer) (*ExecResult, error)
}
```

Output is written to the provided `io.Writer`s in real time; the returned `ExecResult` contains `ExitCode` and `TimedOut` but its `Stdout`/`Stderr` fields are empty (output went to the writers). All three built-in backends implement this interface.

## TerminalOpener (optional)

Backends can optionally implement `TerminalOpener` to host an interactive
shell with a PTY (this is what powers the web terminal in `agents-server`):

```go
type TerminalOpener interface {
	OpenTerminal(ctx context.Context, opts TerminalOptions) (Terminal, error)
}

type TerminalOptions struct {
	Cols, Rows int               // initial PTY size; 0 = 80x24
	Term       string            // TERM value; "" = "xterm-256color"
	Shell      []string          // nil = backend default (SSH: login shell; docker: bash if present, else sh)
	Env        map[string]string
}

type Terminal interface {
	io.ReadWriteCloser              // Read: PTY output (ANSI included, EOF on shell exit); Write: user input
	Resize(cols, rows int) error
	Wait() (int, error)             // exit code after EOF; -1 when unknown
}
```

The **SSH** backend always supports it (a new session with `RequestPty` on the
existing connection, one per terminal). The **docker** backend supports it only
in `Persistent` mode — an interactive shell needs a long-lived container to
attach to — and force-kills the shell's process tree on `Close`. The **local**
backend does not implement it: handing out a host shell is a deliberately
bigger grant than running individual commands, so it is excluded by design.
`OpenTerminal` returns an error wrapping `ErrTerminalUnsupported` when the
backend's current configuration cannot host a terminal. The context bounds
session establishment only; the returned `Terminal` lives until `Close`.

See [examples/sandbox](../examples/sandbox/main.go), [sandbox/docker/example](../sandbox/docker/example/main.go) and [sandbox/ssh/example](../sandbox/ssh/example/main.go) for runnable programs.
