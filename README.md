# AgentReach

**Point your coding agent at any box you can SSH into. The server never gets your agent.**

[![CI](https://github.com/bojieli/agentreach/actions/workflows/ci.yml/badge.svg)](https://github.com/bojieli/agentreach/actions/workflows/ci.yml)
[![Go 1.25.8+](https://img.shields.io/badge/go-1.25.8%2B-00ADD8)](https://go.dev)
[![License](https://img.shields.io/badge/license-MIT-blue)](LICENSE)
[![Zero dependencies](https://img.shields.io/badge/dependencies-none-brightgreen)](go.mod)

<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/assets/architecture-dark.svg">
    <img src="docs/assets/architecture-light.svg" width="880"
         alt="The coding agent runs on your machine. reach catches its Bash, Read, Write and Edit calls before they touch the local disk and runs them over one ssh connection on the target instead. The API key and the conversation never leave your machine, and the target needs nothing but the sshd it already runs.">
  </picture>
</p>

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

## What reach actually does

The agent stays where it is. What moves is the work it hands off. An agent only
touches a machine two ways — it runs shell commands, and it reads or writes
files — and reach gets in the middle of both, on your side, before either one
reaches your disk.

**Commands.** reach takes over whatever the harness spawns as a shell, unwraps
the command, runs it over ssh on the target, and hands back stdout and an exit
code. As far as the model can tell, it called `Bash`. Every harness offers some
door in:

- Claude Code — `CLAUDE_CODE_SHELL_PREFIX`
- Goose — `GOOSE_SHELL`
- Codex — its remote-environment protocol
- opencode — a custom tool shadowing the built-in one
- anything else — reach's own `bash`, earlier on `PATH`, winning the lookup

**Files.** Depends on the mode:

- **exec mode** — the agent's own `Read`, `Write` and `Edit` are switched off.
  They call the local filesystem directly and there is no seam to redirect them
  through, so the agent falls back to its shell, which is already remote.
- **mirror mode** — reach answers those calls itself, pulling the file over the
  same ssh connection when a tool opens it and pushing it back when it changes.
  Wired up for Claude Code today.

**What this isn't.** No syscall tracing. No filesystem in any of it: no SSHFS,
no FUSE, no mount, no file synchronisation between the two machines. reach sits
at the seam where the agent hands work to the operating system, one request and
one response at a time.

[HOW-IT-WORKS.md](docs/HOW-IT-WORKS.md) walks a single tool call through the
whole path, in figures.

## What it looks like

```console
$ reach build-box claude
probing ssh://build-box ...
session "build-box" -> ssh://build-box/home/you
  target   Linux x86_64
  fileops  pipe (negotiated; nothing written to the target)
  search   ripgrep (fast, structured)
  connect  multiplexed (one authenticated connection, reused)
reach: Claude Code -> ssh://build-box/home/you (bash runs on the target)

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

There is also a container image on GitHub Packages, for when the machine you
drive agents from is itself disposable — a CI job, a devcontainer, a jump box:

```console
docker run --rm -v "$HOME/.ssh:/root/.ssh:ro" ghcr.io/bojieli/agentreach version
```

It carries an `ssh` client and the helper binaries for every target platform.
Your keys and `known_hosts` have to be mounted in; there is nothing useful to
bake into an image, and reach will not invent credentials it was not given.

The only thing reach needs is the `ssh` binary you already have. It shells out
to that rather than speaking SSH itself, so `ProxyJump`, `IdentityFile`, `Match`
blocks, hardware tokens and 2FA all keep working the way you've configured them.

You can sit in front of Linux, macOS or Windows. Targets can be anything POSIX
you can reach over `ssh://`, `docker://`, `podman://`, or the usual
`user@host:/path` shorthand.

## Quick start

Name a machine, then name an agent:

```console
reach build-box claude                        # an ssh_config alias; work where a login lands
reach ssh://build-box/srv/app claude          # ... or in a directory you name
reach client-box:/srv/app codex               # somebody else's server
```

reach opens one multiplexed SSH connection, asks the host what it can do, and
launches the agent locally with its shell quietly redirected there. You talk to
it like you always do. Nothing is written to that host unless you ask for the
helper tier by name, and `reach doctor` tells you exactly what was found, which
tier got picked, and whether anything is sitting on the target.

Any command takes a target, not just the agents:

```console
reach build-box exec -- go test ./...
reach build-box doctor
```

Each target gets its own session, named after it, so **several machines can be
open at once** — one terminal per box, and no bookkeeping to keep them apart:

```console
$ reach status
NAME            TARGET                    MODE  FILEOPS  CWD
build-box-app   ssh://build-box/srv/app   exec  pipe     /srv/app
client-box-app  ssh://client-box/srv/app  exec  posix    /srv/app
```

A second command against a target reuses that session instead of probing again,
so running one costs no more than typing it.

When you're finished:

```console
reach down build-box-app
```

That closes the connection and leaves nothing behind.

For a session meant to outlive one agent — several commands, several days, a
name you chose — bind it up front and address it by name:

```console
reach up ssh://build-box/srv/app --name build
reach claude --session build
reach down build
```

## Agents that work today

| Agent | Command | Where reach gets in | Status |
|---|---|---|---|
| [Claude Code](docs/harnesses/claude-code.md) | `reach claude` | `CLAUDE_CODE_SHELL_PREFIX`, a hook it already ships | verified end-to-end (2.1.233) |
| [Codex](docs/harnesses/codex.md) | `reach codex` | its remote-environment protocol, which carries every tool it has | verified end-to-end (0.148.0) |
| [Kimi Code](docs/harnesses/kimi.md) | `reach kimi` | a patched npm bundle plus `KIMI_SHELL_PATH` | verified (0.37.2) |
| [opencode](docs/harnesses/opencode.md) | `reach opencode` | custom tools that shadow the built-in `bash` and `read` | verified (1.18.18) |
| [Goose](docs/harnesses/goose.md) | `reach goose` | `GOOSE_SHELL`, a documented override | verified |
| [Crush](docs/harnesses/crush.md) | `reach crush` | its own server mode, run on the target | verified |
| [Gemini CLI](docs/harnesses/gemini.md) | `reach gemini` | a `bash` earlier on `PATH`, plus `excludeTools` for the rest | verified |

Those fall into three groups, plus one exception, and the group decides how much
of the agent survives the trip.

Codex and opencode are the clean ones. Both document a way to change the machine
their tools act on, so reach answers at the other end and the model keeps every
tool it started with. Codex is the best fit reach has, because it has no file
tools at all: `apply_patch` and the rest run as commands inside `exec_command`,
so intercepting that one protocol leaves nothing behind to deny.

Claude Code, Goose and Kimi hand over the shell and only the shell. Their file
tools call straight into Node's `fs` or Rust's `std::fs`, so reach denies them
and the agent works through its shell instead, or in Claude Code's case mirrors
them if you ask for mirror mode.

Gemini gives you no hook at all, so reach wins the `PATH` lookup for `bash` and
hides the rest of the built-ins through `excludeTools` in a managed
`settings.json`.

Crush is the exception to the nothing-installed rule. Its server mode is exactly
the seam reach wants, and `reach crush` starts `crush server` on the target and
tunnels the client to it, which means `crush` itself has to already be there.

Whichever group you land in, nobody logs in again. Subscription logins, OAuth
tokens and API keys keep working exactly as they do now, because reach never
touches them.

## Commands

```console
reach <target> <cmd>    bind a session to the target and run the command there
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
build-box                      an ssh_config alias, or a name in /etc/hosts
ssh://user@host/path/to/work
user@host:/path/to/work        (scp-style shorthand)
docker://container/path
podman://container/path
local:///path                  (this machine, mostly for testing)
```

Leave the path off any of them — `build-box`, `ssh://build-box` — and the
session works wherever a login on that machine lands, which reach asks the
machine for rather than guessing. A bare word is read as a host only when your
own ssh configuration or hosts file names it, or when it is an address or a
dotted name; anything else is reported as a mistyped command, which is what it
almost always is.

Session flags go between the target and the command, and everything from the
command onwards belongs to the command:

```console
reach build-box --mode mirror --fresh claude --resume
```

## Why not SSHFS, an MCP server, or just SSH?

Four things look like they should work. Each breaks somewhere specific.

- **Install the agent on the server.** 300 MB of Node on a machine that isn't
  yours, and your API key with it, on a box where other people have root.
- **Run an MCP file server on the target.** The model sees
  `mcp__remote__read_file` where it expected `Read`. You haven't moved the
  agent, you've retrained it.
- **Let the agent ssh in from its own shell.** Every command is a separate
  login, so `cd` stops persisting and relative paths drift — and the agent's own
  `Read` and `Write` still edit your laptop.
- **Mount the target with SSHFS or FUSE.** macOS wants a kernel extension and a
  reboot. Worse, when the link stalls the read is stuck inside the kernel: the
  tool call never returns, and the model gets no error it could retry.

<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/assets/not-a-mount-dark.svg">
    <img src="docs/assets/not-a-mount-light.svg" width="880"
         alt="A mount turns one tool call into a stream of syscalls through the kernel's VFS and a FUSE daemon, so a stalled link parks the agent in uninterruptible sleep with no error it can see. reach answers each tool call with one request and one response over ssh, so a timeout comes back as a tool error the model can read, retry or route around. Nothing is mounted and no kernel is involved.">
  </picture>
</p>

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
rather than loopback. [HOW-IT-WORKS.md](docs/HOW-IT-WORKS.md) follows a single
`Bash` call all the way down and back — the envelope reach takes apart, how `cd`
survives between calls, and where the exit status hides.

## Choosing a mode

`exec` is the default, and it's the one to stay on unless you have a reason not
to. `--mode mirror` earns its keep during heavy editing sessions, when you'd
rather use Claude Code's native file tools than push every change through a shell
redirect:

```console
reach ssh://build-box/srv/app --mode mirror claude
```

Mirror keeps a content digest for each file it hands over, so if the file changed
on the server between the read and the write, the write is refused rather than
clobbering someone else's work. Go in knowing what it is, though: reads can be a
little stale, and it copies on demand rather than syncing. For shell-shaped work
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
| [HOW-IT-WORKS.md](docs/HOW-IT-WORKS.md) | the implementation in figures, and what reach is deliberately not |
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

## Acknowledgements

reach exists because [Zihan Zheng](https://github.com/zzh1996) (@zzh1996)
proposed the idea.

## License

MIT. See [LICENSE](LICENSE).
