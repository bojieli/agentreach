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
│      │ argv, hook JSON on stdin, or a generated tool       │
│  ┌───▼─────────────────────────────────────┐               │
│  │ session   target · cwd · capabilities   │  a file,      │
│  │           tier decision                 │  not a daemon │
│  ├─────────────────────────────────────────┤               │
│  │ fileops   posix · sftp · pipe · agent   │               │
│  ├─────────────────────────────────────────┤               │
│  │ transport ssh · docker · podman · local │               │
│  └───┬─────────────────────────────────────┘               │
└──────┼─────────────────────────────────────────────────────┘
       │  system ssh (multiplexed where the client supports it)
┌──────▼─────────────────────────────────────────────────────┐
│ target: stock sshd only. no node, no python, no waldo bits │
└────────────────────────────────────────────────────────────┘
```

Only the top layer is harness-specific.

## There is no daemon

Session state — which target, which directory, which tier, what the target's
userland supports — lives in a file under `~/.waldo`. Every waldo invocation is
a short-lived process that reads it.

A daemon would buy exactly one thing: connection reuse. SSH's `ControlMaster`
already provides that — measured against real hosts at 4–5× faster per command
than reconnecting: 171 ms against 772 ms on one, 557 ms against 2.85 s on
another. Paying for it a second time would mean a lifecycle, a socket, crash
recovery, version skew between a running daemon and an upgraded binary, and
orphaned processes holding connections to someone else's server — in exchange
for nothing.

One consequence is worth stating early, because it shapes the tier design more
than anything else: **waldo runs one process per tool call.**

## Platforms

waldo runs on Linux, macOS and Windows, and targets any POSIX host. The split
matters because the two sides need different things: the operator's machine runs
a harness and needs to intercept its shell, while the target only ever sees
shell commands.

Every operating-system difference lives in two files — `platform_other.go` and
`platform_windows.go` — so the cost of supporting Windows is visible in one
place rather than spread through the adapters. Windows needs four things Unix
gives for free: a launcher that is not `execve`, shims that are not symlinks,
executability decided by `PATHEXT` rather than a mode bit, and a search-path
variable matched case-insensitively.

The fifth difference cannot be abstracted away. Win32-OpenSSH does not implement
`ControlMaster`, so a Windows operator pays a full connection setup per command
rather than ~7 ms on a shared one. That is not a portability detail: the
argument for [having no daemon](#there-is-no-daemon) is precisely that
ControlMaster already provides connection reuse, and on Windows that premise is
false. waldo therefore *probes* for multiplexing rather than assuming it, records
the answer, and reports it — see [WINDOWS.md](WINDOWS.md).

## waldo uses the system ssh, not a Go SSH library

Users reach real hosts through jump hosts, certificate authorities, hardware
tokens, `gpg-agent`, Kerberos, 1Password, and `Match exec` blocks.
Reimplementing that surface faithfully is not realistic, and getting it subtly
wrong strands people on exactly the hosts they most need to reach. waldo shells
out to the `ssh` they already have, so `~/.ssh/config` keeps working unchanged.

The cost is that ssh reports its own failures as exit 255, which is
indistinguishable from a command that genuinely exited 255. waldo therefore
carries the real status in-band behind an unguessable marker, and its
**absence** is the signal that the transport, rather than the command, failed.
Getting this wrong in either direction is bad: a transport failure reported as a
command failure sends the agent chasing a phantom bug, and a command failure
reported as a transport failure makes waldo retry something that must not be
retried.

## The two interfaces

waldo separates *reaching a target* from *performing file operations on it*.

```go
// internal/transport — how to reach a target and run a command.
type Transport interface {
    Run(ctx, ExecRequest) (ExecResult, error)   // to completion, bounded output
    Open(ctx, command string) (Stream, error)   // long-lived, piped stdio
    Describe() string
    Close() error
}

