# How reach works

The [README](../README.md) says what reach does. [ARCHITECTURE.md](ARCHITECTURE.md)
says why it is built this way and which alternatives were thrown out. This
document is for the reader in between: what actually happens, in pictures, and
what reach is deliberately *not* — because most of the guesses people make about
the implementation are guesses about a filesystem, and there is no filesystem
here.

The whole mechanism in one sentence: **reach sits at the seam where a coding
agent hands a piece of work to its operating system, and answers that handoff
over an ssh connection instead of on your disk.** One request, one response, one
tool call at a time.

## What reach is not

<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/not-a-mount-dark.svg">
    <img src="assets/not-a-mount-light.svg" width="880"
         alt="A mount turns one tool call into a stream of syscalls through the kernel's VFS and a FUSE daemon, so a stalled link parks the agent in uninterruptible sleep. reach answers each tool call with one request and one response over ssh, so a timeout comes back as a tool error the model can read.">
  </picture>
</p>

### It is not SSHFS, or FUSE, or a mount of any kind

reach never calls `mount(2)`. No kernel module, no kernel extension, no mount
point, and nothing under `/Volumes` or `/mnt`, on either machine.

This is a design decision, not an omission, and it comes from the one rule the
whole project is built on:

> Every failure must be a value the agent can reason about, never a process that
> stops responding.

A mount makes that impossible. It turns one tool call into a stream of syscalls
that go through the kernel's VFS layer, and when the link stalls, the calling
process parks in uninterruptible sleep — the agent freezes in the middle of a
tool call, with no error it can see, no timeout it can honour, and nothing to
report to the model. A timeout in reach is a tool result the model reads,
retries or routes around. On macOS a FUSE mount also wants a kernel extension
and a reboot before it will start at all.

There is a second, quieter reason. `Grep` and `Glob` are first-class operations
in reach and execute **on the target**, so only matches cross the network. Over
a mount, searching a tree means dragging every candidate file across the link to
answer a question the target could have answered locally.

### It does not synchronise files

There is no rsync, no watcher, no bulk copy of the workspace, no background
reconciliation, and nothing that runs while you are not looking.

In the default `exec` mode, **not one byte of the target's files is written to
your disk.** The agent's file access happens through its shell, which is already
remote: `cat`, `rg`, `sed` and friends run on the target, and only their output
comes back.

`mirror` mode — opt-in, Claude Code only — copies exactly one file, at the
moment a tool opens it, and pushes it back when the tool changes it. Nothing is
fetched until a tool asks for it, and a write is refused if the file changed on
the target in between. That is a fetch, not a sync: there is nothing to
reconcile, because nothing is being kept in step.

### It is not an MCP server, and the model sees no new tools

The model still calls `Bash` and gets back stdout and an exit code. It still
calls `Read` where a harness has a seam for it. Nothing is renamed to
`mcp__remote__read_file`, so no retraining, no prompt changes, and no tool the
model has to be taught to prefer. Where reach cannot redirect a tool honestly,
it denies that tool rather than letting it act on the wrong machine.

### The agent does not run on the target

The harness process, its API key or OAuth token, and the entire conversation
stay on your machine. The target receives shell commands and, at one file-op
tier, a program on stdin that is never written to its disk. It sends back bytes,
exit statuses and error values.

By default reach writes **nothing** to the target. `reach doctor` says so in
those words, and lists any exception.

### There is no daemon

No background service, no socket of reach's own, no lifecycle to manage. A
session is a JSON file under `~/.reach/sessions/`, and every reach invocation is
a short-lived process that reads it, does one thing, and exits.

The one long-lived process involved is `ssh` itself: OpenSSH's `ControlMaster`
keeps the authenticated connection open so later tool calls reuse it instead of
reconnecting — measured at 4–5× faster per command on real links. `reach down`
ends it. (Win32-OpenSSH has no `ControlMaster`, so reach probes for multiplexing
rather than assuming it; see [WINDOWS.md](WINDOWS.md).)

### No harness is forked

reach uses seams the harnesses already ship — an environment variable, a
documented config key, a hook, a remote-environment protocol — and generates
config rather than patching binaries. Two honest exceptions, both documented
where they live: Kimi Code's shipped npm bundle spawns its shell by absolute
path and needs [a patch](../contrib/) before `KIMI_SHELL_PATH` is honoured, and
`reach crush` starts `crush server` on the target, so that one harness does have
to be installed there.

## The seam: what an agent asks its machine for

An agent only touches a machine two ways. It runs a command, and it reads or
writes a file. Every adapter in reach is one answer to the question *where can
this harness be asked to send those two things?*

| harness | where reach gets in | shell | file tools |
|---|---|---|---|
| Claude Code | `CLAUDE_CODE_SHELL_PREFIX`, plus its own hooks in mirror mode | remote | denied in `exec`; mirrored in `mirror` |
| Codex | its remote-environment protocol (`environments.toml` → `reach exec-server`) | remote | remote — the protocol carries every tool it has |
| opencode | generated tools that shadow the built-ins by name | remote | remote |
| Goose | `GOOSE_SHELL` | remote | denied (`available_tools: [shell]`) |
| Kimi Code | `KIMI_SHELL_PATH` via a patched npm bundle | remote | denied |
| Gemini CLI | a `bash`/`sh`/`zsh` earlier on `PATH` | remote | denied (`excludeTools`) |
| Crush | its own server mode, run on the target | remote | remote |

