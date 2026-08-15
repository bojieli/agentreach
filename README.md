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

Nothing. Three of waldo's four file-operation tiers write nothing to the target
at all — no agent, no daemon, no cached binary, no temp files left behind — and
those are the only ones waldo will choose on its own. The fourth installs a
helper, and only when you ask for it by name.

Tier 0 needs nothing but a POSIX shell, and is the floor everything else falls
back to. `waldo doctor` shows what a given host supports, what waldo negotiated,
and what it has left behind — which is normally the sentence "waldo has written
nothing to this target."

See [docs/TRANSPORTS.md](docs/TRANSPORTS.md).

| tier | needs on target | writes to target | large read, real link |
|---|---|---|---|
| `posix` | a POSIX shell | **nothing** | baseline |
| `sftp` | SFTP subsystem (stock OpenSSH) | **nothing** | ~1.2x faster |
| `pipe` *(negotiated when available)* | `python3` | **nothing** | ~4.5x faster |
| `agent` | can execute an upload | one cached binary, opt-in only | ~2.8x faster |

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

Or:

```console
go install github.com/bojieli/waldo/cmd/waldo@latest
```

Or take a binary from [releases](https://github.com/bojieli/waldo/releases).
Each archive also carries the optional tier-3 helper for every target platform,
so nothing is ever downloaded at run time.

Requires Go 1.23+ to build, and the `ssh` client you already use. waldo shells
out to your system `ssh` on purpose, so `~/.ssh/config` — `ProxyJump`,
`IdentityFile`, `Match` blocks, hardware tokens, 2FA — keeps working exactly as
it does today.

### Where it runs

waldo has two sides, and they have different requirements. The machine you sit
at runs waldo and the agent; the target only ever sees shell commands.

| your machine | status |
|---|---|
| **Linux** (amd64, arm64) | supported; unit and integration tests on every commit |
| **macOS** (Intel, Apple silicon) | supported; unit and integration tests on every commit |
| **Windows** (amd64, arm64) | supported; unit tests and a CLI smoke test on every commit — see the caveat below |
| **Windows via WSL** | supported — inside WSL this *is* Linux |

| your target | status |
|---|---|
| **Linux** (GNU coreutils, any arch) | supported; all four tiers tested against a real sshd |
| **macOS / BSD** | supported; all four tiers tested against a real sshd |
| **busybox / toybox** | supported; the full suite runs against an Alpine container |
| **Windows** | not supported: waldo's floor is a POSIX shell |

**The Windows caveat is speed, not correctness.** Win32-OpenSSH does not
implement `ControlMaster`: its multiplexing passes file descriptors over a Unix
socket, which Windows has no equivalent for. So every command opens and
authenticates its own connection — roughly 130 ms instead of 7 ms, and one
authentication per tool call instead of one per session.

waldo does not assume this. It establishes a multiplexed connection during
`waldo up` and asks the client to confirm it, so the answer reflects what your
client will actually do against that host, and a future Windows OpenSSH that
gains the feature will simply be used. `waldo up` prints which you got, and
`waldo doctor` explains it. Full detail, including what is verified on Windows
and what is not: [docs/WINDOWS.md](docs/WINDOWS.md).

Two practical consequences on Windows:

- **Run an `ssh-agent`.** Without one, a passphrase-protected key needs its
  passphrase on every single tool call, and waldo runs `ssh` in batch mode, so
  they will all fail instead of prompting.
- **A POSIX shell is optional.** waldo does not need Git for Windows or MSYS2 to
  drive a remote host; it only matters if you want this Windows machine to be
  the *target*, which is not supported.

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

# what did the agent actually do on the target?
waldo log
waldo log --failed

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
    │            fileops tier: posix · sftp · pipe · agent
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

## What it did

waldo records every command it runs on a target and every file it changes
there, in a local JSON-lines file per session:

```console
$ waldo log
WHEN      ACTION  STATUS  DETAIL
14:02:11  exec    ok      go test ./...
14:02:30  write   ok      /srv/app/internal/handler.go
14:02:31  exec    exit 1  go vet ./...
```

The point is the situation waldo is built for: you pointed an autonomous agent
at a machine you do not own, and afterwards somebody asks what it did. Without a
record the honest answer is "I don't know".

Nothing is sent anywhere — it is a file only you can read, and it outlives
`waldo down` deliberately. `WALDO_NO_AUDIT=1` turns it off, which is the right
call when a command line will contain a secret.

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
- The tier ordering measured over loopback **inverts** over a real 258 ms link,
  because the tiers differ in round trips per operation rather than in work
  done. waldo negotiates on the remote numbers, since remote hosts are the
  entire point. Both tables, and what they cost to learn, are in
  [docs/TRANSPORTS.md](docs/TRANSPORTS.md); reproduce with `make bench`, or
  against your own host with `WALDO_BENCH_SSH_HOST=my-box make bench`.
- All four tiers pass the conformance suite against five real remote hosts
  (Ubuntu 22/24, Debian 12/13, root and non-root accounts, links from
  sub-millisecond to ~540 ms), a Docker container, and a busybox (Alpine)
  target — not only against loopback.
- A target with no shell at all (a `FROM scratch` container) is refused in about
  a second with an explanation, rather than hanging. A read-only target serves
  reads and refuses writes without leaving debris. An unprivileged target passes
  the whole suite and refuses what it should.
- waldo refuses SSH agent forwarding even when the operator's own ssh config
  turns it on for that host — verified against a host configured that way, by
  observing that no agent socket reaches the target.

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

## Contributing

The one rule that matters: **claims about a harness must be backed by an
experiment, not by its documentation.** Everything else is in
[CONTRIBUTING.md](CONTRIBUTING.md), including the design rules that are
load-bearing and the conformance suite a new file-operation tier has to pass.

Security reports go through GitHub Security Advisories rather than public
issues — see [SECURITY.md](SECURITY.md). Changes are recorded in
[CHANGELOG.md](CHANGELOG.md).

## License

Apache 2.0 — see [LICENSE](LICENSE).