// Implemented only by ssh: starts a subsystem rather than a command.
type SubsystemOpener interface {
    OpenSubsystem(ctx, name string) (Stream, error)
}
```

```go
// internal/fileops — how to act on files, in four interchangeable ways.
type FileOps interface {
    Read(ctx, path string, off, n int64) ([]byte, error)
    Write(ctx, path string, data []byte, mode fs.FileMode) error
    Stat(ctx, path string) (*FileInfo, error)
    List(ctx, path string) ([]FileInfo, error)
    Mkdir(ctx, path string, mode fs.FileMode) error
    Remove(ctx, path string, recursive bool) error
    Rename(ctx, from, to string) error
    Search(ctx, SearchRequest) ([]Match, error)
    Glob(ctx, root, pattern string) ([]string, error)
    Hash(ctx, path string) (string, error)
    Tier() Tier
    Close() error
}
```

`Search` and `Glob` are first-class operations, not helpers derived from `List`.
They execute **on the target** and return only matches, at every tier — which is
precisely what a mount cannot do, and the main reason waldo beats one on the
operation that matters most. Deriving them client-side would mean dragging every
candidate file across the network to answer a question the target could have
answered locally.

## Tiers

Four strategies implement `FileOps`. They share almost no code — a shell
pipeline, an SFTP subsystem, a Python handler, a Go binary — and a user cannot
tell which is in use. That interchangeability claim is only worth something
because every tier runs one identical conformance suite
(`internal/fileops/fileopstest`): over the local transport in unit tests, and
over a real sshd in `test/integration`, which additionally asserts that a file
written through any tier reads back byte-for-byte through every other. A tier
that cannot pass it does not ship.

Full detail, including what each tier requires and writes, is in
[TRANSPORTS.md](TRANSPORTS.md). The architectural points:

- **Tier 0 is the floor and needs only a POSIX shell.** Everything above it is
  an optimisation that is never required.
- **Negotiation follows measurement, not the tier numbering.** The numbers rank
  capability; waldo ranks tiers by what they actually cost in a
  process-per-call design, where an interpreter or binary starting up is pure
  overhead. That makes tier 1 the negotiated choice where available, and the
  nominally fastest tier 3 the slowest to start.
- **A pinned tier is an instruction.** `--fileops=X` fails rather than
  substituting something else, because a `waldo status` reporting a tier the
  session is not using is a lie the operator will act on. An autonegotiated tier
  may still step down, and says so on stderr.
- **Only tier 3 writes to the target**, only when asked, never on an
  `--untrusted` session. Everything it installs is listed by `waldo doctor` and
  removed by `waldo agent uninstall`.

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
fidelity: `read`/`write`/`edit`/`grep`/`glob` are backed by the target directly,
and no mirroring is needed.

### `mirror` — the file a tool is about to touch, materialised

Gives Claude Code native file tools without MCP and without FUSE. A `PreToolUse`
hook rewrites the tool's `file_path` to a local copy that waldo fetches at that
moment; a `PostToolUse` hook writes the result back.

This is deliberately **not** a sync engine, and not a bulk copy of the
workspace. Nothing is mirrored until a tool asks for it, and there is no
background reconciliation. A sync engine has to answer questions waldo has no
good answer to — both sides changed, deleted or never fetched — and getting them
wrong loses the operator's work. Fetching exactly the file a tool is about to
touch, at the moment it touches it, raises none of them.

**Writes are guarded by a digest taken at fetch time.** If the file changed on
the target in between — a build, a deploy, another session — the write is
refused with an error the agent can act on, rather than overwriting from a stale
base. A refusal the agent can see is always better than a quiet loss.

**`Grep` and `Glob` stay denied in mirror mode.** The mirror holds only files
already opened, so a search across it would report confidently incomplete
results, and an agent told "no matches" concludes the code does not exist. The
agent is pointed at `rg`/`find` over the shell, which run on the target and are
faster anyway.

**Where the mirror lives.** Under `~/.waldo/mirror/<session>/`, with the
target's absolute path reproduced beneath it: `/srv/app/main.go` becomes
`~/.waldo/mirror/default/srv/app/main.go`. Placing it at the identical absolute
path would make compiler output and stack traces line up with no translation at
all, which is genuinely attractive — but it would require waldo to write to
`/srv` on the operator's own machine. That is usually impossible without root,
and an unacceptable thing for this tool to do even where it is possible.

The residual cost is that the agent sees a local path in a `Read` result and
could try to use it in a shell command, where it does not exist. The mirror-mode
system prompt tells it to use the target's own paths, and the hook leaves every
path outside the workspace alone so the harness's own files keep working.

Paths are cleaned before being joined to the mirror root, so a path containing
`..` cannot escape it. File paths can originate in content read from an
untrusted target, which makes that a real attack path rather than a theoretical
one.

## Harness adapters

| harness | seam | verified |
|---|---|---|
| Claude Code | `CLAUDE_CODE_SHELL_PREFIX` → whole command as one argv element | yes, 2.1.233 |
| Codex | `bash` shim on `PATH` (`execvp`) + sandbox network allowance | see [harnesses/codex.md](harnesses/codex.md) |
| Kimi Code | `PATH` shim | see [harnesses/kimi.md](harnesses/kimi.md) |
| opencode | generated tools shadowing built-ins by name | see [harnesses/opencode.md](harnesses/opencode.md) |

No harness is forked. Claude Code and Codex keep their own authentication, so
subscription logins continue to work and no key is introduced anywhere.

Harnesses want a *program path* for their shell hook, not a command line —
Claude Code stats the value of `CLAUDE_CODE_SHELL_PREFIX` directly, so
`waldo shell-prefix` would be looked up as a single filename and fail. waldo
dispatches on `argv[0]` through a symlink instead, which costs nothing per tool
call.

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
