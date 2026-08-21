# Claude Code harness

`waldo claude` launches Claude Code with every Bash tool call intercepted by
waldo's shell seam, so it runs on the session's remote target instead of the
local machine.

## Shell seam: `CLAUDE_CODE_SHELL_PREFIX`

Claude Code (v0.2+) supports a first-class intercept mechanism:

```
CLAUDE_CODE_SHELL_PREFIX=/path/to/binary
```

When this variable is set, every Bash tool call is wrapped as:

```sh
<prefix> -c "<shell envelope>"
```

waldo sets `CLAUDE_CODE_SHELL_PREFIX` to its `waldo-shell-prefix` binary alias,
which sits at `~/.waldo/bin/waldo-shell-prefix` (a symlink to the waldo
binary). When invoked as `waldo-shell-prefix`, the waldo binary detects the
`-c <envelope>` argument pattern, strips the shell envelope, and forwards the
portable command to the session target over the tunnel.

This seam is robust: even if Claude Code resolves its shell by absolute path
(e.g. from `/etc/passwd`) rather than walking PATH, the prefix hook fires first.
PATH shim harnesses (Codex, Gemini, Goose) share a vulnerability to changes in
how the harness resolves its shell — the `CLAUDE_CODE_SHELL_PREFIX` mechanism is
immune to that class of regression.

## Envelope parsing

Claude Code wraps commands in a shell envelope before passing them to the
prefix:

```sh
source /path/to/claude-code-snapshot.sh
pwd -P >| /tmp/claude-pwd-file
<actual command>
```

The `waldo-shell-prefix` binary strips `source …` and `pwd -P >| …` lines,
forwarding only the actual command to the target. This envelope parsing lives in
`internal/envelope`.

## Exec mode and mirror mode

### Exec mode (default)

waldo denies Claude Code's native file tools (Read, Edit, Write, Glob, Grep,
NotebookEdit) by injecting a `--settings` file. These tools have no seam: they
act on the local filesystem, not the target. All file access must go through the
Bash tool, which runs on the target.

### Mirror mode

When the session was created with `waldo session new --mirror`, waldo wires its
hook into Claude Code's PreToolUse and PostToolUse events via a `--settings`
file. The hook intercepts Read/Write/Edit/Glob/Grep/NotebookEdit, fetching the
file from the target before a read and writing it back after a write.

Mirror mode lets Claude Code's native file tools work transparently against the
remote target. It requires the target to support the sha256 content-hash tier
(`waldo doctor` will confirm this).

## Seam probe

`waldo harness verify claude` probes whether CLAUDE_CODE_SHELL_PREFIX is
honoured by the installed Claude Code version:

1. A mock Anthropic Messages API server (DialectAnthropic) is started locally.
2. Claude Code is launched with `ANTHROPIC_BASE_URL` pointing at the mock.
3. The mock scripts a two-turn conversation: turn 1 emits a `tool_use` block
   asking Claude to run `echo <marker>; hostname`; turn 2 records the
   `tool_result` from the prefix invocation.
4. The recorded result is compared against the session target's hostname.
5. If they match: verdict **ok** (seam routes to target).
6. If the local hostname appears: verdict **BYPASSED** (seam broken).

The verdict is cached per Claude Code version. `waldo claude` consults the cache
at startup and re-probes automatically when the version changes.

### Seam coverage

| Vector                | Covered? | How                              |
|-----------------------|----------|----------------------------------|
| Shell (Bash tool)     | ✓        | CLAUDE_CODE_SHELL_PREFIX hook    |
| Read (exec mode)      | ✓        | tool denied via settings file    |
| Write (exec mode)     | ✓        | tool denied via settings file    |
| Read (mirror mode)    | ✓        | PreToolUse hook fetches from target |
| Write (mirror mode)   | ✓        | PostToolUse hook writes to target |
| WebFetch / WebSearch  | —        | Claude's own network; not a seam |

## Doctor output

`waldo doctor` reports the Claude Code seam status in the LOCAL HARNESSES
section. The seam note includes the cached verdict when one exists:

```
LOCAL HARNESSES
  Claude Code   found (claude) — seam: CLAUDE_CODE_SHELL_PREFIX → waldo-shell-prefix (verified ok)
```

Run `waldo harness verify claude` to populate or refresh the verdict.

## Troubleshooting

**"The scripted command ran on the local machine"** — the installed version of
Claude Code is not honouring CLAUDE_CODE_SHELL_PREFIX. Check:

1. `claude --version` — is this a version that supports the prefix mechanism?
2. `waldo doctor` — confirm `waldo-shell-prefix` is current.
3. Try `CLAUDE_CODE_SHELL_PREFIX=/usr/bin/env claude -c 'echo test'` manually
   to see whether the hook fires.

If the mechanism is broken in your version, either downgrade or use
`--allow-local-file-tools` with the understanding that file tools will act on
the local machine.

**"Cannot determine the Claude Code version"** — `claude` is not in PATH or
does not respond to `claude --version`. Install Claude Code or confirm it is in
PATH before running `waldo claude`.
