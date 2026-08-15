# Security Policy

## Reporting a vulnerability

Please report privately through **GitHub Security Advisories** on this
repository ("Report a vulnerability" under the Security tab) rather than by
opening a public issue.

Include:

- the waldo version (`waldo version`)
- the harness and its version, if one is involved
- the target's `uname -sm`
- what you expected waldo to do, and what it did instead

You can expect an acknowledgement within a few days. If a fix is warranted, the
advisory will credit you unless you would rather it did not.

## What is in scope

waldo's premise is that **the target is not trusted**. Anything that breaks one
of these properties is a vulnerability, not a bug:

- **No credential reaches the target.** The agent, its API key or OAuth token,
  and its conversation stay on the operator's machine.
- **Nothing is installed or written by default.** Only the opt-in `helper` tier
  writes to a target, and never on a session marked `--untrusted`.
- **waldo acts only on the paths it was asked to act on.** In particular, a path
  originating in content read from a target must not be able to escape the
  mirror root or reach an arbitrary local file.
- **SSH agent forwarding is never enabled.**
- **A failure is a value, never a hang.** A target that stops responding must
  produce an error the agent can reason about.
- **The tiers agree.** A read through one tier returning different bytes than
  another is a correctness bug of the most serious kind, because the operator
  cannot see which tier answered.

## The audit log

waldo records the commands it runs on a target to
`~/.waldo/sessions/<name>.audit.jsonl`, mode 0600. Two consequences worth
knowing:

- **It contains whatever the agent ran**, which can include a secret that
  appeared on a command line. It is local and never transmitted, but it is a
  file worth the same care as your shell history. `WALDO_NO_AUDIT=1` disables it.
- **It is evidence, not a control.** It records what waldo was asked to do, not
  everything that happened on the target, and a compromised target can do things
  waldo never sees.

## What is out of scope

- **Prompt injection from target output.** This is real, and it is documented
  rather than fixed: file contents and command output from a target flow into
  the context of an agent holding your credentials. See
  [docs/SECURITY.md](docs/SECURITY.md) for the reasoning and the mitigations.
- **A malicious target lying about its own state.** waldo reports what the
  target says. It verifies transport integrity, not target honesty.
- **Your own SSH configuration.** waldo delegates destination resolution to your
  `ssh` client so that jump hosts, certificates and hardware tokens keep
  working, and inherits whatever that configuration does.

The full threat model is in [docs/SECURITY.md](docs/SECURITY.md).

## Supported versions

waldo is pre-1.0. Fixes land on `main` and in the next release; there are no
maintained release branches yet.
