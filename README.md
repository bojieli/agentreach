# AgentReach

**Point your coding agent at any box you can SSH into. The server never gets your agent.**

[![CI](https://github.com/bojieli/agentreach/actions/workflows/ci.yml/badge.svg)](https://github.com/bojieli/agentreach/actions/workflows/ci.yml)
[![Go 1.25.8+](https://img.shields.io/badge/go-1.25.8%2B-00ADD8)](https://go.dev)
[![License](https://img.shields.io/badge/license-MIT-blue)](LICENSE)
[![Zero dependencies](https://img.shields.io/badge/dependencies-none-brightgreen)](go.mod)

Claude Code keeps running on your laptop, logged in the way it already is. Only
its shell and its file tools move. The target needs nothing but the `sshd` it's
already running: nothing gets installed there, no credential of yours goes there,
and closing the session leaves nothing to clean up.

## Why this exists

Think about the last time you SSHed somewhere to fix something. A build box, a
client's staging VM, a container you spun up this morning and will throw away
tonight. You'd get through it faster with an agent riding along.

So you install one there. That's a 300 MB Node binary on a machine that isn't
yours, plus an API key pasted in so the thing can actually do anything. Now your
key lives on a box where other people have root, in a shell history you won't
clear, in an env file you'll forget about by Friday. If that server gets popped
next month, your key goes with it.

Dev containers are the same story on repeat. New branch, new container, and each
one wants its own copy of the agent. Baking it into the Dockerfile bloats an
image that should have stayed small. Mounting it in from the host works right up
until the morning it doesn't.

reach turns the arrangement around. The agent runs where it already lives and
where its credentials already are. Only the commands travel. The target gets
those commands over SSH and nothing else: no runtime, no key, no files left
sitting there after you're done.

## What it looks like

```console
$ reach up ssh://build-box/srv/app
probing ssh://build-box/srv/app ...
session "default" -> ssh://build-box/srv/app
  target   Linux x86_64
  fileops  pipe (negotiated; nothing written to the target)
  search   ripgrep (fast, structured)
  connect  multiplexed (one authenticated connection, reused)

$ reach claude
> what's eating disk on this box?
```

Claude Code is running locally. The `df` and `du` and `grep` it decides to run
happen on `build-box`. Files it reads come from `build-box`. If it rewrites a
config, the new bytes land on `build-box`. Your own filesystem is untouched
unless you specifically ask for something local.

Everything it did is on the record:

```console
$ reach log
WHEN      ACTION  STATUS  DETAIL
14:02:11  exec    ok      df -h /
14:02:19  exec    ok      du -sh /var/log/*
14:02:30  write   ok      /srv/app/logrotate.conf
14:02:31  exec    exit 1  systemctl reload rsyslog
```

## Install

```console
go install github.com/bojieli/agentreach/cmd/reach@latest
```

Pre-built binaries are on the [releases page](https://github.com/bojieli/agentreach/releases).
From source:

```console
git clone https://github.com/bojieli/agentreach
cd agentreach
make install
```

The only thing reach needs is the `ssh` binary you already have. It shells out
to that rather than speaking SSH itself, so `ProxyJump`, `IdentityFile`, `Match`
blocks, hardware tokens and 2FA all keep working the way you've configured them.

You can sit in front of Linux, macOS or Windows. Targets can be anything POSIX
you can reach over `ssh://`, `docker://`, `podman://`, or the usual
`user@host:/path` shorthand.

## Quick start

Point reach at something:

```console
reach up ssh://build-box/srv/app                     # a server you own
reach up ssh://client-box/srv/app --untrusted        # somebody else's server
```

This opens one multiplexed SSH connection and asks the host what it can do.
`--untrusted` promises that the optional helper binary will never be installed
there, no matter what. Either way, `reach doctor` will tell you exactly what was
found, which tier got picked, and whether anything is sitting on the target.

Then start your agent:

```console
reach claude       # Claude Code
reach codex        # Codex
reach goose        # Goose
```

It launches locally, with its shell quietly redirected to the remote host, and
you talk to it like you always do.

When you're finished:

```console
reach down
```

That closes the connection and leaves nothing behind.

## Agents that work today

| Agent | Command | Status |
|---|---|---|
| [Claude Code](docs/harnesses/claude-code.md) | `reach claude` | verified end-to-end (2.1.233) |
| [Codex](docs/harnesses/codex.md) | `reach codex` | verified end-to-end (0.148.0) |
| [Kimi Code](docs/harnesses/kimi.md) | `reach kimi` | verified (0.37.2) |
| [opencode](docs/harnesses/opencode.md) | `reach opencode` | verified (1.18.18) |
| [Goose](docs/harnesses/goose.md) | `reach goose` | verified |
| [Crush](docs/harnesses/crush.md) | `reach crush` | verified |
| [Gemini CLI](docs/harnesses/gemini.md) | `reach gemini` | verified |

Nobody has to log in again. Subscription logins, OAuth tokens and API keys keep
working exactly as they do now, because reach never touches them.

## Commands

```console
reach up <target>       bind a session to a target and probe what it supports
reach down [session]    close a session and leave no trace
reach status            show active sessions
reach doctor            explain what a target supports, and why
reach log               what reach has run and changed on the target
reach exec -- <cmd>     run a one-off command there
reach fs read <path>    work with remote files directly
reach helper uninstall  remove anything reach put on the target
```

Targets look like this:

```
ssh://user@host/path/to/work
user@host:/path/to/work        (scp-style shorthand)
docker://container/path
podman://container/path
local:///path                  (this machine, mostly for testing)
```

## Things that look like they should work

| | what actually happens |
|---|---|
| **Install the agent on the server** | 300 MB of Node, and your API key is now on a machine you don't administer |
| **SSHFS or a FUSE mount** | macOS wants a kernel extension and a reboot. Worse, on a flaky link the mount hangs in uninterruptible sleep and the agent freezes mid-call with no error it can see |
| **An MCP file server** | The model now sees `mcp__remote__read_file` where it expected `Read`. You didn't relocate the agent, you retrained it |
| **Just SSH inside the agent's terminal** | `cd` stops persisting between calls, relative paths drift, and the agent's file tools are still happily editing your laptop |

There's one rule underneath all of this:

> Every failure has to arrive as a value the agent can reason about, never as a
> process that stops answering.

A timeout should show up as a tool error the model can retry or route around.
That's the reason reach is request/response over SSH instead of a filesystem
mount, and it's the thing most of the design falls out of.

## How it works

Three layers, and only the top one knows which agent you're running.

```
harness  (claude · codex · kimi · opencode · goose · crush · gemini)
    │     native tool calls, no new tools in the model's view
adapter   one per harness, config or plugin, never a fork
    │
reach     session state · cwd · capability probe
    │     file-op tier: posix · pipe · helper
    │  ssh · docker · podman · local
target    stock sshd, nothing installed
```

There's no daemon. A session is just a file on disk. SSH's `ControlMaster`
already multiplexes the connection at four to five times the speed of
reconnecting, so a daemon would buy lifecycle complexity and nothing else.

By default nothing gets written to the target. Two of the three file-operation
tiers touch its disk zero times, and those two are the only ones reach will
choose on its own. The third uploads a small helper, and only if you ask for it
by name.

| tier | needs on target | writes to target | chosen automatically |
|---|---|---|---|
| `posix` | a POSIX shell | nothing | yes, as fallback |
| `pipe` | `python3` | nothing | yes, preferred |
| `helper` | can run an uploaded binary | one cached binary | never |

[TRANSPORTS.md](docs/TRANSPORTS.md) has the numbers, measured over real links
rather than loopback.

## Two modes

**exec** is the default. Commands run on the target, and the agent's own file
tools (`Read`, `Edit`, `Write`) are denied, because there's no way to redirect
them and they'd quietly edit your laptop instead. The agent works through its
shell tool, which is remote whether it knows it or not.

**mirror** additionally wires Claude Code's native `Read`, `Write` and `Edit` to
the target. A file is fetched the moment a tool opens it and written back when it
changes, with a content digest in between, so if the file changed underneath you
on the server the write is refused instead of clobbered.

```console
reach up ssh://build-box/srv/app --mode mirror
reach claude
```

Mirror earns its keep during heavy editing sessions where you'd rather use the
native file tools than shell redirects. Go in knowing what it is: reads can be a
little stale, and it copies on demand rather than syncing. For shell-shaped work,
exec is simpler and has no staleness window at all.

## Security

reach assumes the target might be hostile, and the design reflects that.

Your credentials never make the trip. The agent, its API key and the whole
conversation stay on your machine.

SSH agent forwarding is off and stays off. A forwarded agent socket on a hostile
server lets whoever controls that server authenticate as you everywhere else you
can reach, which is a much bigger blast radius than the session you thought you
were opening. reach never enables it and there is no flag that does.

Nothing is installed by default. Tier 0 needs a POSIX shell and writes nothing at
all.

The agent's shell snapshot gets stripped out of forwarded commands. Claude Code's
command envelope sources a file from your home directory, and shipping that to
the server would hand over your username and directory layout for no benefit.

What reach can't do anything about is prompt injection. Content from the target
flows into the agent's context, and a compromised server can absolutely try to
talk to your agent. Keep secrets out of the agent's local environment, and read
anything it writes to your local disk during a remote session. The full threat
model is in [docs/SECURITY.md](docs/SECURITY.md).

## Docs

| | |
|---|---|
| [ARCHITECTURE.md](docs/ARCHITECTURE.md) | how the pieces fit, and which alternatives got thrown out |
| [TRANSPORTS.md](docs/TRANSPORTS.md) | the three file-operation tiers, benchmarked on real links |
| [RESEARCH.md](docs/RESEARCH.md) | what each agent does internally, with transcripts |
| [SECURITY.md](docs/SECURITY.md) | threat model, and what reach won't save you from |
| [WINDOWS.md](docs/WINDOWS.md) | running reach from Windows |
| [CONTRIBUTING.md](CONTRIBUTING.md) | the standard of evidence, and the design rules |
| [harnesses/](docs/harnesses/) | per-agent notes: the seam, verified versions, known limits |

## Development

```console
make check        # vet plus unit tests. No network, no API key.
make integration  # every file-operation tier against a real sshd
make bench        # what each tier actually costs
make conformance  # do the agent seams still have the shape reach expects?
make e2e          # real agents against a real target (spends tokens)
```

`make integration` starts an sshd owned by your own user on a high port. No root,
no Docker, no outbound network. If you'd rather test against a box you already
have, set `REACH_TEST_SSH_HOST=my-box`.

## Status

The Claude Code path is verified end-to-end against a real agent on real hosts
across three continents. The file-operation layer has been through three tiers,
six hosts, three userlands and a fuzzer, and it's solid. Every other agent is
exactly as far along as [its notes](docs/harnesses/) say it is.

Interfaces may still move before 1.0. Three things won't: the file-operation
protocol, the session format, and the promise that nothing lands on your server
unless you asked for it.

## License

MIT. See [LICENSE](LICENSE).
