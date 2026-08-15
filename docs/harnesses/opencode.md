# opencode

**Unverified.** opencode was not installed on the machine used for verification,
so everything here is derived from its documentation rather than from experiment.
It is written down so the adapter has a spec; treat the claims as untested until
an end-to-end run exists.

## Seam

opencode documents that a custom tool whose filename matches a built-in **takes
precedence over it**. That makes opencode the only harness where waldo needs no
workaround at all: shadowing `bash`, `read`, `write`, `edit`, `grep` and `glob`
gives full fidelity with no mirror, no denied tools and no envelope parsing.

Plugins additionally expose `tool.execute.before` / `tool.execute.after`, which
can inspect and rewrite tool arguments.

## Planned shape

A small TypeScript plugin whose tools shell out to `waldo` (or speak to it over
a socket), so the backend logic stays in one implementation shared with the
other harnesses.

Unlike Claude Code and Codex, opencode is API-key based rather than
subscription-based — which is a reason to support it, not a reason to prefer it:
the point of waldo is that the harness choice stays the operator's.
