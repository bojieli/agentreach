# AgentReach

**Your coding agent stays on your laptop. Its hands work on the server.**

[![CI](https://github.com/bojieli/agentreach/actions/workflows/ci.yml/badge.svg)](https://github.com/bojieli/agentreach/actions/workflows/ci.yml)
[![Go 1.25.8+](https://img.shields.io/badge/go-1.25.8%2B-00ADD8)](https://go.dev)
[![License](https://img.shields.io/badge/license-MIT-blue)](LICENSE)
[![Zero dependencies](https://img.shields.io/badge/dependencies-none-brightgreen)](go.mod)

---

## The problem

Two situations come up constantly when you work with coding agents:

**Working on a remote server.** You SSH into a build box, a cloud VM, a client's staging environment. You want an agent to help. The obvious move — install Claude Code or Codex there — takes five minutes and leaves your API key on a machine you may not fully control. On a shared server, that key is visible to whoever has root. On a client's machine, it's on a machine someone else administers. Even on your own VM, a compromised server means a compromised key.

**Working in dev containers.** You spin up a fresh container for every project, every branch, every experiment. Installing Claude Code — a 300 MB Node binary — inside each one is slow and doesn't belong in a Dockerfile. Installing it on the host and doing something clever with mounts is fiddly and fragile.

AgentReach (`reach`) solves both: the agent runs on your machine, where your credentials live, and its tools act on the remote host as if it were local. Nothing is installed on the server. Your API key never leaves your laptop.

---

## See it

```console
$ reach up ssh://build-box/srv/app
session "default" → ssh://build-box/srv/app
  target   Linux x86_64
  fileops  pipe (negotiated; nothing written to the target)
  search   ripgrep (rg)
  connect  multiplexed (one authenticated connection, reused)

$ reach claude
> what's eating disk on this box?
```

Claude Code runs on your laptop. The commands it issues — `df`, `du`, `grep` — run on `build-box`. Files it reads come from `build-box`. When it writes a config, the change lands on `build-box`. It never touches your local filesystem unless you ask it to.

```console
$ reach log
WHEN      ACTION  STATUS  DETAIL
14:02:11  exec    ok      df -h /
14:02:19  exec    ok      du -sh /var/log/*
14:02:30  write   ok      /srv/app/logrotate.conf
14:02:31  exec    exit 1  systemctl reload rsyslog
```

---

## Supported agents

| Agent | Command | Status |
|---|---|---|
| [Claude Code](docs/harnesses/claude-code.md) | `reach claude` | ✓ verified end-to-end (2.1.233) |
| [Codex](docs/harnesses/codex.md) | `reach codex` | ✓ verified end-to-end (0.148.0) |
| [Kimi Code](docs/harnesses/kimi.md) | `reach kimi` | ✓ verified (0.37.2) |
| [opencode](docs/harnesses/opencode.md) | `reach opencode` | ✓ verified (1.18.18) |
| [Goose](docs/harnesses/goose.md) | `reach goose` | ✓ verified |
| [Crush](docs/harnesses/crush.md) | `reach crush` | ✓ verified |
| [Gemini CLI](docs/harnesses/gemini.md) | `reach gemini` | ✓ verified |

Each agent's existing authentication stays untouched — subscription logins, OAuth tokens, and API keys all continue to work exactly as they do today.

---

## Install

```console
go install github.com/bojieli/agentreach/cmd/reach@latest
```

Or grab a pre-built binary from [Releases](https://github.com/bojieli/agentreach/releases), or build from source:

```console
git clone https://github.com/bojieli/agentreach
cd agentreach
make install
```

**Requirements:** The `ssh` binary you already have on your system. No other runtime dependency. reach shells out to your existing `ssh` so that `ProxyJump`, `IdentityFile`, `Match` blocks, hardware tokens, and 2FA all keep working exactly as they do today.

Runs on **Linux, macOS, and Windows** (as the machine you sit at). Targets any POSIX host over `ssh://`, `docker://`, `podman://`, or the familiar `user@host:/path` shorthand.

---

## Quick start

**Step 1: Point reach at a target.**

```console
# A server you own and trust:
reach up ssh://build-box/srv/app

# A shared or client-owned server:
reach up ssh://client-box/srv/app --untrusted
```

`--untrusted` adds an extra guarantee: the optional helper binary is never installed on that host under any circumstances. Both forms leave nothing on the server otherwise.

This opens a multiplexed SSH connection and probes the host for what it supports. `reach doctor` shows you the full result — what was found, what tier was negotiated, and what (if anything) reach has placed there.

**Step 2: Run your agent.**

```console
reach claude       # Claude Code
reach codex        # Codex
reach goose        # Goose
# ... any supported agent
```

The agent launches on your machine with its shell silently redirected to the remote host. You interact with it normally. Commands run there; your API key stays here.

**Step 3: Clean up.**

```console
reach down
```

Closes the connection and leaves nothing behind.

---

## Commands

```console
reach up <target>       bind a session to a target and probe its capabilities
reach down [session]    close a session and leave no trace
reach status            show active sessions
reach doctor            diagnose what a target supports and why
reach log               audit log — what reach ran and changed on the target
reach exec -- <cmd>     run a one-off command on the target
reach fs read <path>    work with files on the target directly
reach helper uninstall  remove anything reach placed on the target
```

**Target formats:**
```
ssh://user@host/path/to/work
user@host:/path/to/work        (scp-style shorthand)
docker://container/path
podman://container/path
```

---

## Why not just…

| | what goes wrong |
|---|---|
| **Install the agent on the server** | A 300 MB Node binary, and your API key is now on a machine you don't control |
| **SSHFS / FUSE mount** | Needs a kernel extension and a reboot on macOS. On a flaky link it hangs in uninterruptible sleep — the agent freezes mid-call with no error to reason about |
| **Add an MCP file server** | The model sees `mcp__remote__read_file` instead of `Read`. You didn't move the agent; you retrained it |
| **SSH in the agent's terminal** | Now `cd` doesn't persist across calls, paths are wrong, and the agent's file tools still edit your laptop |

reach's governing rule:

> **Every failure must be a value the agent can reason about, never a process that stops responding.**

A timeout becomes a tool error the model retries or routes around. That single property is why reach is built on request/response over SSH rather than a filesystem mount.

---

## How it works

Three layers. Only the top layer knows which agent you're using.

```
harness  (claude · codex · kimi · opencode · goose · crush · gemini)
    │     native tool calls — the model sees no new tools
adapter   per harness · no fork · config or plugin
    │
reach     session state · cwd · capability probe
    │     file-op tier: posix · pipe · helper
    │  ssh · docker · podman · local
target    stock sshd only — nothing installed
```

**No daemon.** Session state is a file. SSH's `ControlMaster` multiplexes the connection at 4–5× the speed of reconnecting, so a daemon would add lifecycle complexity in exchange for nothing.

**Nothing installed on the target** (by default). Two of the three file-operation tiers write nothing at all, and those are the only ones reach picks automatically. The third installs a small helper only when you explicitly ask for it.

| tier | needs on target | writes to target | picked automatically |
|---|---|---|---|
| `posix` | a POSIX shell | nothing | yes (fallback) |
| `pipe` | `python3` | nothing | yes (preferred) |
| `helper` | can run an uploaded binary | one cached binary | **never** |

---

## Two modes

**`exec`** (default) — commands run on the target. The agent's native file tools (`Read`, `Edit`, `Write`) are denied, because they have no way to be redirected and would silently act on your laptop. The agent uses its shell tool, which is transparently remote.

**`mirror`** — additionally wires Claude Code's native `Read`/`Write`/`Edit` to work against the target. Each file is fetched the moment a tool opens it, and written back when it changes, guarded by a content digest so a file that changed on the server in between is refused rather than clobbered.

```console
reach up ssh://build-box/srv/app --mode mirror
reach claude
```

Mirror is useful for heavy edit sessions where you want to use Claude Code's native file tools rather than shell redirects. Use it with eyes open: reads can be slightly stale, and it's a copy-on-demand mechanism, not a sync engine. For shell-shaped work, `exec` mode is simpler and has no staleness window.

---

## Security

reach is built for untrusted targets, and the security model reflects that.

- **No credential reaches the target.** The agent, its API key, and its conversation stay on your machine.
- **SSH agent forwarding is off, unconditionally.** A forwarded agent socket on a hostile server lets that server authenticate as you everywhere else you can reach. There is no flag to re-enable this.
- **Nothing is installed by default.** Tier 0 requires only a POSIX shell and writes nothing.
- **The agent's shell snapshot is stripped** from forwarded commands. Claude Code's command envelope sources a file from your home directory; forwarding it would disclose your username and directory layout to the server for no benefit.

What reach does not protect against: prompt injection. Content from the target flows into the agent's context. A compromised server can try to instruct the agent. Keep secrets out of the agent's local environment, and review any writes the agent makes to your local disk during a remote session. Full threat model: [docs/SECURITY.md](docs/SECURITY.md).

---

## Docs

| | |
|---|---|
| [ARCHITECTURE.md](docs/ARCHITECTURE.md) | how it fits together, and what was rejected |
| [TRANSPORTS.md](docs/TRANSPORTS.md) | the three file-operation tiers, measured on real links |
| [RESEARCH.md](docs/RESEARCH.md) | what each agent actually does internally, with transcripts |
| [SECURITY.md](docs/SECURITY.md) | threat model and what reach does not protect you from |
| [WINDOWS.md](docs/WINDOWS.md) | running reach from Windows |
| [CONTRIBUTING.md](CONTRIBUTING.md) | the standard of evidence and the design rules |
| [harnesses/](docs/harnesses/) | per-agent notes: seam, verified versions, known limits |

---

## Development

```console
make check        # vet + unit tests. No network, no API key.
make integration  # every file-operation tier against a real sshd
make bench        # what each tier actually costs, measured
make conformance  # do the agent seams still have the shape reach expects?
make e2e          # real agents against a real target (spends tokens)
```

`make integration` starts an sshd owned by your user on a high port — no root, no Docker, no external network. Set `REACH_TEST_SSH_HOST=my-box` to run against a host you already have.

---

## Status

The Claude Code path is verified end-to-end against a real agent, against real hosts on three continents. The file-operation layer — three tiers, six real hosts, three userlands, a fuzzer — is solid. The other agents are at the stage each of [their notes](docs/harnesses/) says they are.

Interfaces may change before 1.0. What is *not* changing: the file-operation protocol, the session format, and the principle that nothing lands on your server without you asking for it.

---

## License

MIT — see [LICENSE](LICENSE).
