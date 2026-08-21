# Changelog

All notable changes to reach are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

reach depends on interception seams that are undocumented implementation
details in closed harness binaries. Entries therefore name the harness versions
a change was verified against: "works with Claude Code" is not a claim this
project makes without a version attached.

## [Unreleased]

## [0.1.0] - 2026-08-21

The first tagged release, and the first artefacts anyone can download. A
changelog this long under a first version is not an accident: reach started as
tier-0 file operations over SSH with adapters for a handful of harnesses (see
"Where this started" at the end), and everything below was built on that before
any of it was published.

### Added

- **A container image on GitHub Packages**, `ghcr.io/bojieli/agentreach`, for
  amd64 and arm64. It carries an `ssh` client and the helper binary for every
  target platform beside `reach` itself, so the helper tier works in it without
  a Go toolchain. Keys are mounted in, never baked in.

- **Releases are gated on CI.** A tag is a claim that a commit is releasable,
  not evidence of it. The release workflow now calls CI as a reusable workflow
  and publishes nothing — no archives, no image, no signature — until every
  test on every platform, the linters, the fuzz runs, govulncheck, the
  cross-compiles and the release dry run are green on the tagged tree.

### Fixed

- **Two flaky tests, one of which blocked a release.** `TestProcessWriteAndTerminate`
  wrote a line to `cat` and killed it immediately, then asserted on the echo.
  The sentinel filter withholds its tail until the stream ends, so a one-line
  echo reaches no client while the process runs — the assertion was really a
  race between `cat` and `SIGKILL`, and under CI load the kill won. It now
  writes enough to clear the withheld tail, waits for the echo, and only then
  terminates. Separately, `Server.Close` does not wait for the pumps started by
  `process/start`, which write the command's audit record last; a test deleting
  its `REACH_HOME` could race one. Production shutdown still does not wait — a
  process the agent started may outlive the request by hours — but the tests
  now terminate and join before tearing down.


- **Path mapping was broken for a Windows operator, in the direction that
  matters.** Both places reach translates between the operator's filesystem and
  the target's compared paths without agreeing on a separator first. The
  workspace comes from Windows and is spelled with backslashes; the paths it is
  compared against arrive from a harness or a `file://` URI and are spelled with
  forward slashes, so nothing ever matched. In `execserver` that meant every
  path fell through to the "this is already a target path" branch — a file the
  agent meant to read beside itself was read on the target instead. In the PATH
  shim it was worse than a miss: `filepath.Rel` returned `..\..\..\etc` and the
  containment guard only recognised `../`, so `cd /etc && cat hostname` was
  rewritten to `cd /srv/app/../../../etc`, escaping the workspace it was
  supposed to be confined to. Both now compare in one spelling, and the shim
  asks only whether a directory is inside the workspace rather than computing a
  relative path and inspecting it for `..` afterwards.

  These had gone unnoticed because the Windows test job never reached the tests:
  it failed at `gofmt` first, on every push since 2026-08-21.


- **The tier reach negotiates by default wrote temporaries under reach's former
  name.** internal/reach/tempfile.go states the rule every tier depends on —
  write to a same-directory `.reach.tmp.*` and rename over the destination — and
  explains that the prefix is deliberate, because anything an interrupted write
  leaves behind has to be identifiable as reach's. The pipe handler spelled it
  `.waldo.tmp.`. Debris from the default tier was therefore unattributable on a
  machine the operator may not own, and the conformance suite's own "nothing may
  be left behind" assertion, which looks for `.reach.tmp.`, could never have
  failed for that tier. A test now reads both implementations and fails if
  either stops honouring the contract.

- **The exec-server's memory grew for the life of an agent session.** A process
  record is kept after its command exits so codex can still read the output, and
  nothing removed it: a server that ran a hundred commands held a hundred
  records, each with up to a mebibyte of retained output, until the agent quit.
  The last thirty-two are kept. Separately, a process remembered every
  process/write id it had ever seen, for deduplicating retries; the last 256 are
  kept, which covers any realistic retry window.

- **A working directory reach could not record was discarded silently.** Nothing
  carries the directory between tool calls but the session file, so a full disk
  or a bad permission in `~/.reach` meant the agent's `cd` quietly stopped
  persisting and its next command ran somewhere else, with nothing saying why.

