# Claude Code

Verified against **2.1.233**. Re-verify with `make conformance`.

## Seam

`CLAUDE_CODE_SHELL_PREFIX` — the prefix program receives the entire command
envelope as a **single argv element**. waldo installs a symlink named
`waldo-shell-prefix` and points the variable at it, because Claude Code stats
the value as a program path: a value like `"waldo shell-prefix"` is looked up as
one filename and fails.

`CLAUDE_CODE_SHELL` also works but is fussier — the path string must contain
`bash` or `zsh` and be executable, and on failure it silently falls back to the
local shell, which is a correctness hazard rather than an error.

## Why the envelope is parsed

See [../RESEARCH.md](../RESEARCH.md) for the captured string. Two segments are
local-only and are removed:

- the shell snapshot `source`, which references local paths and leaks the
  operator's username and directory layout to the target
- `pwd -P >| /tmp/claude-<rand>-cwd`, which is how `cd` persists between calls;
  forwarded verbatim it is written remotely while the harness reads locally, so
  directory changes silently stop working

## Modes

**exec** (default) — Bash runs on the target. `Read`, `Edit`, `Write`, `Glob`
and `Grep` are **denied**, because they have no seam and would keep acting on
the local filesystem while the agent believes otherwise. The agent is told to
use shell commands, which are transparently remote.

**mirror** — additionally wires waldo's hook into the file tools. Each file is
fetched from the target the moment a tool opens it and written back when
changed, so `Read`, `Edit` and `Write` work natively on remote content.
`Grep`/`Glob` remain denied: the mirror holds only files already opened, so a
search would report confidently incomplete results.

```console
waldo up ssh://box/srv/app --mode mirror
waldo claude
```

Writes are guarded by a content digest taken at fetch time. If the file changed
on the target in between, the write is refused and the agent is told to re-read
and redo it, rather than overwriting someone else's change from a stale base.

## Known limits

- `NODE_OPTIONS=--require` does not work: Claude Code is a compiled Node SEA.
  In-process patching of `node:fs` is impossible.
- Output is streamed, but a command producing enormous output is capped; waldo
  keeps the head and the tail, since failures are usually at the end.
