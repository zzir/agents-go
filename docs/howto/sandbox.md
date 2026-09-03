# Sandbox agents

The `sandbox` packages run **model-generated code** in an isolated environment and expose that capability to an agent as a tool. Unlike a provider-hosted "code interpreter", which runs on someone else's servers, these sandboxes run in *your* infrastructure under your controls.

```
agents.Agent ── CodeTool  ──► sandbox.Sandbox (interface)
             ── FileTools ──►   ├── sandbox.LocalSandbox      (dev only, no isolation)
                                ├── sandbox/docker.Sandbox    (ephemeral / persistent containers,
                                │                              local daemon or remote over SSH)
                                └── sandbox/e2b.Sandbox       (E2B API: E2B's cloud, self-hosted,
                                                               or a compatible service)
```

The Docker backend is a **separate Go module** (`sandbox/docker`) so the core module stays dependency-light; the E2B backend needs only the standard library and lives in the root module.

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

`Deny` is checked after `Allow`, so a deny always wins; a refusal reaches the
model as a result naming the rule; the zero value allows everything and a
policy whose patterns do not compile refuses everything. A policy filters
approval noise and is **not a security boundary** — it matches command text,
not shell semantics; containment comes from the backend
([spec §2.7j](../reference/spec.md#27j-sandbox-command-policy)).

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
background process all survive. The `session_id` argument exists only when
`Sessions` is on. Completion is detected with a random per-session sentinel, a
timed-out session is closed rather than reused, and the named shells belong to
the tool, not the run ([spec §2.7k](../reference/spec.md#27k-persistent-shells)).

Requires a backend with interactive terminal support (persistent Docker, e2b).
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
// write_file and list_files — no shell needed; ApplyPatchTool adds apply_patch.
tools := []*agents.Tool{sandbox.CodeTool(sb, sandbox.CodeToolConfig{})}
tools = append(tools, sandbox.FileTools(sb, sandbox.FileToolConfig{})...)
tools = append(tools, sandbox.ApplyPatchTool(sb, sandbox.FileToolConfig{}))

agent := &agents.Agent{
	Name:         "data analyst",
	Instructions: agents.StaticInstructions("Write and run Python code to answer the question. Iterate until the output is correct."),
	Tools:        tools,
}
```

The model writes code, `CodeTool` executes it in the sandbox, and the combined `exit_code` / `stdout` / `stderr` go back to the model so it can fix its own mistakes. `FileTools` adds `read_file`, `write_file` and `list_files` — native file operations backed by the sandbox's `ReadFile`/`WriteFile`/`ListDir` methods, so the model can manipulate files without piping through shell commands. Execution failures (non-zero exit, timeouts) and malformed arguments are normal tool output the model can correct; *infrastructure* failures (daemon down, missing image) abort the run.

The optional string arguments (`workdir`, `session_id`) accept the zero-value sentinels `null`, `0` and `false` as "unused"; any other non-string scalar is refused as correctable text ([spec §2.7l](../reference/spec.md#27l-sandbox-tool-argument-decoding)).

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
	Image:   "python:3.12-slim",          // pulled on first use when absent
	Network: "",                          // "" = no network; else the docker network name
	Limits:  sandbox.Limits{MemoryBytes: 256 << 20, CPUs: 0.5, PIDs: 128},
	Env:     map[string]string{"TZ": "UTC"}, // set on the container itself
})
```

Each `Exec` creates a locked-down container and removes it afterwards: read-only root filesystem, all capabilities dropped, `no-new-privileges`, writable `work` dir and `/tmp` (tmpfs), the command run as the entrypoint verbatim, a hard per-command timeout, and the daemon-side `json-file` log capped at 10m so output floods cannot fill the host disk. `Options.Limits` are enforced only when set — the workbench caps them by default ([decisions §5.38](../explanation/decisions.md)). An empty `User` is the image's own user and an empty `Network` is no network at all ([spec §2.7o](../reference/spec.md#27o-a-docker-sandbox-runs-as-the-images-user-and-joins-no-network)).

With `Persistent: true` a single container is reused across `Exec` calls with a writable root filesystem; `VolumeName` mounts a named volume at `/workspace` (durable on any daemon), `TmpfsSize` resizes `/tmp` (default 64m), and `KeepOnClose` stops the container instead of removing it so a later Sandbox with the same `ContainerName` and configuration adopts it (an older configuration of ours is replaced; a foreign holder is a hard error). A timed-out exec has its process tree killed best-effort via an `AGENTS_SANDBOX_EXEC` environment marker — the container's PID/memory limits are the backstop for a process that scrubs its environment.

Optional capabilities are discovered by type assertion — `ExecStreamer`, `TerminalOpener`, `Lifecycle` (`Start`/`Stop`/`Status`, [spec §2.7p](../reference/spec.md#27p-stop-keeps-the-filesystem-and-promises-nothing-else)) and `Exporter` (the working tree as a tar stream); `sandbox/sandboxtest` is the conformance suite every backend runs and detects them.

`sandbox/e2b` is the second backend: any service speaking the E2B API — E2B's
own cloud, a self-hosted one, or a compatible service such as Alibaba Cloud's
Function Compute cloud sandbox. It needs an `APIKey`, a `TemplateID` that
already exists on the service, and — for anything but E2B's own cloud — the
`APIURL` and `Domain` that address it. The remote sandbox is created lazily on
first use, exactly as the docker container is; `OnSandboxID` is how a caller
remembers which one, so a restart resumes it rather than provisioning a second
([decisions §5.34](../explanation/decisions.md)). Any template works, including
a stock one: the working directory is created on the sandbox rather than
expected of the image ([spec §2.7q](../reference/spec.md#27q-a-sandbox-makes-its-working-directory)).

`Env` sets variables on the **container**, so a command, a persistent shell and a terminal opened into it all read the same values; an `ExecRequest.Env` entry of the same name wins for that one call. It is part of the adoption fingerprint: changing it replaces a persistent container instead of adopting the old one, keeping `/workspace` but discarding whatever was installed into the container itself ([spec §2.7n](../reference/spec.md#27n-a-sandboxs-environment-is-part-of-its-container-identity)).

### Remote daemon over SSH

```go
sb, err := docker.New(docker.Options{
	Image: "python:3.12-slim",
	Host:  "ssh://sandbox@dev-box",                    // ssh://user@host[:port][/socket]
	SSH:   docker.SSHAuth{KeyFile: "~/.ssh/id_ed25519"}, // or Password / UseAgent / KeyBytes
})
```

`Host: "ssh://…"` reaches a remote daemon's unix socket through SSH, in pure Go: every docker API request opens a `direct-streamlocal` channel on one shared SSH connection — no `ssh` binary locally, no docker CLI on the remote. The remote needs sshd with streamlocal forwarding allowed (the OpenSSH default) and the SSH user able to reach `/var/run/docker.sock` (typically the `docker` group). A severed SSH connection is re-established on the next request. The containers, images and volumes all live on the remote host; everything else about the backend is unchanged — except `WorkDir`, which `New` rejects with any non-local `Host`: a bind source resolves on the daemon's filesystem while the file tools run here, so remote daemons use `VolumeName`.

Authentication methods are tried in order — SSH agent (`UseAgent`), private key (`KeyFile`/`KeyBytes`, optionally `Passphrase`), then `Password`. Host keys are verified against `~/.ssh/known_hosts` by default; `InsecureIgnoreHostKey` disables this (dev/test only), and `KnownHostsFile` customizes it.

## FileTools configuration

```go
sandbox.FileToolConfig{
	Timeout:        10 * time.Second, // per file operation (default: sandbox.DefaultTimeout)
	MaxOutputBytes: 8192,             // truncation for read_file output
}
```

File operations require a **persistent working directory** (`WorkDir`). Backends without one (bare `sandbox.NewLocal()`, ephemeral Docker without `WorkDir`) return `sandbox.ErrNoWorkDir`.

**Path resolution follows shell semantics, the same view `exec_command` has** ([spec §2.7t](../reference/spec.md#27t-sandbox-file-tools-share-execs-path-view)): a relative path resolves under the working directory, an absolute path is used as-is — the model learns real paths from `pwd`/`ls` output and both spellings reach the same file. The one exception is **docker bind-mount mode**, whose file operations run on the *host* side of the mount: they are confined to `WorkDir` via `os.Root`, absolute paths must lie under the in-container mount point `/workspace` (translated to the host directory), and anything else fails with `sandbox.ErrOutsideWorkDir` (rendered to the model as "outside the working directory").

Every backend answers the file tools the same way: a missing path is `fs.ErrNotExist` ("not found" to the model), a directory read is "is a directory", and `list_files` sorts entries by name whatever order the backend returned them in.

`ReadFile` is size-capped on every backend: files larger than the backend's `MaxReadFileBytes` option (0 = `sandbox.DefaultMaxReadFileBytes`, 8 MiB) fail with `sandbox.ErrReadLimitExceeded` instead of being read into memory — model code cannot OOM the host by creating a huge file and reading it back. `apply_patch` still deletes such a file: it parks it under a temp name beside itself for the commit instead of snapshotting it in memory ([spec §2.7s](../reference/spec.md#27s-apply_patch-locates-hunks-by-whole-lines)). Errors returned to the model contain only the requested relative path and the error kind, never host or remote absolute paths.

## Writing a backend

Implement [`sandbox.Sandbox`](https://pkg.go.dev/github.com/zzir/agents-go/sandbox#Sandbox) — `Exec`, `ReadFile`, `WriteFile`, `CreateExclusive`, `ListDir`, `RemoveFile`, `Rename`, `Close` — to add your own backend (Firecracker, Kubernetes, remote runners, …). Optional interfaces the tools and the workbench discover by type assertion: `ExecStreamer` (output written to the writers as it arrives; the result's `Stdout`/`Stderr` are then empty), `TerminalOpener` (an interactive PTY shell — what powers the web terminal; the context bounds establishment only, the `Terminal` lives until `Close`), `Lifecycle` and `Exporter`. All three built-in backends stream; the docker backend hosts terminals only in `Persistent` mode and force-kills the shell's process tree on `Close`, e2b always does, and the local backend never does — handing out a host shell is a deliberately bigger grant than running commands. `OpenTerminal` returns an error wrapping `ErrTerminalUnsupported` when the current configuration cannot host one.

Three things a backend must get right:

- `CreateExclusive` is **atomic**: it creates parents, fails with `fs.ErrExist` if the path exists, and leaves no partial file on failure — `apply_patch`'s Add/Move rely on it to reject a clobber race-free.
- `Rename` creates the destination's parents; `apply_patch` parks a file too large to snapshot with it.
- A backend that assembles `sh -c` command lines passes every interpolated value — path, argument, environment entry — through `sandbox.ShellQuote`, the helper the Docker backend uses, so the escaping has one definition.

Run `sandbox/sandboxtest` against it; the suite detects the optional capabilities, so a backend implementing none still passes the core.

See [examples/sandbox](../../examples/sandbox/main.go) and [sandbox/docker/example](../../sandbox/docker/example/main.go) for runnable programs.
