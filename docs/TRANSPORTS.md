# Transport tiers

waldo separates *how it reaches a target* (transport) from *how it performs file
operations* (file-op strategy). File-op strategies are tiered by what the target
can support, and waldo negotiates automatically. Every tier is selectable
explicitly with `--fileops=<tier>`; `waldo doctor` reports which tiers a given
host qualifies for and why.

The guarantee: **tier 0 works on any host running stock `sshd` and requires
nothing installed, nothing written to disk, and no subsystem beyond a login
shell.** Higher tiers are optimisations, never requirements.

| tier | name | remote requirement | writes to remote disk |
|---|---|---|---|
| 0 | `posix` | a POSIX shell | none |
| 1 | `pipe` | `python3` present | none |
| 2 | `agent` | ability to execute an uploaded binary | one cached binary |

**Every tier answers one file operation in one network round trip.** That is the
rule the set is chosen by, not an accident of which protocols were available —
see [Why there is no SFTP tier](#why-there-is-no-sftp-tier).

## What they actually cost

Measured twice, because the first measurement was taken in the wrong place.

Both tables drive waldo through its CLI, one process per operation, which is how
a harness drives it. Reproduce with `make bench`, or against your own host with
`WALDO_BENCH_SSH_HOST=my-box make bench`.

**At zero latency**, the tiers are not really what is being measured. Same
machine, same `local://` transport, same work — only the operating system
differs:

| 40 × 1 KiB read | macOS arm64 | Linux x86_64 |
|---|---|---|
| `posix` | 1.47 s | **0.20 s** |
| `pipe` | 1.94 s | 0.87 s |
| `agent` | 0.95 s | 0.47 s |

Tier 0 is 7× more expensive on the macOS target, and waldo's own startup is not
the reason — 40 invocations of `waldo version` take 0.13 s. The cause is that
tier 0 spawns a process per read on the target, and macOS creates processes far
more slowly than Linux. A loopback benchmark therefore mostly measures the
target's process spawner. Useful to know — **if your target is macOS, tier 0
costs disproportionately more** — but not what should decide anything.

**Over a real link**, a host ~171 ms per command, median of three runs:

| tier | 15 × 1 KiB read | 8 MiB read | 8 MiB write |
|---|---|---|---|
| `posix` | 5.97 s | 6.42 s | 5.73 s |
| `pipe` | **4.88 s** | **5.30 s** | **5.19 s** |
| `agent` | 10.83 s | 5.60 s | 6.50 s |

`pipe` wins on every axis because it moves a whole file in one frame with no
per-operation process on the target. `posix` is close behind, and much closer
than it used to be, because it no longer base64-encodes on links proven 8-bit
clean. `agent` is fastest in bulk and slowest to start, which is binary launch
cost and nothing else — its advantage is per-operation, and waldo runs one
process per tool call, so batching never gets to amortise it.

## Tier 0 — `posix` (strict SSH, universal)

For targets where **nothing may be installed** and only shell access exists.
Every operation is an ordinary command over the SSH connection:

| op | implementation |
|---|---|
| read | `tail -c +N \| head -c M`, raw when the link is 8-bit clean |
| write | `> <tmp> && mv <tmp> <path>` (atomic rename), raw when clean |
| stat | `stat -c` / BSD `stat -f` |
| list | GNU `find -printf` with NUL records, else `find -exec stat` |
| search | `rg --json` if present, else `grep -rn` |
| glob | `find` with `-path` or `-name` |
| mkdir/rename/remove | `mkdir -p` / `mv` / `rm` |

Binary-safe in both directions, and only pays for it when it has to. waldo
proves the link is 8-bit clean during `waldo up` — piping every byte value
through the target's own digest command in one direction, and having the target
print them back in the other — and moves content unencoded when it is. A
transport that garbles anything keeps base64, which is unconditionally safe and
costs a third of the bandwidth.

That measurement is not decoration. It is why this tier now reads an 8 MiB file
faster than the SFTP tier does.

Reads and writes are offset-addressed, so a dropped connection resumes rather
than restarts.

`dd bs=1` is deliberately not used for ranges: it issues one syscall per byte,
which makes a large offset pathologically slow.

This tier is the floor, and the fallback whenever nothing higher can be proven.

## Tier 1 — `pipe`

A stdlib-only Python handler runs on the target and speaks a length-framed
protocol over one long-lived channel. **Nothing is ever written to the remote
filesystem** — the handler exists only in the memory of a process that dies with
the session.

The obvious design, `ssh host 'python3 -' < handler.py`, cannot work: the
interpreter reads its program from stdin until EOF, leaving no stdin for the
protocol afterwards. waldo instead runs a one-line bootstrap that decodes the
handler from the first line of stdin and executes it, so everything after that
newline is protocol:

```console
ssh host "exec python3 -c 'import sys,base64;exec(compile(base64.b64decode(sys.stdin.buffer.readline()),\"<waldo>\",\"exec\"))'"
```

File content travels as a raw payload beside the JSON header rather than inside
it, so binary files cross the wire unencoded and no text codec can mangle a NUL
byte or invalid UTF-8. Digests are computed on the target, so a file that has
not changed never crosses the network at all.

`exec` replaces the shell with the interpreter, so closing the channel kills the
handler rather than leaving an orphan on someone else's machine.

## Tier 2 — `agent` (auto-installed)

A small static Go binary, installed by waldo when this tier is selected. The
user is never asked to install anything by hand. It speaks the identical
protocol to tier 1 — one client serves both, because a second implementation
would be a second thing to keep honest.

This is the only tier that writes to a target, so everything about it is built
to be verifiable and reversible:

1. `uname -sm` resolves the target's OS and architecture. An unrecognised
   platform is an error, not a guess: a wrong guess leaves an unrunnable binary
   on a host that was supposed to stay untouched.
2. waldo looks for a build for that platform beside its own binary (release
   archives ship them), then in `~/.waldo/agent/`, then cross-compiles one with
   the local Go toolchain from a source checkout. Nothing is downloaded at run
   time: a tool that exists to touch nothing should not be fetching executables
   over the network to put on a client's server.
3. The binary is uploaded **using tier 0**, to
   `~/.cache/waldo/agent-<version>-<os>-<arch>` on the target. Bootstrapping the
   fast tier with the universal one means installation works on exactly the
   hosts waldo can already reach.
4. `waldo-agent --selftest` prints its version, a digest of itself, and its
   platform. waldo compares all three against the file it just sent, and
   reinstalls rather than trusting a mismatch. Version alone would accept a
   truncated upload; a digest alone would accept a binary left behind by a
   different release.

Properties:

- **Self-updating.** The version is part of the path, so an upgraded waldo
  installs a new agent rather than reusing a stale one.
- **Visible.** `waldo doctor` lists exactly what waldo has placed on the host,
  and says plainly when it has placed nothing.
- **Removable.** `waldo agent uninstall` deletes the cache directory. That path
  is derived by waldo, never from anything the target said.
- **Never automatic.** Autonegotiation stops below this tier. Writing to someone
  else's machine stays an explicit operator decision.
- **Refused on untrusted targets.** A session created with `--untrusted` cannot
  use this tier at all.

Set `WALDO_AGENT_BINARY` to use a build you produced yourself.

## Negotiation

```console
waldo up ssh://host/srv/app                  # negotiate the best proven tier
waldo up ssh://host/srv/app --fileops=posix  # pin tier 0 — install nothing, touch nothing
waldo up ssh://host/srv/app --fileops=agent  # opt in to the auto-installed helper
```

Negotiation order is `pipe`, then `sftp`, then `posix`, measured against a real
link rather than loopback — see
[What they actually cost](#what-they-actually-cost). It never selects `agent`.

Two rules make the outcome trustworthy:

- **A pinned tier is never substituted.** If `--fileops=sftp` cannot be built,
  waldo fails and explains why. Reporting a tier the session is not using is a
  lie the operator would act on.
- **An autonegotiated tier may degrade, but never silently.** The fallback and
  its reason are printed, recorded in the session, and shown by `waldo status`.

The tier is not merely chosen during `waldo up` — it is *built* once, to prove
it works. Recording a tier that turns out to be unusable would move the failure
out of `waldo up`, where an operator is present to act on it, and into the
middle of an agent's turn, where it looks like a broken tool.

## Why there is no SFTP tier

waldo had one, and removing it is the most useful thing this document records.

SFTP is the one structured file protocol every stock `sshd` already speaks, so a
tier built on it promised faster file access with nothing installed. It was
implemented, tested, measured — and then deleted, for two reasons that only
became clear once it was measured on a real network.

**It cannot answer a tool call in one round trip.** `READ` takes a handle, and
the handle only exists in `OPEN`'s response. That dependency is in the protocol,
so no amount of pipelining removes it: `OPEN` and `STAT` can share a round trip
because both take a path, but `READ` cannot join them, and version 3 has no
composite open-and-read. Two was the floor. Every surviving tier does it in one —
the shell tier sends a command and reads its output, the pipe and agent tiers
send a request and read a response.

**Its remaining advantage was bandwidth, and that turned out to be waldo's own
fault.** Tier 0 base64-encoded file content unconditionally, which costs a third
of the bandwidth, and SFTP did not. Once waldo started *proving* whether a link
is 8-bit clean and skipping the encoding when it is, the shell tier read 8 MiB
in 6.4 s against SFTP's 8.1 s, and matched it on small reads and writes. The
tier that existed to beat the shell on file content no longer beat it on any
axis.

What it cost while it lived: several hundred lines of hand-written protocol, and
three bugs — temporary filenames that collided between processes, writes that
were not pipelined and so took 79 s to move 8 MiB over a slow link, and four
serial round trips per read. All three were mistakes the shell tier's simplicity
makes impossible.

If you need file access on a target with no `python3` and nothing installable,
that is tier 0, and it is now as fast. If you want request/response without
`python3`, that is `--fileops=agent`, at the cost of the one thing waldo will
not do silently: writing a binary to the target.

## Conformance

The four tiers share almost no code, and a user cannot tell which is in use. So
all four run one identical suite (`internal/fileops/fileopstest`) covering NUL
bytes, invalid UTF-8, CRLF, empty files, 5 MiB payloads, offset reads, awkward
filenames, atomic overwrite, and not-found reporting.

`test/integration` runs that same suite over a real sshd and adds the property
that matters most: a file written through any tier reads back byte-for-byte
through every other, with matching digests. A tier that cannot pass does not
ship.
