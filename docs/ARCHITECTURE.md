# waldo Architecture

> A *waldo* is a teleoperated remote manipulator: the operator stays behind the
> barrier, the manipulator works inside. That is the whole design. The agent —
> the brain, and the credentials it holds — never leaves your machine. Only the
> observation and action space is remote.

## Problem

Coding agents assume their tools act on the machine they run on. Three
constraints break that assumption simultaneously:

1. **The agent cannot be installed on the target.** Many servers cannot run a
   310 MB Node SEA or a Bun runtime.
2. **Credentials must not reach the target.** A client's server is untrusted;
   an API key or OAuth token placed there is disclosed.
3. **The target must stay unmodified.** No daemon, no binary, no footprint.

The standard answers each fail one of these: running the agent remotely fails
(1) and (2); an MCP tool server changes the tool names the model sees, which
breaks transparency; a FUSE mount fails loudly on macOS (kernel extension,
reboot) and fails *silently* on an unstable link — stalled I/O in uninterruptible
sleep, with the agent frozen mid-call and no error to reason about.

## Principle

> Every failure must be a value the agent can reason about, never a process
> that stops responding.

An RPC timeout becomes a tool error the model sees, retries, or routes around.
That single property is why waldo is built on request/response over SSH rather
than on a filesystem mount.

## Shape

```
┌─ your machine ─────────────────────────────────────────────┐
│  harness (claude / codex / kimi / opencode)                │
│      │ native tool calls — the model sees no new tools     │
│  ┌───▼─────────────────────────────────────┐               │
│  │ adapter (per harness, thin, no fork)    │               │
│  └───┬─────────────────────────────────────┘               │
│      │ unix socket, length-prefixed JSON                   │
│  ┌───▼─────────────────────────────────────┐               │
│  │ waldo daemon                            │               │
│  │   session registry · cwd state          │               │
│  │   connection pool · retry policy        │               │
│  └───┬─────────────────────────────────────┘               │
└──────┼─────────────────────────────────────────────────────┘
       │  ssh  ·  docker  ·  podman  ·  kubectl  ·  local
┌──────▼─────────────────────────────────────────────────────┐
│ target: stock sshd only. no node, no python, no waldo bits │
└────────────────────────────────────────────────────────────┘
```

Three layers; only the top one is harness-specific.

## The backend contract

Every target implements one interface. `search` and `glob` are **first-class
operations, not derived from readdir** — they execute server-side and return
only matches, which is precisely what a mount cannot do and why waldo is faster
than a mount on the operation that matters most.

```go
type Backend interface {
    Exec(ctx, ExecRequest) (ExecResult, error)
    Read(ctx, path string, off, n int64) ([]byte, error)
    Write(ctx, path string, data []byte, mode fs.FileMode, expect Precondition) (Digest, error)
    Stat(ctx, path string) (FileInfo, error)
    List(ctx, path string) ([]FileInfo, error)
    Search(ctx, SearchRequest) ([]Match, error)
    Glob(ctx, pattern, root string) ([]string, error)
    Remove(ctx, path string) error
    Rename(ctx, from, to string) error
    Mkdir(ctx, path string, mode fs.FileMode) error
    Close() error
}
```

Implementations: `local`, `ssh`, `docker`, `podman`, `kubectl`. A session binds
to exactly one, so nothing can cross-contaminate between targets.

### Zero remote footprint

File operations ride the **SFTP subsystem that stock OpenSSH already ships**,
used as a protocol rather than as a filesystem. Search and glob are ordinary
commands (`rg`, else `grep -r`; `find`) on an exec channel. Nothing is written
to the remote host, so nothing needs cleaning up and nothing can be left behind
on a client's machine.

If a hardened host has the SFTP subsystem disabled, the SSH backend degrades to
`cat`/`base64`/`dd` over exec channels. `waldo doctor` reports which tier a
host is on.

### Behaviour on a bad link

- A native SSH implementation (`x/crypto/ssh` + `pkg/sftp`), not the `ssh`
  binary — waldo owns timeouts, keepalives, and concurrency instead of
  inheriting OpenSSH's.