Two things follow from this table, and they are the honest limits of the design.

**A denied tool is a safety property.** Claude Code's `Read` calls Node's `fs`
directly. There is no seam in front of it, so if reach left it enabled it would
keep reading your laptop while the model believed it was reading the server —
confident nonsense at best, and a `Write` that destroys your own work at worst.
Where reach cannot redirect, it denies, and tells the model which shell command
to use instead.

**The best harnesses are the ones that document a machine boundary.** Codex is
the best fit reach has: it has no native file tools at all — `apply_patch` and
the rest run as commands inside `exec_command` — so intercepting that one
protocol leaves nothing to deny.

Because these seams are undocumented implementation details of closed binaries,
`reach harness verify` drives the real harness against an offline mock model and
checks *where a scripted command actually ran*. A version measured to bypass its
seam is refused at launch rather than run against the wrong machine.

## One Bash tool call, end to end

<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/toolcall-dark.svg">
    <img src="assets/toolcall-light.svg" width="880"
         alt="The model calls Bash. Claude Code wraps the command and runs the program named by CLAUDE_CODE_SHELL_PREFIX, which is a symlink to reach. reach reads the session file, strips the two local-only segments of the envelope, and sends the command over the ssh connection the session already holds. sshd runs it in a shell with nothing installed. The output, exit status and resulting directory come back, and reach records the directory locally so cd keeps working.">
  </picture>
</p>

A few details in that path are worth spelling out, because they are where the
work actually is.

**The shell prefix is a symlink, not a script.** Harnesses want a *program path*
for their shell hook, not a command line — Claude Code stats the value of
`CLAUDE_CODE_SHELL_PREFIX` — so `reach shell-prefix` would be looked up as one
filename and fail. reach installs an alias of itself at
`~/.reach/bin/reach-shell-prefix` and dispatches on `argv[0]`. No wrapper
script, no extra process per tool call.

**The envelope is taken apart, not forwarded.** Claude Code wraps every Bash
call:

```sh
source <LOCAL_SNAPSHOT>.sh 2>/dev/null || true
  && { shopt -u extglob || … ; } >/dev/null 2>&1 || true
  && eval '<USER COMMAND>' < /dev/null
  && pwd -P >| /tmp/claude-<rand>-cwd
```

Two segments must not travel:

- The **snapshot** restores the *local* shell's functions, aliases and `PATH`.
  On the target it is at best a no-op, and it embeds your username and directory
  layout — on a client's server, a disclosure reach would be causing. Stripped.
- The **`pwd -P` redirect** is how Claude Code persists `cd` between calls: it
  writes the resulting directory to a local temp file and reads it back after
  the command returns. Forwarded verbatim it would be written on the *remote*
  filesystem while the harness reads the local path, and `cd` would silently
  stop working. reach strips it, tracks the directory itself, and writes the
  local file the envelope named.

Envelope shape is version-specific, so `internal/envelope` parses defensively,
forwards the whole string unchanged when it does not recognise the shape, and
has a conformance test that fails when a Claude Code upgrade changes it.

**The exit status rides back in-band.** `ssh` reports its own failures as exit
255, which is indistinguishable from a command that genuinely exited 255. reach
carries the real status behind an unguessable marker, and the marker's *absence*
is the signal that the transport, rather than the command, failed. Getting this
backwards in either direction is bad: a transport failure reported as a command
failure sends the agent chasing a phantom bug, and a command failure reported as
a transport failure makes reach retry something that must not be retried.

**One process per tool call, one channel per command.** That is why there is no
daemon to write: `ControlMaster` already supplies the only thing a daemon would
have bought. (Under Codex it is one process per *session* instead — the
exec-server is spawned once and speaks JSON-RPC on stdio for the whole run.)

## Files, without a filesystem

`internal/fileops` defines ten operations — read, write, stat, list, mkdir,
remove, rename, search, glob, hash — and three interchangeable strategies
implement them. They share almost no code, a user cannot tell which is in use,
and all three run one identical conformance suite over a real sshd, including
the property that a file written through any tier reads back byte-for-byte
through every other.

