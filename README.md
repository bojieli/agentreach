# waldo

**Your coding agent stays on your machine. Its tools act somewhere else.**

A *waldo* is a teleoperated remote manipulator — the arms used to handle
hazardous material from behind a barrier. The operator never enters the cell.
That is exactly this: your agent, your API keys, your subscription login all
stay local, while the agent's own tools read, write and run commands on a
remote host or container.

```console
$ waldo up ssh://client-box/srv/app --untrusted
$ waldo claude
```

Claude Code now runs commands on `client-box`, using its **native Bash tool**.
No MCP server, no new tool names, no filesystem mount, and **nothing installed
on the target**.

> **Status:** early. The Claude Code path is verified end-to-end against a real
> agent (see [Verified](#what-is-actually-verified)); other harnesses are in
> progress. Interfaces may change before 1.0.

---

## Why this exists

You want an agent to work on a machine that cannot host it:

- **It can't run the agent.** Claude Code is a 310 MB Node binary; Codex is
  247 MB of Rust. Plenty of servers are not going to run either.
- **It shouldn't hold your credentials.** On a client's server, an API key or
  OAuth token you place there is disclosed. Running the agent remotely means
  putting it there.
- **It must stay untouched.** No daemon, no helper binary, no footprint.

The usual answers each break one of those. Running the agent on the box fails
the first two. An MCP tool server changes the tool names the model sees — the
model calls `mcp__remote__read` instead of `Read` and behaves differently, so
you have retrained the agent rather than moved it. A FUSE mount fails loudly on
macOS (kernel extension, reboot) and, worse, fails *silently* on a flaky link:
stalled I/O in uninterruptible sleep, the agent frozen mid-call with no error to
reason about.

waldo's governing rule:

> **Every failure must be a value the agent can reason about, never a process
> that stops responding.**

A timeout becomes a tool error the model retries or routes around. That single
property is why waldo is request/response over SSH and not a mount.

## What it does not put on your target

Nothing. Tier 0 — the default — uses only a POSIX shell and moves file content
base64-framed over the same SSH connection. No agent, no daemon, no cached
binary, no temp files left behind. `waldo doctor` shows exactly what a given
host supports and what silently degrades.

Higher tiers are optimisations, never requirements. See
[docs/TRANSPORTS.md](docs/TRANSPORTS.md).

| tier | needs on target | writes to target | large read |
|---|---|---|---|
| `posix` | a POSIX shell | **nothing** | baseline |
| `sftp` *(negotiated when available)* | SFTP subsystem (stock OpenSSH) | **nothing** | ~7x faster |
| `pipe` | `python3` | **nothing** | ~5x faster |
| `agent` | can execute an upload | one cached binary, opt-in only | ~7x faster |

Autonegotiation stops below `agent`. Writing to someone else's machine stays an
explicit decision by the operator — and `waldo doctor` tells you whether waldo
has put anything there, with `waldo agent uninstall` to take it back off.

The tiers are interchangeable, and that is tested rather than asserted: all four
run one identical conformance suite, and the integration suite additionally
proves that a file written through any tier reads back byte-for-byte through
every other. Pinning a tier with `--fileops` that the host cannot support is an
error, never a silent downgrade.

## Install

```console
git clone https://github.com/bojieli/waldo && cd waldo
make install          # builds and installs to ~/.local/bin
```

Or take a binary from [releases](https://github.com/bojieli/waldo/releases);
each archive also carries the optional tier-3 helper for every target platform,
so nothing is ever downloaded at run time.

Requires Go 1.23+ to build, and the `ssh` client you already use. waldo shells
out to your system `ssh` on purpose, so `~/.ssh/config` — `ProxyJump`,
`IdentityFile`, `Match` blocks, hardware tokens, 2FA — keeps working exactly as
it does today.

## Use

```console
# bind a session to a target
waldo up ssh://build-box/srv/app

# run something there
waldo exec -- go test ./...

# work with files on the target directly
waldo fs read /srv/app/main.go
waldo fs grep 'func main' /srv/app
waldo fs write /srv/app/note.txt < local.txt

# launch an agent wired to it
waldo claude

# what does this host actually support, and what has waldo left on it?
waldo doctor
waldo agent uninstall   # remove the tier-3 helper, if you opted into it

# close the session and its connection
waldo down
```

Targets: `ssh://[user@]host[:port]/abs/path`, `docker://container/abs/path`,
`podman://…`, `local:///abs/path`, or scp-style `user@host:/abs/path`.

## How it works

Three layers; only the top one is harness-specific.

```
harness (claude · codex · kimi · opencode)
    │  native tool calls — the model sees no new tools
adapter          per harness · no fork · config or plugin
    │
waldo            session state · cwd · capability probe
    │  ssh · docker · podman · local
target           stock sshd only
```

There is deliberately **no daemon**. SSH's `ControlMaster` already provides
connection reuse — measured at ~7 ms per command against ~130 ms for a cold
connect — so a daemon would add a lifecycle, a socket, crash recovery and
orphaned processes in exchange for nothing.

### The Claude Code adapter

Claude Code exposes `CLAUDE_CODE_SHELL_PREFIX`, which hands an arbitrary
program the entire command as a single argument. waldo takes that envelope
apart rather than forwarding it, because two of its segments are local-only:

- **The shell snapshot** is generated on *your* machine and restores *your*
  functions, aliases and `PATH`. Remotely it is a no-op, and it embeds your
  username and directory layout — an information disclosure waldo would be
  causing on a client's server. It is stripped.
- **`pwd -P >| /tmp/claude-<rand>-cwd`** is how Claude Code persists `cd`
  between calls: it writes the resulting directory to a *local* temp file and
  reads it back. Forwarded verbatim, that file is written on the *remote* while
  the harness reads the local path, so `cd` silently stops working. waldo
  strips it, tracks the directory itself, and writes the local file the
  harness expects.

Full details, including the exact captured envelope, in
[docs/RESEARCH.md](docs/RESEARCH.md).

### Harness support

| harness | exec | file read/write/edit | mechanism |
|---|---|---|---|
| **Claude Code** | verified | **verified** — native `Read`/`Edit`/`Write` act on the target in `--mode mirror` | `CLAUDE_CODE_SHELL_PREFIX` + hooks |
| **Codex** | seam verified | **via shell** — reads, writes and `apply_patch` all travel over the shell tool, so one seam covers everything | `bash` shim on `PATH` (`execvp`) |
| **Kimi Code** | working | native tools still local | PATH shim (hooks planned) |
| **opencode** | partially verified | full — tools shadowed by name | generated tools (`waldo opencode install`) |

### Two modes

**`exec`** (default) — commands run on the target. Claude Code's native file
tools are **denied**, because they would keep acting on your local filesystem
while the agent believes otherwise. The agent uses the shell, which is
transparently remote.

**`mirror`** — additionally makes native file tools work. waldo fetches each
file the moment a tool opens it and writes it back when it changes:

```console
waldo up ssh://box/srv/app --mode mirror
waldo claude          # Read and Edit now operate on the target
```

This is deliberately **not** a sync engine. Nothing is mirrored until a tool
asks for it, and there is no background reconciliation — a sync engine would
have to answer questions waldo has no good answer to (both sides changed;
deleted or never fetched), and getting those wrong loses your work. Writes are
guarded by a content digest taken at fetch time, so a file that changed on the
target in between is refused rather than overwritten from a stale base.

`Grep` and `Glob` stay denied in mirror mode: the mirror holds only files
already opened, so a search would report confidently incomplete results. The
agent is pointed at `rg`/`find` over the shell, which run on the target and are
faster anyway.

**Why Claude Code's file tools are denied.** `Read`, `Edit`, `Write`, `Glob`
and `Grep` have no interception seam, and Claude Code ships as a compiled Node
SEA, so in-process patching is impossible (verified: `NODE_OPTIONS=--require`
is ignored). Left enabled they would keep acting on your **local** filesystem
while the agent believes it is working on the target — reading the wrong file
and reporting confident nonsense, or writing over your own work. waldo denies
them and tells the agent to use the shell, which is transparently remote. This
is a safety property, not a preference; `--allow-local-file-tools` overrides it
if you understand the consequence.

## Security

waldo exists because the target is not trusted.

- **No credential ever reaches the target.** The agent and its keys stay local.
- **SSH agent forwarding is off, always.** On a host with a hostile root, a
  forwarded agent socket lets that host authenticate as you against everything
  else you can reach.
- **Local paths are stripped** from forwarded commands where waldo can do so
  safely.
- **Output from the target is untrusted input.** It flows into the context of
  an agent that holds your credentials and can write to your local disk. Treat
  a compromised target as able to attempt prompt injection.

Details and the full threat model: [docs/SECURITY.md](docs/SECURITY.md).

## What is actually verified

Claims here are backed by experiments against real binaries, not by reading
docs. Reproduce with `make e2e` (spends model tokens) and `make conformance`.

- Claude Code 2.1.233 runs its native Bash tool on a remote sshd container,
  reads a file that exists only there, and `cd` persists across separate agent
  tool calls.
- In mirror mode, Claude Code used its native `Read` and `Edit` tools to change
  a file that exists only on the remote container, and the edit landed there.
- A `PreToolUse` hook can rewrite `Read`'s `file_path` via `updatedInput` —
  verified by having an agent read one path and receive another's content.
- `NODE_OPTIONS=--require` does **not** work against Claude Code's SEA binary.
- Codex 0.147.0 resolves its shell through `execvp`, so a `PATH` shim
  intercepts it.
- **All four file-operation tiers** round-trip NUL bytes, invalid UTF-8, CRLF,
  empty files, 5 MiB payloads and filenames containing quotes and spaces without
  corruption — over the local transport and over a real sshd.
- A file written through any tier reads back byte-for-byte, with a matching
  digest, through every other tier.
- `sftp` is the fastest tier in practice and `agent` the slowest to start, which
  is the opposite of what the tier numbering suggests. waldo negotiates on those
  measurements rather than on the numbering; the table and the reasoning are in
  [docs/TRANSPORTS.md](docs/TRANSPORTS.md), reproducible with `make bench`.

Not fully verified: a live Codex agent run (the Codex install used here points
at a third-party provider that rejects its token); a live Kimi run (no OAuth
login); and opencode end to end — its generated tools load, but the round trip
could not be completed on this machine. Each limitation is stated precisely in
`docs/harnesses/`.

These seams are undocumented implementation details in closed binaries. Every
one is covered by a conformance test that fails loudly when a harness upgrade
changes its shape, so breakage surfaces in seconds instead of mid-task.

## Development

```console
make check        # vet + unit tests. No network, no API key, no tokens.
make integration  # every file-operation tier against a real sshd
make bench        # what each tier actually costs
make conformance  # do the harness seams still have the shape waldo expects?
make e2e          # real agents against a real target (SPENDS TOKENS)
```

`make integration` starts an sshd owned by your user on a high port, so it needs
neither root, nor Docker, nor a network.

## License

Apache 2.0 — see [LICENSE](LICENSE).
