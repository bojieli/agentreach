# Contributing to AgentReach

## Before you start

AgentReach hooks undocumented implementation details in closed binaries. That shapes everything about how this project works and what it asks of contributors.

The most important rule:

> **Claims about an agent harness must be backed by an experiment, not by its docs.**

Agent binaries are not open contracts. A seam that silently changes shape is the single most likely way this project breaks for users. If you add or change a harness adapter, you must:

1. Run a real experiment that proves the seam does what you say.
2. Record what you observed in `docs/RESEARCH.md`, with the exact harness version.
3. Add a conformance test that will fail when the harness changes the shape you depend on.

If you cannot verify something, say so in the docs rather than implying it works. [`docs/harnesses/opencode.md`](docs/harnesses/opencode.md) is the model for how to document a partially verified adapter.

---

## Running the tests

```console
make check        # vet + unit tests — no network, no API key, no tokens
make integration  # every file-operation tier against a real sshd
make bench        # what each tier costs; source for the table in docs/TRANSPORTS.md
make mock         # verifies the mock model server — no API key needed
make conformance  # checks that harness seams still have the expected shape
make e2e          # real agents against a real target — spends tokens
```

`make integration` starts an `sshd` owned by your user on a high port. It needs neither root, nor Docker, nor an external network. Mocks are deliberately not used there: shell-quoting bugs, exit statuses swallowed by SSH's use of 255, and whether a link is 8-bit clean are exactly the problems a mock cannot catch.

Set `REACH_TEST_SSH_HOST=my-box` to run against a real remote host. Set `REACH_TEST_SSHD=docker` to run against a Debian container, which is how a GNU target gets exercised from a BSD host.

Tests that spend model tokens are gated behind `make e2e` and are never part of `make check`, so you can develop and verify the vast majority of the project without an API key.

---

## Adding or changing a file-operation tier

Every tier runs one shared conformance suite, [`internal/fileops/fileopstest`](internal/fileops/fileopstest). Add cases there rather than to a single tier's tests:

- The tiers share almost no code.
- A user cannot tell which tier is in use.
- The entire design rests on their producing identical results.

A case that only one tier passes belongs in the shared suite until every tier passes it.

If you change which tier reach negotiates by default, re-run `make bench` and update the table in [`docs/TRANSPORTS.md`](docs/TRANSPORTS.md). The intuitive ordering is wrong here — see that document for why — so this is a decision to measure, not to reason about.

---

## Adding a harness adapter

1. Read the existing adapters (start with Claude Code and Codex — they represent two opposite ends of the seam-quality spectrum).
2. Find the seam. Harnesses provide shells through `env` vars, config files, exec-server protocols, or PATH shims. Prefer a first-class seam over an intercept.
3. Write a probe: `reach harness verify <name>` should drive a real harness turn against the offline mock model and confirm where the command ran.
4. Write a launch guard: if the seam can regress with a version bump (it usually can), refuse to launch a version with a bypassed seam rather than running the agent against the wrong machine.
5. Document what you observed in `docs/harnesses/<name>.md` and `docs/RESEARCH.md`, with version numbers.

---

## Design rules

These are load-bearing. A change that violates one needs a very good reason and an explicit discussion.

**Every failure must be a value the agent can reason about, never a process that stops responding.** This is why reach is request/response over SSH rather than a filesystem mount, and why timeouts and caps exist on every code path.

**Silent wrong-target access is the worst possible failure.** An agent that reads a local file believing it is remote produces confident nonsense. One that writes a local file believing it is remote destroys the operator's work. Where reach cannot redirect a tool, it denies the tool. Where a session is engaged but broken, reach fails loudly rather than falling back to local execution.

**Nothing is installed on the target by default.** Tier 0 needs only a POSIX shell and writes nothing. Autonegotiation never selects the helper tier; that stays an explicit operator decision.

**Every target is untrusted, and there is no flag that says otherwise.** No credential is ever sent to a target. SSH agent forwarding is off, unconditionally. Output from a target is untrusted input flowing into the context of an agent that holds the operator's credentials.

---

## Code style

- Go 1.25.8+, `gofmt`, `go vet` clean.
- Comments explain *why*, especially where the code looks odd. Most of the strange-looking code in reach is strange because a harness or shell made it necessary, and the next reader needs to know which.
- Errors tell the operator what to do next, not just what went wrong.
- Platform differences between Linux/macOS and Windows live exclusively in `cmd/reach/platform_other.go` and `cmd/reach/platform_windows.go`. If you find yourself adding a `runtime.GOOS` check anywhere else, add a function to those two files instead. Three of the Windows differences fail *silently* in the direction where an agent runs commands on the operator's own machine, and they are only findable when they are all in one place with the reasoning attached.

---

## Reporting security issues

Use [GitHub Security Advisories](https://github.com/bojieli/agentreach/security/advisories) rather than opening a public issue. See [`docs/SECURITY.md`](docs/SECURITY.md) for the full security model.