- **A timed-out command was reported as though it had stopped.** Closing the
  channel is the whole of reach's control over a command it started: a stock
  sshd offers no way to signal a remote process group, and a command producing
  no output never notices the disconnect, so `sleep 600` survives a timeout and
  so does a quiet build. The timeout now says the command may still be running
  and how to check. A local target does not get the warning, because that
  process is reach's own child and really is killed.

- **A broken file-operation handler ended file access for the whole session.**
  Breaking the stream when a request is abandoned or half-written is what stops
  a stale response from being read as the answer to the next one, but the
  verdict was permanent and nothing acted on it: the error said "this session's
  file access must be restarted" and no code path restarted anything. Per tool
  call that cost nothing, since the process was about to exit. Under `reach
  exec-server`, where one handler serves an entire agent session, one cancelled
  or timed-out operation ended file access for the rest of it. The request that
  discovers the break still fails — whether it reached the far end is unknown,
  and a `rename` retried on a guess is applied twice — but the next one starts
  the program again on a new channel. That is safe because the protocol is
  stateless: every request carries its own path and offset. A program that never
  answered is not restarted, so a target with no interpreter fails once instead
  of spawning a doomed process per operation.

- **A target that refused another channel was reported as a command that did
  not complete.** sshd caps concurrent channels per connection with
  `MaxSessions`, 10 by default, and multiplexing means every tool call running
  at once is a channel on one connection. An agent that fanned out past the cap
  had its eleventh tool call refused, and the refusal arrived as ssh exit 255
  with "administratively prohibited" — naming neither the cause nor anything an
  operator could do. reach now moves to another connection to the same target
  and runs the command there, bounded at four extra connections, and a refusal
  it cannot work around says what `MaxSessions` is and what to change. The retry
  is safe in a way retrying a failed command is not: a refused channel means the
  remote shell was never started. A dropped connection is never retried, because
  it says nothing about whether the command ran.

- **Short-lived commands tore down a connection another session was using.**
  The control socket is keyed on the destination, so two sessions on one host
  share a connection and authenticate once. Four callers closed it anyway:
  `reach down` on one of two sessions on the same host, the exec-server when
  codex exited, and `reach doctor` and `reach helper` whenever they ran. The
  session was still up in every case, so the connection came back — but in batch
  mode, which is every connection after `reach up`, and on a host that wants a
  password or a token a reconnect that cannot prompt fails rather than being
  slow. `reach down` now asks who else is on a connection before ending it; the
  other three no longer close it at all.

- **`reach up` threw away the connection it had just authenticated.** The
  multiplexing probe forced batch mode on, so on exactly the hosts multiplexing
  matters most for — a passphrase, a password, a hardware token — the probe
  could not authenticate, reach recorded "no multiplexing", and every later tool
  call opened its own connection in batch mode and failed too. The probe then
  tore down the master it had established, on the grounds that the caller had
  not asked for one. It now takes batch mode from its caller, stretches its
  timeout to three minutes when it may prompt, and keeps a connection it proved
  working. When one does expire, ssh's "Permission denied" is followed by what
  happened and what to do about it.

- **The exec-server answered pipelined requests about one path out of order.**
  Requests are dispatched concurrently, and must be — process/read long-polls
  while others are answered — but concurrent was also unordered. Twenty writes
  to one path followed by a read, sent without waiting, left the file holding
  whichever write finished last and the read reporting that same stale content.
  Two chunks pipelined to one process's stdin could arrive swapped, corrupting
  the input of anything interactive. Requests about one path, handle or process
  now queue in the order they arrived on the wire; requests about different ones
  still overlap.

