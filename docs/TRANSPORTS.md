# Transport tiers

waldo separates *how it reaches a target* (transport) from *how it performs file
operations* (file-op strategy). File-op strategies are tiered by what the target
can support, and waldo negotiates the best available tier automatically. Every
tier is selectable explicitly with `--fileops=<tier>`; `waldo doctor` reports
which tiers a given host qualifies for and why.

The guarantee: **tier 0 works on any host running stock `sshd` and requires
nothing installed, nothing written to disk, and no subsystem beyond a login
shell.** Higher tiers are optimisations, never requirements.

| tier | name | remote requirement | writes to remote disk | relative speed |
|---|---|---|---|---|
| 0 | `posix` | a POSIX shell | none | baseline |
| 1 | `sftp` | SFTP subsystem (OpenSSH default) | none | ~3-10x on file ops |
| 2 | `pipe` | `python3` present | none | ~10-30x on batches |
| 3 | `agent` | ability to execute an uploaded binary | one cached binary | fastest |

## Tier 0 — `posix` (strict SSH, universal)

For targets where **nothing may be installed** and only shell access exists.
Every operation is an ordinary command over the SSH connection:

| op | implementation |
|---|---|
| read | `dd if=<path> bs=1 skip=<off> count=<n>` piped through `base64` |
| write | `base64 -d > <tmp> && mv <tmp> <path>` (atomic rename) |
| stat | `stat -c` / BSD `stat -f` fallback, else `ls -ld` parsing |
| list | `ls -A` with a NUL-safe format |
| search | `rg --json` if present, else `grep -rn` |
| glob | `find` with `-path`, else shell globbing |
| mkdir/rename/remove | `mkdir -p` / `mv` / `rm` |

Binary-safe because content is base64-framed in both directions. Reads and
writes are offset-addressed, so a dropped connection resumes rather than
restarts. Encoding costs ~33% bandwidth, which is the price of universality.

This tier is the default when waldo cannot prove a higher tier is available.

## Tier 1 — `sftp`

Uses the SFTP subsystem that ships enabled in stock OpenSSH. Structured file
operations with no encoding overhead and pipelined requests. Still **zero
footprint**: SFTP is used as a protocol, not mounted as a filesystem.

Falls back to tier 0 automatically if the subsystem is disabled — which some
hardened hosts do — rather than failing.

## Tier 2 — `pipe`

A single stdlib-only handler script is piped to the remote over **stdin**
(`python3 -`) and speaks length-framed JSON-RPC over that one long-lived
channel. It batches operations and keeps no state on disk:

    ssh host 'exec python3 -' < handler.py

**Nothing is ever written to the remote filesystem** — the handler exists only
in the memory of a process that dies with the session. This tier exists for
hosts where you want batched performance but must leave no trace.

## Tier 3 — `agent` (auto-installed)

A small static Go binary, **installed automatically by waldo** when this tier is
selected. The user is never asked to install anything by hand.

Installation sequence, performed by waldo on first connect:

1. probe `uname -sm` to resolve the target's OS/architecture
2. look for a cached, version-matched, hash-matched binary at
   `~/.cache/waldo/agent-<version>-<os>-<arch>`
3. if absent, stream the matching binary over the existing connection and
   `chmod 0700` it
4. verify by running `waldo-agent --selftest`, which prints its version and
   build hash; a mismatch triggers reinstall rather than silent reuse

Properties:

- **Self-updating.** The version is part of the path, so an upgraded waldo
  installs a new agent rather than reusing a stale one.
- **Removable.** `waldo agent uninstall <target>` deletes the cache directory.
  `waldo doctor` reports exactly what waldo has placed on each host.
- **Never required.** If upload fails, the executable bit cannot be set, or the
  cache directory is on a `noexec` mount, waldo logs the reason and falls back
  to tier 2, then tier 1, then tier 0. Degradation is automatic and visible.
- **Opt-in.** Because this is the only tier that writes to the target, it is
  never selected by autonegotiation on a host marked `untrusted: true`.

The agent speaks the same JSON-RPC protocol as tier 2, plus streaming reads,
recursive hashing for the mirror engine, and server-side diff application.

## Negotiation

    waldo up ssh://host/srv/app                  # autonegotiate, highest safe tier
    waldo up ssh://host/srv/app --fileops=posix  # pin tier 0 — install nothing, touch nothing
    waldo up ssh://host/srv/app --fileops=agent  # opt in to the auto-installed helper

Autonegotiation never selects tier 3 on its own; it stops at tier 2. Writing to
someone else's machine is a decision the operator makes explicitly.