- One TCP connection, many multiplexed channels; SFTP pipelines requests.
- Per-request deadlines and exponential-backoff reconnect.
- Reads and stats retry silently. Writes retry only under a
  content-hash precondition, so a replayed write can never clobber a
  concurrent change.
- Offset-addressed reads and writes, so a transfer resumes rather than restarts.
- Bounded output capture, so a runaway log cannot wedge a session.

## Two modes

A session runs in one of two modes, chosen by what the harness's file tools can
be made to do.

### `exec` — command execution is remoted; file tools are not

Correct and zero-copy. Used when the harness's file tools cannot be redirected
(Claude Code, Codex, Kimi) and mirroring is not wanted.

Because the harness's native `Read`/`Edit`/`Write` would still silently act on
the **local** filesystem — reading the wrong file while the agent believes it is
remote — waldo **denies those tools** in the generated harness config for this
mode. Silent wrong-target file access is the worst failure this design can
produce, so it is made structurally impossible rather than documented as a
caveat. The agent uses the shell for file access, which is transparently remote.

For harnesses that *can* shadow tools by name (opencode), `exec` mode is full
fidelity: `read`/`write`/`edit`/`grep`/`glob` are backed by the remote backend
directly, and no mirroring is needed.

### `mirror` — a real local tree, kept coherent

Gives Claude Code, Codex, and Kimi native-speed file tools without MCP and
without FUSE. waldo materialises the remote workspace as **ordinary local
files** and keeps them coherent around each command:

- before an exec: upload files whose local content hash changed
- after an exec: `find <root> -newermt @<ts>` on the remote (one cheap command,
  no remote agent) then download exactly what changed

Reads never block on the network because the files are genuinely local, and a
sync failure is a visible error rather than a stalled syscall. This is what
distinguishes mirroring from a mount.

**Path identity.** The mirror is placed at the *same absolute path* as the
remote workspace. `/srv/app` remotely is `/srv/app` locally. Nothing —
commands, compiler output, stack traces, git worktree `gitdir` pointers — needs
translating, and the entire class of bugs where an absolute path is correct on
one side and wrong on the other cannot occur. waldo refuses to start a mirror
session it cannot place at the identical path, rather than silently falling
back to translation.

## Harness adapters

| harness | seam | verified |
|---|---|---|
| Claude Code | `CLAUDE_CODE_SHELL_PREFIX` → whole command as one argv element | yes, 2.1.233 |
| Codex | login-shell substitution + `apply_patch_tool=false` | see `docs/harnesses/codex.md` |
| Kimi Code | `PreToolUse` hook / shell substitution | see `docs/harnesses/kimi.md` |
| opencode | custom tools shadowing built-ins by name | documented API |

No harness is forked. Claude Code and Codex keep their own authentication, so
subscription logins continue to work and no key is introduced anywhere.

### The Claude Code envelope

Claude Code wraps every Bash call. waldo parses that envelope rather than
forwarding it, because two segments are local-only. Full shape and rationale in
[RESEARCH.md](RESEARCH.md); the operational summary:

- **strip** `source <local-snapshot>.sh` — references local paths, and leaks the
  local username and directory layout to the remote host
- **strip** `pwd -P >| /tmp/claude-<rand>-cwd` — this is how `cd` persists
  between calls; forwarded verbatim it would be written on the *remote* while
  Claude Code reads it *locally*, and `cd` would silently stop working. waldo
  tracks cwd itself and writes the local file the envelope named.
- **forward** everything else

Envelope shape is version-specific, so `internal/envelope` parses defensively,
falls back to forwarding the whole string when the shape is unrecognised, and
is covered by a conformance test that fails when a Claude Code upgrade changes
it.

## Security posture

waldo exists because the target is not trusted. Consequences, in full in
[SECURITY.md](SECURITY.md):

- No credential, token, or key is ever sent to the target.
- SSH agent forwarding is **refused by default**. On a host with a hostile root,
  a forwarded agent socket lets that host authenticate as you everywhere else
  you can reach.
- Local paths and usernames are stripped from forwarded commands where possible.
- Output from the target is **untrusted input**. It flows into the context of an
  agent that holds your credentials and can write to your local disk; waldo
  frames it as untrusted data.