- **Nine Windows tests failed the first time the Windows suite ever ran.**
  Repairing the line-ending failure below let that job reach its tests at last,
  and none of the nine turned out to be a defect in reach — but each was a test
  that could not have passed on Windows and had never been asked to. Four build
  a `local://` target out of `t.TempDir()`, which on Windows is `C:\...` and not
  a URL; a `local://` target is refused there by design, so they now skip and
  say why. Four failed only in cleanup, with every assertion already passed:
  `t.TempDir` fails a test whose directory it cannot fully remove, and the shim
  is a hard link to the running test binary, which Windows locks. One asserted a
  0600 file mode on a platform with no POSIX mode bits, for a binary that is
  only ever copied onto a POSIX target. The last reported that the mirror had
  "escaped the mirror root" — alarming in the one place it should be, and
  untrue: it compared a `\mirror\root\...` path against a hardcoded
  forward-slash prefix, while the round-trip assertion beside it passed and the
  real containment guard uses `filepath.Rel`.

- **A target that could not run the handler said so only about two thirds of
  the time.** The pipe and helper tiers both diagnose "this machine has no
  python3" from the first request, which doubles as the handshake — but only the
  *read* half of that request carried the diagnosis. Which half fails is a race:
  if the program is already gone the write gets EPIPE, and if it dies a moment
  later the write lands in the pipe buffer and the read gets EOF. So the same
  broken target reported either `python handler did not start: ... (/bin/sh:
  exec: /nonexistent: not found)` or the bare `write request: write |1: broken
  pipe`, which names neither the program nor the reason. The program's own
  complaint arrives on a third pipe, and the error now waits for it rather than
  racing it, so the explanation is part of the message instead of a coin toss.
  This was failing CI as a flake roughly one run in fifteen.

- **A request that could not be written left the stream in service.** Every
  other failure in the handler protocol marks the stream unusable, because a
  frame that was half accepted leaves the far end waiting for the rest of it —
  and the next request would then be read as this one's tail and answered
  against the wrong header, which for a file read means returning one file's
  bytes as another's. The write path was the one path that did not, so a failed
  write was followed by a request that could be answered with confident
  nonsense. It now takes the stream out of service like everything else.

- **Three CI jobs had never verified anything.** `lint` failed while installing,
  because golangci-lint-action v6 rejects a v2 version string outright — so no
  linter had ever run against this repository (it is clean, now that it does).
  `fuzz parsers` pointed at `./cmd/reach-agent/`, which the tier rename had
  moved, so the frame parser — one of the two parsers that reads input reach
  does not control — was never fuzzed. And the Windows CLI check wrote `set -uo
  pipefail` to turn off `-e`, which does not turn off `-e`; the runner had
  already enabled it, so the expected non-zero exit ended the step before a
  single assertion ran.

- **The Windows test job failed on line endings.** Git for Windows checks out
  CRLF by default, and `gofmt -l .` calls a CRLF file unformatted, so the job
  died listing every Go file in the repository before running a test. A
  `.gitattributes` now pins LF everywhere — which also stops a Windows build
  from embedding a CRLF `handler.py` and sending it to somebody else's POSIX
  machine.

- **The release pipeline could not have produced a release.** The build hooks
  wrote the helper binaries into `dist/`, which goreleaser then refuses to find
  non-empty, so a tag would have failed in the one step nobody runs locally. Two
  more defects were behind it: `docs/**/*` matched only nested files, so no
  archive carried ARCHITECTURE, TRANSPORTS, SECURITY or WINDOWS; and goreleaser
  ships an archive whose file globs matched nothing without an error, so a
  release could have gone out with no helper binary at all — the thing the
  helper tier installs on your target. CI now builds a snapshot on every push
  and asserts the archives contain what they promise.

- **`reach fs` blamed a flag when the subcommand was wrong.** `reach fs search
  --root /srv` reported "flag provided but not defined: -root" rather than
  saying the subcommand is `grep`. Flags are now parsed after the subcommand is
  checked, and a plausible wrong guess names the right command.

- **The session name was spelled differently by different commands.** `reach env
  --session prod` failed with "flag provided but not defined" while `reach log
  --session prod` worked. Every command that acts on a session now accepts both
  `--session NAME` and a positional name.

- **A session naming a removed tier loaded as a *pinned* posix session.**
  `Load` discarded `ParseTier`'s error, leaving the tier at its zero value, so a
  session created with `--fileops=sftp` came back reporting the tier it was told
  while running a different one — reach doing the thing it exists to prevent.
  Such a session is now refused, with the explanation of what happened to the
  tier and the command to recreate it.

