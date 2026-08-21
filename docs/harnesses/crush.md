# Crush (Charm)

Open-source Go agent by Charm.  Installed as `crush` CLI.  MIT-licensed.

## Primary seam: server mode

Crush has a native remote-execution mode:

```bash
crush server --host tcp://host:port
```

In server mode every tool call — bash, file read, file write, edit, glob,
grep — executes on the machine running `crush server`.  The CLI instance that
talks to the model acts as the client; the server instance acts on the target.

For waldo, `waldo crush` would:
1. Start `crush server` on the session target (via SSH).
2. Launch `crush --remote tcp://localhost:PORT` locally, tunneling the TCP
   connection through the SSH session.
3. All tool calls route to the server on the target.

This gives 100% coverage with no patching and no env-var tricks — the seam is
Crush's own design.  Implementation in waldo is pending but straightforward.

## Secondary seam: hooks with input rewrite

Crush's `internal/agent/hooked_tool.go` runs `PreToolUse` hooks before every
tool call and applies input rewrites:

```go
if result.UpdatedInput != "" {
    call.Input = result.UpdatedInput
}
```

A hook that receives the tool call's JSON input and returns a modified version
causes Crush to execute the original tool with the rewritten parameters.  For
path-taking tools (read, write, edit), a hook can rewrite the `path` parameter
from a local path to a remote path — provided the remote path exists locally
too, or the tool is configured to operate on a mounted filesystem.

Hooks also block calls (`DecisionDeny`) and halt the whole turn (`Halt`).
Blocking a file tool and providing a fake response is NOT supported: the hook
cannot substitute a result, only allow/deny/rewrite-input.

Hook coverage is therefore partial: path rewriting helps with mirrored
filesystems but cannot redirect arbitrary local paths to a remote target.
Server mode (above) is the correct primary seam.

## PATH shim: complicated by deny list

Crush has a deny list for subprocess-based attacks:
`internal/agent/tools/bash.go` blocks tools named `ssh`, `scp`, `curl`, and
`wget` by exact binary name.  The PATH shim is named `bash`, which is not
blocked, so the shim would intercept Crush's Bash tool calls in principle.

However, the deny list means the agent cannot use `ssh` or `scp` inside its
Bash calls — waldo's own transport uses SSH, so this is a second-order concern
— and the PATH shim adds complexity without adding capability beyond what
server mode already provides.

Server mode is always preferred.

## Seam coverage

| Tool surface | Server mode | Hook rewrite | PATH shim |
|---|---|---|---|
| `bash` (shell commands) | **✓ remote** | partial | partial |
| `read` (file) | **✓ remote** | path rewrite only | — |
| `write` (file) | **✓ remote** | path rewrite only | — |
| `edit` (file) | **✓ remote** | path rewrite only | — |
| `glob` | **✓ remote** | — | — |
| `grep` | **✓ remote** | — | — |

## Implementation status

`waldo crush` is implemented.  It uses Crush's server mode end-to-end:
`crush server --host tcp://127.0.0.1:PORT` starts on the target, the port is
forwarded through the SSH ControlMaster socket, and the local crush client
connects to `tcp://127.0.0.1:PORT`.

Crush requires an SSH session (`ssh://` target); Docker and local sessions
are not supported in server mode.  `--force` bypasses server mode and launches
crush locally (tools act on the operator's machine — operators are warned).

## Probe

A formal seam probe (`waldo harness verify crush`) is not yet implemented.
Server mode is inherently verifiable by design — all tool calls execute on
the server (the target) — but a scripted end-to-end probe would confirm this
behaviorally rather than by assertion.  It is tracked for a future release.