The rule the set is chosen by: **every tier answers one file operation in one
network round trip.** (An SFTP tier existed and was deleted, because `READ`
needs a handle that only `OPEN`'s response can give it — two round trips
minimum, in the protocol itself. The reasoning is kept in
[TRANSPORTS.md](TRANSPORTS.md#why-there-is-no-sftp-tier).)

| tier | needs on the target | writes to the target | chosen automatically |
|---|---|---|---|
| `posix` | a POSIX shell | nothing | yes, as fallback |
| `pipe` | `python3` | nothing | yes, preferred |
| `helper` | can run an uploaded binary | one cached binary | never |

Reading `/srv/app/main.go` looks like this:

- **`posix`** — one ordinary command, and the answer is its stdout:

  ```sh
  tail -c +1 -- '/srv/app/main.go' | head -c 1048576
  ```

  The capability probe that runs when a session is bound proves whether the
  link is 8-bit clean, by piping every byte value through the target's own
  digest command and having the target print them back. Content moves unencoded
  where that holds, and falls back to base64 — always safe, a third more
  bandwidth — where it does not.

- **`pipe`** — a stdlib-only Python handler runs on the target and speaks a
  length-framed protocol over one long-lived channel:

  ```
  uint32 header_len | {"id":7,"op":"read","path":"/srv/app/main.go","offset":0,"limit":8388608} | uint32 payload_len | payload
  ```

  File content travels in the raw payload beside the JSON, never inside it, so
  a NUL byte or invalid UTF-8 cannot be mangled by a text codec on the way.

  The handler is **never written to the target's disk**: reach base64-encodes it
  into the first line of stdin, a one-line bootstrap `exec`s it, and everything
  after that newline is protocol. `exec` replaces the shell with the
  interpreter, so closing the channel kills the handler instead of leaving an
  orphan on someone else's machine.

- **`helper`** — the identical protocol with a small static Go binary on the far
  end. It is the only tier that writes to a target, so it is never chosen
  automatically, refused outright on an `--untrusted` session, listed by
  `reach doctor`, and removed by `reach helper uninstall`.

`Search` and `Glob` are first-class here rather than derived from `List`, at
every tier, because that is the difference that matters most in practice: `rg`
runs on the target and only the matches come back.

## The two modes

<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/modes-dark.svg">
    <img src="assets/modes-light.svg" width="880"
         alt="In exec mode Bash runs on the target and the native file tools are denied, so the agent reads and writes through its shell and nothing is copied to your disk. In mirror mode Read, Write and Edit are re-pointed by a hook at one file reach fetches when the tool opens it and writes back when it changes; Grep and Glob stay denied because the mirror holds only files already opened.">
  </picture>
</p>

`exec` is the default and the one to stay on. `mirror` exists for one case:
it must be Claude Code, *and* it must be the native file tools. Its costs are
stated plainly in [ARCHITECTURE.md](ARCHITECTURE.md#two-modes) — reads can be
stale, an `Edit` is a whole-file read-modify-write over the network, and `Grep`
and `Glob` stay denied even there. Where a harness offers a direct seam
(opencode's tool shadowing, Codex's exec-server), that mechanism executes file
operations *on the target* with no copy and no staleness window, and it is
strictly better.

## Where the state lives

```
~/.reach/
├── bin/reach-shell-prefix          an alias of the reach binary; the harness's shell hook
├── sessions/
│   ├── build-box-app.json          target · mode · negotiated tier · capability probe
│   ├── build-box-app.cwd           the working directory, carried between tool calls
│   └── build-box-app.audit.jsonl   every command run and file changed, one JSON line each
├── conf/                           generated harness config (deny lists, hooks, settings)
└── mirror/<session>/               mirror mode only: the files a tool has actually opened

$XDG_RUNTIME_DIR/reach/  (or your temp dir)
└── c-<hash>-0.sock                 ssh's control socket — the connection, reused
```

The target names the session, so several machines can be open at once without
bookkeeping: `reach build-box claude` and `reach client-box claude` are two
sessions, two connections and two files. A tool call finds its own machine
through `$REACH_SESSION`, which the launcher sets for the agent it starts.

The session file carries a schema version, and one written by a newer reach is
refused rather than partly read: `encoding/json` drops fields it does not
recognise without a word, and a session reach has only partly understood is
exactly the uncertainty about *which machine* this project exists to remove.

## Check any of it yourself

None of the claims above need to be taken on trust.

```console
# Nothing is mounted — run this on your machine and on the target, before and after binding a session
$ mount | grep -i -e reach -e sshfs -e fuse
(no output)

# Between tool calls nothing of reach's is running. The only long-lived process is
# ssh's own multiplexing master, holding the connection open for the next call.
$ ps ax | grep '[r]each'
51234  ??  Ss  0:00.03 ssh: /var/folders/…/reach/c-9f2ac71e0b44-0.sock [mux]

# Nothing was put on the target
$ reach build-box doctor
  ...
  reach has written nothing to this target.

# Commands really run over there
$ reach build-box exec -- 'hostname; pwd; id -un'

# In exec mode, no content from the target is cached locally
$ ls -R ~/.reach/mirror 2>/dev/null
(nothing)

# And everything the agent did is on the record
$ reach build-box log
```

## Further reading

| | |
|---|---|
| [ARCHITECTURE.md](ARCHITECTURE.md) | the design decisions and the rejected alternatives |
| [TRANSPORTS.md](TRANSPORTS.md) | the three file-operation tiers, benchmarked on real links |
| [RESEARCH.md](RESEARCH.md) | what each harness does internally, with transcripts |
| [SECURITY.md](SECURITY.md) | the threat model, and what reach will not save you from |
| [harnesses/](harnesses/) | per-agent notes: the seam, verified versions, known limits |
