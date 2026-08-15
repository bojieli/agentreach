# waldo

**Your coding agent stays on your laptop. Its hands work on the server.**

[![CI](https://github.com/bojieli/waldo/actions/workflows/ci.yml/badge.svg)](https://github.com/bojieli/waldo/actions/workflows/ci.yml)
[![Go 1.23+](https://img.shields.io/badge/go-1.23%2B-00ADD8)](https://go.dev)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)
[![Dependencies](https://img.shields.io/badge/dependencies-none-brightgreen)](go.mod)

A *waldo* is a teleoperated manipulator — the arms used to handle hazardous
material from behind a barrier. The operator never enters the cell.

```console
$ waldo up ssh://client-box/srv/app --untrusted
$ waldo claude
```

Claude Code is now working on `client-box`. It is using its **own `Bash` tool**,
with its own name, on the machine you pointed at. No MCP server. No renamed
tools. No filesystem mount. **Nothing installed on the server**, and your API
key never leaves your laptop.

## See it

```console
$ waldo up ssh://client-box/srv/app
session "default" -> ssh://client-box/srv/app
  target   Linux x86_64
  fileops  pipe (negotiated; nothing written to the target)
  search   grep (no ripgrep on target)
  connect  multiplexed (one authenticated connection, reused)

$ waldo exec -- 'hostname; whoami'
client-box                      # ← the server, not your laptop
deploy

$ waldo claude
> what's eating disk on this box?
```

The agent runs `df`, reads logs, greps the codebase, edits a config — all of it
on `client-box`. Then:

```console
$ waldo log                      # everything it did over there
WHEN      ACTION  STATUS  DETAIL
14:02:11  exec    ok      df -h /
14:02:19  exec    ok      du -sh /var/log/*
14:02:30  write   ok      /srv/app/logrotate.conf
14:02:31  exec    exit 1  systemctl reload rsyslog
```

## Why not just…

| | the catch |
|---|---|
| **Run the agent on the server** | It's a ~300 MB Node binary, and your API key is now on someone else's machine |
| **MCP file server** | The model sees `mcp__remote__read` instead of `Read`. You didn't move the agent, you retrained it |
| **SSHFS / FUSE mount** | Needs a kernel extension and a reboot on macOS. On a flaky link it hangs in uninterruptible sleep — the agent freezes mid-call with no error to reason about |
| **`ssh` in the agent's terminal** | Now `cd` doesn't persist, paths are wrong, and the agent's file tools still edit your laptop |

waldo's governing rule:

> **Every failure must be a value the agent can reason about, never a process
> that stops responding.**

A timeout becomes a tool error the model retries or routes around. That single
property is why waldo is request/response over SSH and not a mount.

## The undocumented things this had to find out

None of these are in anyone's documentation. Each was found by running the real
binary and watching what it did.

**Claude Code hides `cd` in a temp file.** Every Bash call is wrapped, and the
wrapper ends with `pwd -P >| /tmp/claude-<rand>-cwd`. That is how the harness
remembers what directory you're in between tool calls — it writes the path
locally and reads it back. Forward that line to a remote host and it gets
written *there*, while Claude Code reads *here*, so `cd /srv/app` silently stops
persisting. waldo strips the line, tracks the directory itself, and writes the
local file the harness is expecting. Get it wrong and nothing errors; the agent
just quietly starts running every command in the wrong place.

**You cannot monkey-patch Claude Code.** It ships as a Node SEA with V8
statically linked. `NODE_OPTIONS=--require` is ignored — verified, not assumed.
So `Read`/`Edit`/`Write` cannot be re-pointed from inside the process, which is
why waldo either denies them or materialises real files for them.

**Harnesses want a program, not a command line.** `CLAUDE_CODE_SHELL_PREFIX` is
`stat`ed directly, so `"waldo shell-prefix"` is looked up as one filename and
fails. waldo installs an alias of itself and dispatches on `argv[0]`.

**Codex resolves its shell through `execvp`**, so a `bash` earlier on `PATH`
intercepts it — no fork, no config file, no cooperation from the harness.

Every one of these is pinned by a conformance test that fails loudly when a
harness upgrade changes the shape, so breakage surfaces in seconds instead of
in the middle of a task. Full transcripts: [docs/RESEARCH.md](docs/RESEARCH.md).

## What is actually verified

This project's rule is that a claim about a harness needs an experiment, not a
reading of its docs.

- **Six real hosts on three continents** — Ubuntu 22/24, Debian 12/13, root and
  non-root accounts, links from 171 ms to 540 ms per command — plus Docker,
  Alpine/busybox, a read-only rootfs, an unprivileged container, and a
  `FROM scratch` image with no shell at all. All four file-operation tiers pass
  the same suite on all of them.
- **The tiers agree byte for byte.** A file written through any tier reads back
  identically, with a matching digest, through every other.
- **The agent gets the PATH you would have on that machine.** `ssh host command`
  runs a non-interactive shell, whose PATH on a real host was missing five
  directories the login shell had — including `~/.cargo/bin`, where
  `cargo install ripgrep` puts `rg`.
- **waldo runs as the operator on macOS and on Linux**, with the whole suite —
  unit and integration — passing on both, including the control-socket path that
  only exists when `XDG_RUNTIME_DIR` is set.
- **NUL bytes, invalid UTF-8, CRLF, empty files, 5 MiB payloads, filenames with
  quotes and spaces** survive every tier intact.
- **waldo refuses SSH agent forwarding even when your own ssh config enables it
  for that host** — verified against a host configured that way, by watching no
  agent socket appear on the target.
- **A target with no shell is refused in about a second, with an explanation** —
  not a hang. A read-only target serves reads and refuses writes without leaving
  debris behind.
- **`cd` persists across separate agent tool calls**, and in mirror mode Claude
  Code's native `Read` and `Edit` changed a file that existed only on the remote
  machine.

Not verified, and said so: a live Codex run (the install here points at a
provider that rejects its token), a live Kimi run (no OAuth login), and opencode
end to end. Each limitation is stated precisely in
[docs/harnesses/](docs/harnesses/).

## Install

```console
go install github.com/bojieli/waldo/cmd/waldo@latest
```

Or grab a [release](https://github.com/bojieli/waldo/releases), or
`git clone && make install`. Needs the `ssh` you already have — waldo shells out
to it on purpose, so `ProxyJump`, `IdentityFile`, `Match` blocks, hardware
tokens and 2FA all keep working exactly as they do today.

Runs on **Linux, macOS and Windows**. Targets any POSIX host over
`ssh://`, `docker://`, `podman://`, or scp-style `user@host:/path`.

## Use

```console
waldo up ssh://build-box/srv/app     # bind a session to a target
waldo exec -- go test ./...          # run something there
waldo fs read /srv/app/main.go       # or work with files directly
waldo claude                         # launch an agent wired to it
waldo log                            # what did it do over there?
waldo doctor                         # what does this host support?
waldo down                           # close it, leave no trace
```

## How it works

Three layers. Only the top one knows which harness you're using.

```
harness (claude · codex · kimi · opencode)
    │  native tool calls — the model sees no new tools
adapter          per harness · no fork · config or plugin
    │
waldo            session state · cwd · capability probe
    │            fileops tier: posix · pipe · helper
    │  ssh · docker · podman · local
target           stock sshd only
```

**There is no daemon.** Session state is a file. SSH's `ControlMaster` already
provides connection reuse — measured at 4–5× faster per command than
reconnecting, against real hosts — so a daemon would add a lifecycle, a socket,
crash recovery and orphaned processes in exchange for nothing.

**Nothing is installed on your target.** Three of the four file-operation tiers
write nothing at all, and those are the only ones waldo picks on its own. The
fourth installs a small helper, only when you ask for it by name, and
`waldo doctor` will tell you it's there.

| tier | needs on target | writes | picked automatically |
|---|---|---|---|
| `posix` | a POSIX shell | nothing | yes (floor) |
| `pipe` | `python3` | nothing | yes (preferred) |
| `helper` | can run an upload | one cached binary | **never** |

Which tier is fastest is [measured, not assumed](docs/TRANSPORTS.md), on a real
network rather than on loopback — and one tier was deleted when the measurement
said it had stopped earning its place.

## Two modes

**`exec`** (default) — commands run on the target. Claude Code's native file
tools are **denied**, because they would keep acting on your laptop while the
agent believes otherwise. The agent uses the shell, which is transparently
remote.

**`mirror`** — additionally makes `Read`/`Write`/`Edit` work. waldo fetches each
file the moment a tool opens it and writes it back when it changes, guarded by a
content digest so a file that changed on the target in between is refused rather
than clobbered.

This is deliberately **not** a sync engine. Nothing is mirrored until a tool
asks for it. A sync engine would have to answer questions waldo has no good
answer to — both sides changed, deleted or never fetched — and getting those
wrong loses your work.

## Non-goals

- **Not a filesystem mount.** See the governing rule.
- **Not a sync engine.** See above.
- **Not protection against prompt injection.** Output from the target flows into
  the context of an agent holding your credentials. waldo makes this *more*
  relevant, not less, because the whole point is pointing agents at machines you
  don't control. [The threat model says so plainly](docs/SECURITY.md).
- **Not a way to run agents on Windows targets.** waldo's floor is a POSIX
  shell. Windows is supported as the machine you sit at, not the one you point
  at.

## Status

Early, and honest about it. The Claude Code path is verified end to end against
a real agent; the others are at the stage each of
[their notes](docs/harnesses/) says they are. Interfaces may change before 1.0.

What is *not* early: the file-operation layer, which is tested against six real
hosts, four tiers, three userlands and a fuzzer.

## Docs

| | |
|---|---|
| [ARCHITECTURE.md](docs/ARCHITECTURE.md) | how it fits together, and what was rejected |
| [TRANSPORTS.md](docs/TRANSPORTS.md) | the four tiers, measured on real links |
| [RESEARCH.md](docs/RESEARCH.md) | what each harness actually does, with transcripts |
| [SECURITY.md](docs/SECURITY.md) | threat model, and what waldo does not protect you from |
| [WINDOWS.md](docs/WINDOWS.md) | running waldo from Windows |
| [CONTRIBUTING.md](CONTRIBUTING.md) | the standard of evidence, and the design rules |

## Development

```console
make check        # vet + unit tests. No network, no API key, no tokens.
make integration  # every tier against a real sshd
make bench        # what each tier actually costs
make conformance  # do the harness seams still have the shape waldo expects?
```

`make integration` starts an sshd owned by your user on a high port — no root,
no Docker, no network. Point either at a host you already have with
`WALDO_TEST_SSH_HOST=my-box`.

## License

Apache 2.0 — see [LICENSE](LICENSE).