- **`reach down` could not remove a session it could not load.** It loaded the
  session first and returned the error, so it refused exactly the sessions most
  in need of removing — and since those failures suggest `reach down` as the way
  out, the advice was a loop whose only exit was deleting a file from `~/.reach`
  by hand. It now removes the local state either way, and says that nothing was
  cleaned up on the target because the session could not be read. A session that
  does not exist is still an error.

- **`reach status` listed only the sessions it could load.** A session file that
  will not load is still configured in somebody's harness; dropping it printed
  "no reach sessions" to an operator whose agent was pointed at one. Unloadable
  sessions are now listed with the reason. Files in the directory that are not
  sessions at all remain silent.

- **`reach status` accepted and ignored arguments**, so `reach status --name
  prod` printed every session while looking like it printed one.

- **Codex 0.148 and Kimi Code 0.37 bypassed the shell shim, and nothing
  noticed.** Both harnesses stopped resolving their shell by name: Codex
  0.148.0 reads the login shell from the account database (`getpwuid_r`) and
  spawns it by absolute path (`/bin/zsh -lc …` on stock macOS), and Kimi Code
  0.37.2 does likewise. No `PATH` entry can intercept an absolute path, so
  every command the agent ran executed on the operator's own machine while the
  agent believed — and reported — that it acted on the target: the failure
  reach exists to prevent, failing silently. The conformance suite missed it
  because its Codex check probed `codex sandbox`, which resolves the
  *user-supplied* program via execvp and kept passing while the shell tool's
  own resolution changed underneath it. reach now measures the seam
  behaviourally (see `reach harness verify` below), caches the verdict per
  harness version, and **refuses to launch a harness version measured to
  bypass the shim**; `--force` overrides, with a warning, for operators who
  accept local execution. Codex ≤ 0.147 resolves its shell by name and is
  unaffected. There is no config key, environment variable, or hook in either
  harness that restores name resolution; the Codex macOS binary's hardened
  runtime also rules out `DYLD_INSERT_LIBRARIES` interposition, so refusal
  plus detection is the honest floor until upstream offers a seam.

- **The PATH shim now answers to `zsh` as well as `bash` and `sh`.** zsh is
  the default login shell on macOS, and harnesses that resolve the login
  shell by name (rather than hard-coding `bash`) otherwise slipped past the
  shim on a stock macOS install.

### Added

- **`REACH_CONTROL_PERSIST`** sets how long an authenticated connection outlives
  its last command — a duration, or `yes` to keep it until `reach down`, which
  is what reach's up/down lifecycle already describes. A value reach cannot
  parse is refused at construction rather than replaced with the default, so an
  operator who asks for five minutes never silently gets an hour.

- **`reach fs mv <from> <to>`.** Every tier implemented Rename and the
  conformance suite covered it; the CLI just never exposed it, so the one file
  operation an agent could not express through `reach fs` was the most ordinary
  one there is.

- **`reach harness verify codex|kimi`** measures a harness's shell seam instead
  of assuming it. The command points the installed harness at a mock model
  server embedded in reach — the Responses API for Codex, chat completions for
  Kimi, both offline, so no API key and no tokens — scripts one shell tool call
  that echoes a marker and the hostname, and checks whether the command ran on
  the session's target or on the local machine. The verdict is cached per
  harness version and consulted by the launch guard, and the whole probe runs
  in `make conformance` via `test/e2e/seam_test.sh`, so a harness upgrade that
  breaks the seam turns the suite red in seconds rather than surfacing as an
  agent quietly operating on the wrong machine. The mock-model server used by
  the harness tests learned the Responses dialect for the same reason.

- **`reach doctor`** reports the cached seam verdict next to each harness, so
  "this Codex bypasses reach" is visible before a session, not during one.

- **`reach status NAME`** shows one session, which the help had always said it
  did. It reads local state only and never contacts the target, so it still
  answers when the target is unreachable — which is when you most want to know
  what reach thinks it is connected to. `reach status` with no name still lists
  everything, including sessions that will not load, with the reason.

