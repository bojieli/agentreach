# Changelog

All notable changes to waldo are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

waldo depends on interception seams that are undocumented implementation
details in closed harness binaries. Entries therefore name the harness versions
a change was verified against: "works with Claude Code" is not a claim this
project makes without a version attached.

## [Unreleased]

### Added

- **Native Windows support.** waldo runs on Windows as a first-class operator
  platform, driving a remote POSIX target. Shims are installed as hard links
  (falling back to copies) rather than symlinks, which need Developer Mode;
  harnesses are launched as child processes, since Windows has no `execve`;
  executables are found through `PATHEXT` rather than a Unix execute bit; and
  the search path is matched case-insensitively, because Windows spells it
  `Path`. Unit tests and a CLI smoke test run on `windows-latest` on every
  commit.
- **Connection multiplexing is probed rather than assumed.** Win32-OpenSSH does
  not implement `ControlMaster`, so waldo establishes a master and asks the
  client to confirm it, records the answer in the session, and reports it in
  `waldo up` and `waldo doctor`. A Windows OpenSSH that gains the feature will
  be used without a code change.
- **File-operation tiers 1, 2 and 3.** Only tier 0 existed; the other three were
  described in `docs/TRANSPORTS.md` and silently served by tier 0.
  - `sftp`: a dependency-free SFTP v3 client over `ssh -s sftp`. Zero remote
    footprint, pipelined reads, atomic writes via `posix-rename@openssh.com`
    where the server offers it.
  - `pipe`: a stdlib-only Python handler that is never written to the target's
    disk.
  - `agent`: an opt-in helper binary, digest-verified after upload, refused on
    `--untrusted` sessions, never selected automatically.
- `waldo agent status` and `waldo agent uninstall`, so the one tier that writes
  to a target can be inspected and reversed.
- Tier autonegotiation, chosen by measurement rather than by tier number, and
  proven by building the tier during `waldo up` instead of assuming it.
- A shared conformance suite every tier must pass (`internal/fileops/fileopstest`),
  plus an integration test that a file written through any tier reads back
  byte-for-byte through every other.
- `make bench`, `make integration`, `make lint`, `make build-agent`.
- Release archives now carry the tier-3 helper for every target platform.

### Changed

- **A pinned `--fileops` tier is never substituted.** It was accepted, reported
  as selected, and then silently downgraded to tier 0. An autonegotiated tier
  may still degrade, but says so and records the reason.
- `waldo doctor` reports which tiers a host qualifies for and why, and lists
  anything waldo has installed there.
- The integration suite runs against a user-owned `sshd` instead of requiring
  Docker, so it needs no container runtime and no network, and covers GNU and
  BSD userlands rather than Linux only.

### Fixed

- **Windows silently did the wrong thing in three places**, each of which ended
  with the agent's commands running on the operator's own machine while it
  believed it was working on the target: the search path was matched as `PATH`
  when Windows spells it `Path`, so the shim directory was never actually put in
  front of the harness; executables were detected by a Unix execute bit that
  Windows never sets, so every harness looked uninstalled; and two of the three
  harness launch sites had no fallback for a failed `execve`, which on Windows
  is every one of them.
- `waldo fs mkdir` failed against every BSD-userland target (macOS included).
  BSD `chmod` takes the mode as a positional argument, so option parsing stops
  there and the `--` that followed was read as a filename.
- Tier 2 and 3 operations ignored their context, so an unresponsive target could
  block a tool call indefinitely. Every tier is now bounded by the session
  timeout.
- SFTP sizes reported by the server were converted without a bound; a server
  claiming 2^63 bytes produced a negative length and nonsense reads.
- `waldo version` reported the compiled-in constant regardless of the release it
  was built from: the linker flags targeted a variable that did not exist.
- A mirror-mode path check treated a file legitimately named `..something` as
  being outside the workspace.
- Ripgrep's `-m` caps matches per file, not in total, so a large search could
  overrun the transport's output cap and arrive truncated mid-JSON.

### Documentation

- `docs/ARCHITECTURE.md` described a daemon, a native Go SSH/SFTP stack and a
  `Backend` interface, none of which exist and the first of which the README
  argues against. It now describes the system that exists.
- `docs/TRANSPORTS.md` carries measured numbers instead of estimates. They
  overturn the ordering it asserted: `sftp` is fastest, and the nominally
  fastest `agent` tier is the slowest to start.

## [0.1.0]

Initial development release: tier-0 file operations, the SSH, container and
local transports, session state, `exec` and `mirror` modes, and adapters for
Claude Code, Codex, Kimi Code and opencode.

Verified against Claude Code 2.1.233 and Codex CLI 0.147.0. See
`docs/RESEARCH.md` for what was checked and how.
