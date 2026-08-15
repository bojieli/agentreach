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
| 1 | `sftp` | SFTP subsystem (OpenSSH default) | none |
| 2 | `pipe` | `python3` present | none |
| 3 | `agent` | ability to execute an uploaded binary | one cached binary |

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
tier 0 spawns `sh`, `tail`, `head` and `base64` *per read*, and macOS creates
processes far more slowly than Linux does. The higher tiers spawn one process
per session instead, so they are much less sensitive to it, and the ordering
flips: on a Linux target `posix` is the fastest tier for small reads, on a macOS
target it is the slowest.

The lesson is that a loopback benchmark mostly measures the target's process
spawner. Useful to know — **if your target is macOS, tier 0 costs
disproportionately more** — but not what should decide the negotiation order.

**Over two real links**, measured the way waldo uses them. (Measure with
`ssh -o ControlPath=… host true` in a loop. ICMP is not a proxy for it: on a
tunnelled or split-DNS setup ping is answered by the local client, and reported
0.3 ms for hosts whose commands actually cost 540 ms.)

A host ~171 ms per command, median of three runs:

| tier | 15 × 1 KiB read | 8 MiB read | 8 MiB write |
|---|---|---|---|
| `posix` | 6.01 s | 8.06 s | 7.72 s |
| `sftp` | 5.71 s | 8.18 s | 5.41 s |
| `pipe` | **5.35 s** | **5.25 s** | **5.16 s** |
| `agent` | 10.16 s | 5.51 s | 5.77 s |

The uncomfortable row is `sftp`. It exists to beat the shell tier on file
content, and after cutting its reads from four round trips to two it is 5%
ahead on small reads, 30% ahead on writes, and *level* on large reads — while
`pipe`, which is a plain request/response, beats it on all three. See
[Is sftp worth it?](#is-sftp-worth-it).

A host ~540 ms per command, on a lossy tunnel:

| tier | 15 × 1 KiB read | 8 MiB read | 8 MiB write |
|---|---|---|---|
| `posix` | 22.42 s | 33.87 s | 8.65 s |
| `sftp` | 36.27 s | 27.78 s | 15.43 s |
| `pipe` | **20.35 s** | **7.46 s** | **6.36 s** |
| `agent` | 42.72 s | 12.23 s | 13.51 s |

Once latency is real, round trips dominate everything else, and the ordering is
consistent at both: `pipe` beats `sftp` on every axis on both hosts, `agent` is
the slowest to start on both, and `posix` is the worst on large reads on both.
The margins narrow as latency falls, which is what should happen if round trips
are what is being counted — and at zero latency they vanish into the target's
process-spawn cost, as the table above shows.

That is why the negotiation order is decided by the real-link numbers. waldo
exists to drive remote hosts; a benchmark with no network in it is measuring
something else.

The reason is that the tiers differ in *round trips per operation*, not in work
done. `pipe` and `agent` answer a whole file in one request and one response
over a channel that is already open. SFTP needs several — open, fstat, read,
close — and each is a round trip. Its cheap setup dominates when round trips are
free and stops mattering the instant they are not.

waldo exists to drive *remote* hosts, so the second table decides the
negotiation order: `pipe`, then `sftp`, then `posix`.

Two more things these numbers show:

**The agent tier is the slowest to start**, in both tables, which is the
opposite of what "fastest tier" suggests. Its advantage is per-operation, and
waldo runs one process per tool call, so a binary launching is pure overhead
that batching never gets to amortise.

**Small reads are dominated by the process model**, not the tier: ~1.4 s each
against a 258 ms host, most of it a new waldo process and a new channel. No tier
fixes that; only a resident process would, which is the trade discussed in
[ARCHITECTURE.md](ARCHITECTURE.md#there-is-no-daemon).

Getting this wrong once already cost something concrete. SFTP writes were a
sequential loop of 32 KiB chunks — one round trip each — which is invisible on
loopback and took **79 seconds** to write 8 MiB over the real link, eleven times
slower than the shell tier it exists to beat. Pipelining the writes as the reads
already were brought it to 15 s. A benchmark on the wrong link would have found
none of it.

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

## Tier 1 — `sftp`

Uses the SFTP subsystem that ships enabled in stock OpenSSH, reached with
`ssh -s <host> sftp` over the same multiplexed connection as everything else —
no extra authentication, no extra TCP connection. Structured file operations, no
encoding overhead, and reads pipelined eight deep so a high-latency link stays
full instead of paying one round trip per 32 KiB.

waldo implements the client (`internal/sftp`) rather than importing one. Version
3 has been frozen since 2001, the subset waldo needs is a few hundred lines, and
every dependency this project takes on is supply chain that an operator inherits
from a tool whose entire premise is touching nothing.

SFTP is used as a **protocol, not a mount**: waldo asks for bytes and gets bytes
or an error. Nothing is written to the target, and no operation can wedge in
uninterruptible sleep the way a stalled mount does.

Atomic writes use OpenSSH's `posix-rename@openssh.com` extension when the server
advertises it. Plain v3 `rename` refuses an existing destination, so without the
extension an overwrite needs remove-then-rename, which has a window where the
file does not exist at all.

Search and glob are **not** SFTP operations — the protocol has no such request,
and answering them client-side would mean transferring every candidate file.
They run as shell commands on the target, exactly as at tier 0.

Falls back to tier 0 automatically if the subsystem is disabled, which some
hardened hosts do. `waldo doctor` reports whether it answered.

## Tier 2 — `pipe`

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

## Tier 3 — `agent` (auto-installed)

A small static Go binary, installed by waldo when this tier is selected. The
user is never asked to install anything by hand. It speaks the identical
protocol to tier 2 — one client serves both, because a second implementation
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

## Is sftp worth it?

An honest question, and the numbers do not flatter it.

The tier exists because SFTP is the one structured file protocol every stock
`sshd` already speaks, so it promised faster file access with nothing installed.
What the measurements show is that it is a *chatty* protocol on a link where
round trips are the entire cost: every read needs a handle, so the floor is two
round trips even after `open` and `stat` are issued in parallel and the `close`
is not waited for. A request/response tier answers the same read in one.

Where it still wins is writes on a host without `python3` — 5.41 s against
7.72 s — because base64 costs the shell tier a third of its bandwidth. Where it
does not win is anywhere `pipe` or `agent` can run.

So the case for keeping it is narrow: a target with no `python3`, where nothing
may be installed, doing many writes. The case against is that it is several
hundred lines of hand-written protocol that has to be kept correct — and it is
where three of this project's bugs came from, all of them concurrency or
round-trip mistakes that the shell tier's simplicity made impossible.

It is kept for now, no longer preferred by autonegotiation, and available
explicitly with `--fileops=sftp`. If it is removed later, the replacement for
its niche is `--fileops=agent`, which is a request/response tier that needs no
`python3` — at the cost of the one thing it must not do silently, which is
write a binary to the target.

## Conformance

The four tiers share almost no code, and a user cannot tell which is in use. So
all four run one identical suite (`internal/fileops/fileopstest`) covering NUL
bytes, invalid UTF-8, CRLF, empty files, 5 MiB payloads, offset reads, awkward
filenames, atomic overwrite, and not-found reporting.

`test/integration` runs that same suite over a real sshd and adds the property
that matters most: a file written through any tier reads back byte-for-byte
through every other, with matching digests. A tier that cannot pass does not
ship.
