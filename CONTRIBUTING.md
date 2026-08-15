# Contributing to waldo

## The standard of evidence

waldo hooks undocumented implementation details in closed binaries. That makes
one rule more important than any style guideline:

> **Claims about a harness must be backed by an experiment, not by its docs.**

If you add or change a harness adapter, add a conformance test that fails when
the seam changes shape, and record what you observed in `docs/RESEARCH.md` with
the version you observed it on. If you cannot verify something, say so in the
docs rather than implying it works — `docs/harnesses/opencode.md` is the model
for how to write down a partially verified adapter.

## Running the tests

```console
make check        # vet + unit tests. No network, no API key, no tokens.
make mock         # verifies the mock model server. No API key.
make conformance  # checks harness seams still have the expected shape.
make e2e          # real agents against a real sshd container. SPENDS TOKENS.
```

`go test -tags integration ./test/integration/...` runs the SSH suite against a
real `sshd` in Docker. Mocks are deliberately not used there: shell quoting that
works locally but not through ssh's own re-parsing, and exit statuses lost to
ssh's use of 255, are exactly the bugs a mock cannot catch.

Tests that spend model tokens are never part of `make check`, so a contributor
without an API key can still develop and verify most of the project.

## Design rules

These are load-bearing. If a change violates one, it needs a very good reason.

**Every failure must be a value the agent can reason about, never a process that
stops responding.** This is why waldo is request/response over SSH rather than a
filesystem mount, and why timeouts and caps exist on every path.

**Silent wrong-target access is the worst possible failure.** An agent that
reads a local file believing it is remote produces confident nonsense; one that
writes a local file believing it is remote destroys the operator's work. Where
waldo cannot redirect a tool, it denies the tool. Where a session is engaged but
broken, waldo fails loudly rather than falling back to local execution.

**Nothing is installed on the target by default.** Tier 0 needs only a POSIX
shell and writes nothing to the target's disk. Autonegotiation never selects the
tier that installs a binary; that stays an explicit operator decision.

**Untrusted targets stay untrusted.** No credential is ever sent to a target, and
SSH agent forwarding is off unconditionally. Output from a target is untrusted
input — it flows into the context of an agent holding the operator's
credentials.

## Code

- Go 1.23+, `gofmt`, `go vet` clean.
- Comments explain *why*, especially where the code looks odd. Most of the
  strange-looking code in waldo is strange because a harness or a shell made it
  necessary, and the next reader needs to know which.
- Errors should tell the operator what to do next, not just what went wrong.

## Reporting security issues

Use GitHub Security Advisories on this repository rather than a public issue.
See [docs/SECURITY.md](docs/SECURITY.md).