- **Releases are signed and ship an SBOM.** Keyless cosign signing over
  `checksums.txt` binds a release to this repository's tagged CI run, and each
  archive carries an SPDX SBOM. reach's helper tier copies a binary onto a
  machine you may not own, and the release archive is where that binary comes
  from, so provenance is not decoration here.

- **Session documents carry a schema version.** A session file outlives the
  binary that wrote it, and more than one reach is often on PATH — a
  package-managed install and a `go install` build are the usual pair.
  `encoding/json` drops unknown fields without a word, so an older binary
  reading a newer document did not fail, it succeeded with a session it had only
  partly understood. Documents from a newer schema are now refused rather than
  half-read; documents written before the field existed still load.

### Changed

- **Mirrored files are verified by digest instead of being transferred.** The
  FileOps interface says Hash is "used by the mirror engine to decide what
  actually changed", and the mirror never called it — so every edit in mirror
  mode moved the file across the network three times, and an agent that read the
  same file twice in a turn pulled the whole thing across each time to produce
  bytes the mirror already held. Push now asks the target for a digest, and
  Fetch skips the transfer when the mirror's copy and the target's both still
  match the digest recorded at fetch time. A target that cannot hash falls back
  to the read that was there before, and the guarantee that a write cannot
  overwrite a file that changed on the target is unchanged.

- **A connection is now kept for an hour when idle, up from ten minutes.** Ten
  minutes is shorter than the gaps an agent session actually has: a model
  thinking, a colleague at the door, a long test run watched from another
  window. Expiring in one costs a full reconnect, and because every connection
  after `reach up` runs in batch mode, on a host wanting a password or a token
  it costs the tool call outright.

- **Overlapping file operations are answered on more than one handler.** The
  handler protocol is serialised on purpose, which costs nothing where reach
  runs a process per tool call. Under `reach exec-server` it was head-of-line
  blocking: a 100 MB read held the stream across a dozen sequential chunk round
  trips while every other file operation waited. Pipelining would not have
  fixed it — the program on the far end reads one frame at a time — so up to
  four handlers are now used, started only when operations actually overlap.

- **Go 1.25.8 is now the minimum**, up from 1.23. govulncheck had been failing
  against three reachable standard-library vulnerabilities, and two of them are
  in `net/url` — reached from `session.ParseTarget`, which is the function that
  decides which host reach connects to. Bumping only CI would have left the
  documented install path (`make install` from a clone) building a binary with
  the vulnerable parser in it, so the floor in `go.mod` moved with it.

- **The `agent` tier is now called `helper`**, and `reach agent` is now
  `reach helper`. "agent" already meant the coding agent — the thing reach
  exists to serve — so `reach agent uninstall` read like it removed Claude Code.
  The binary it installs is `reach-helper`, cached as
  `~/.cache/reach/helper-<version>-<os>-<arch>` on the target. Both old spellings
  now explain the rename rather than reporting an unknown tier or command.

### Removed

- **The SFTP tier.** It was implemented, tested, measured, and then deleted.
  SFTP cannot answer a tool call in one network round trip — `READ` takes a
  handle that only exists in `OPEN`'s response, so its floor was two, while
  every other tier does it in one. Its remaining advantage was bandwidth, and
  that turned out to be reach's own fault: tier 0 base64-encoded content
  unconditionally. Once reach began proving whether a link is 8-bit clean and
  skipping the encoding when it is, the shell tier read 8 MiB in 6.4 s against
  SFTP's 8.1 s and matched it elsewhere. `--fileops=sftp` now explains the
  removal rather than reporting an unknown tier. Full reasoning in
  `docs/TRANSPORTS.md`.

### Added

- **Content moves unencoded on links proven 8-bit clean.** reach pipes every
  byte value through the target's own digest command, and has the target print
  them back; base64 is used only where something garbles a byte. That is a third
  of the bandwidth on every file, in both directions.
- **An audit log.** reach records every command it runs on a target and every
  file it changes there, readable with `reach log`. The situation reach is built
  for ends with somebody asking what the agent did on a machine you do not own,
  and "I don't know" is not an answer about a production host. Local only,
  outlives `reach down` deliberately, and `REACH_NO_AUDIT=1` turns it off.
