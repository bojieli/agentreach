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

Measured against a real OpenSSH server, driving waldo the way a harness does —
one process per operation, over a warm `ControlMaster` connection. Reproduce
with `make bench`.

| tier | 40 × 1 KiB read | 20 MiB read | 20 MiB write |
|---|---|---|---|
| `posix` | 3.55 s | 1.85 s | 0.65 s |
| `sftp` | **1.89 s** | **0.26 s** | 0.29 s |
| `pipe` | 3.29 s | 0.35 s | **0.26 s** |
| `agent` | 5.17 s | 0.26 s | 0.39 s |

One run on one machine. Absolute times move by ±30% with load, so treat the
ratios rather than the seconds as the result — the *ordering* was identical in
every run: `sftp` fastest on small reads, `agent` slowest, and every tier above
0 between five and seven times faster on a large read.

Two of those results contradict what the tier numbering suggests, and both are
worth stating plainly:

**`sftp` wins on both axes.** A subsystem channel costs less to set up than a
shell pipeline, and content moves without base64's 33% expansion. It is the tier
waldo negotiates whenever the subsystem answers.

**`agent` is the slowest to start, not the fastest.** Its advantage — batching
many operations over one long-lived process — cannot be realised in a design
where every tool call is a new waldo process, so what remains is the cost of
launching a binary. Tiers 2 and 3 pay for themselves only on large payloads.

Latency moves this further in tier 0's disfavour, not its favour: these numbers
are over loopback, where a round trip is free. On a real link, tier 0's
sequential per-chunk round trips and 33% larger payloads both get worse, while
`sftp`'s pipelined reads keep the connection full.

## Tier 0 — `posix` (strict SSH, universal)

For targets where **nothing may be installed** and only shell access exists.
Every operation is an ordinary command over the SSH connection:

| op | implementation |
|---|---|
| read | `tail -c +N \| head -c M` piped through `base64` |
| write | `base64 -d > <tmp> && mv <tmp> <path>` (atomic rename) |
| stat | `stat -c` / BSD `stat -f` |
| list | GNU `find -printf` with NUL records, else `find -exec stat` |
| search | `rg --json` if present, else `grep -rn` |
| glob | `find` with `-path` or `-name` |
| mkdir/rename/remove | `mkdir -p` / `mv` / `rm` |

Binary-safe because content is base64-framed in both directions. Reads and
writes are offset-addressed, so a dropped connection resumes rather than
restarts. Encoding costs ~33% bandwidth, which is the price of universality.

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

Negotiation order is `sftp`, then `pipe`, then `posix` — measured order rather
than numeric order, for the reasons in
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

## Conformance

The four tiers share almost no code, and a user cannot tell which is in use. So
all four run one identical suite (`internal/fileops/fileopstest`) covering NUL
bytes, invalid UTF-8, CRLF, empty files, 5 MiB payloads, offset reads, awkward
filenames, atomic overwrite, and not-found reporting.

`test/integration` runs that same suite over a real sshd and adds the property
that matters most: a file written through any tier reads back byte-for-byte
through every other, with matching digests. A tier that cannot pass does not
ship.