- **Fuzz targets** for the three parsers that read input reach does not control:
  the SFTP wire format, the agent's framing, and the harness command envelope.
- **Native Windows support.** reach runs on Windows as a first-class operator
  platform, driving a remote POSIX target. Shims are installed as hard links
  (falling back to copies) rather than symlinks, which need Developer Mode;
  harnesses are launched as child processes, since Windows has no `execve`;
  executables are found through `PATHEXT` rather than a Unix execute bit; and
  the search path is matched case-insensitively, because Windows spells it
  `Path`. Unit tests and a CLI smoke test run on `windows-latest` on every
  commit.
- **Connection multiplexing is probed rather than assumed.** Win32-OpenSSH does
  not implement `ControlMaster`, so reach establishes a master and asks the
  client to confirm it, records the answer in the session, and reports it in
  `reach up` and `reach doctor`. A Windows OpenSSH that gains the feature will
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
- `reach helper status` and `reach helper uninstall`, so the one tier that writes
  to a target can be inspected and reversed.
- Tier autonegotiation, chosen by measurement rather than by tier number, and
  proven by building the tier during `reach up` instead of assuming it.
- A shared conformance suite every tier must pass (`internal/fileops/fileopstest`),
  plus an integration test that a file written through any tier reads back
  byte-for-byte through every other.
- `make bench`, `make integration`, `make lint`, `make build-agent`.
- Release archives now carry the tier-3 helper for every target platform.

### Changed

- **A pinned `--fileops` tier is never substituted.** It was accepted, reported
  as selected, and then silently downgraded to tier 0. An autonegotiated tier
  may still degrade, but says so and records the reason.
- `reach doctor` reports which tiers a host qualifies for and why, and lists
  anything reach has installed there.
- The integration suite runs against a user-owned `sshd` instead of requiring
  Docker, so it needs no container runtime and no network, and covers GNU and
  BSD userlands rather than Linux only.

### Fixed

- **Mirror-mode digests were lost under concurrent tool calls.** They lived in
  one shared JSON document that every hook rewrote whole, so parallel fetches
  clobbered each other — one entry survived out of twenty, measured. A lost
  digest is not a lost optimisation: `Push` treats "no record" as "nothing to
  verify against" and writes anyway, so the guarantee that a write cannot
  overwrite a file that changed on the target silently stopped holding, in
  exactly the concurrent case where two tools are most likely to touch one tree.
  Records are now one file per path.
- **`reach up` accepted a workspace that does not exist**, then failed every
  subsequent command with a `cd` error from the target — which reads as reach
  being broken, once per tool call, rather than as a wrong path, once, in front
  of the operator who typed it.
- **`reach down` left the tier-3 helper on the target without saying so**, which
  made "reach leaves no trace" false by omission. It now reports the footprint,
  and `reach down --clean` removes it.
- **Windows silently did the wrong thing in three places**, each of which ended
  with the agent's commands running on the operator's own machine while it
  believed it was working on the target: the search path was matched as `PATH`
  when Windows spells it `Path`, so the shim directory was never actually put in
  front of the harness; executables were detected by a Unix execute bit that
  Windows never sets, so every harness looked uninstalled; and two of the three
  harness launch sites had no fallback for a failed `execve`, which on Windows
  is every one of them.
- `reach fs mkdir` failed against every BSD-userland target (macOS included).
  BSD `chmod` takes the mode as a positional argument, so option parsing stops
  there and the `--` that followed was read as a filename.
- Tier 2 and 3 operations ignored their context, so an unresponsive target could
  block a tool call indefinitely. Every tier is now bounded by the session
  timeout.
- SFTP sizes reported by the server were converted without a bound; a server
  claiming 2^63 bytes produced a negative length and nonsense reads.
- `reach version` reported the compiled-in constant regardless of the release it
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
  fastest `helper` tier is the slowest to start.

### Where this started

The initial development state, never tagged: tier-0 file operations, the SSH,
container and local transports, session state, `exec` and `mirror` modes, and
adapters for Claude Code, Codex, Kimi Code and opencode.

Verified against Claude Code 2.1.233 and Codex CLI 0.147.0. See
`docs/RESEARCH.md` for what was checked and how.
