# Claude Code harness

`reach claude` launches Claude Code with every Bash tool call intercepted by
reach's shell seam, so it runs on the session's remote target instead of the
local machine.

## Shell seam: `CLAUDE_CODE_SHELL_PREFIX`

Claude Code (v0.2+) supports a first-class intercept mechanism:

```
CLAUDE_CODE_SHELL_PREFIX=/path/to/binary
```

When this variable is set, Claude Code runs the command through the named
program instead of a shell:

```sh
<prefix> "<shell envelope>"
```

The envelope arrives as a single argument. Earlier versions prefixed it with
`-c`, so reach joins whatever argv it is given rather than looking for a
particular flag; both shapes work.

reach sets `CLAUDE_CODE_SHELL_PREFIX` to its `reach-shell-prefix` binary alias,
which sits at `~/.reach/bin/reach-shell-prefix` (a symlink to the reach
binary). When invoked as `reach-shell-prefix`, the reach binary strips the
shell envelope and forwards the portable command to the session target over the
tunnel.

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

The `reach-shell-prefix` binary strips `source …` and `pwd -P >| …` lines,
forwarding only the actual command to the target. This envelope parsing lives in
`internal/envelope`.

## Hook commands come through the same seam

Claude Code runs **hook commands** through `CLAUDE_CODE_SHELL_PREFIX` as well
as Bash tool calls (observed on 2.1.278). A hook belongs on the operator's
machine — it names local paths, reads a local transcript, and in reach's case
is the reach binary itself — so forwarding it to the target is always wrong:

```
bash: line 1: /Users/you/.local/bin/reach: No such file or directory
```

reach tells the two apart in `runShellPrefix` before deciding where to run
anything. A tool call carries the envelope described above; a hook arrives
bare, and Claude Code sets `CLAUDE_PROJECT_DIR` only in a hook's environment.
Both signals must agree before reach runs a command locally, because the two
ways of being wrong are not comparable: a hook sent to the target fails
visibly, while an agent's command run locally executes on the operator's own
machine while the agent reports it as remote. Anything ambiguous goes to the
target.

## Exec mode and mirror mode

### Exec mode (default)

reach denies Claude Code's native file tools (Read, Edit, Write, Glob, Grep,
NotebookEdit) by injecting a `--settings` file. These tools have no seam: they
act on the local filesystem, not the target. All file access must go through the
Bash tool, which runs on the target.

The same settings file wires reach's hook into `PreToolUse` for Bash, because
denying the file tools is not enough on its own. Before running a command,
Claude Code resolves the paths inside it against the *local* filesystem and the
session's local working directories, and refuses what falls outside them —

```
cat in '/srv/app/main.go' was blocked. For security, Claude Code may only
concatenate files from the allowed working directories for this session
```

— which is every path worth naming in an exec-mode session. A write is refused
the same way (`Output redirection to '/srv/app/x' was blocked`) with no prompt
attached, so the operator cannot approve it even when it is exactly what they
asked for.

The hook answers with what Claude Code cannot know: the command is not going to
run on this machine at all. So it returns **allow** for every Bash command.
reach has no opinion of its own about what may run on the target — the operator
connected it there on purpose — so it never asks and never denies. An
operator's own `deny` rule still outranks the allow.

Plan mode is the one exception. There the hook allows only a command that reads
— `cat`, `sed -n 'A,Bp'`, a single `sed 's/…/…/'`, `rg`, `ls`, `find` without
`-exec` or `-delete`, and pipelines or `;`-sequences of those, with output
discarded to `/dev/null` or joined with `2>&1` if need be (the list is in
`cmd/reach/bashpolicy.go`) — and leaves everything else to Claude Code, whose
plan mode refuses it. An allow there would carry a write straight past the
operator's "change nothing yet".

One shape is beyond the hook's reach: `cd /srv/app && grep -rn x app/utils.py`
is still asked about every time, because Claude Code treats a compound command
whose working directory it cannot resolve as needing approval regardless of
what a hook says. The exec-mode system prompt therefore tells the agent to pass
the target's absolute paths to a command rather than changing into them first.

### Mirror mode

When the session was created with `reach session new --mirror`, reach wires its
hook into Claude Code's PreToolUse and PostToolUse events via a `--settings`
file. The hook intercepts Read/Write/Edit/Glob/Grep/NotebookEdit, fetching the
file from the target before a read and writing it back after a write.

Mirror mode lets Claude Code's native file tools work transparently against the
remote target. It requires the target to support the sha256 content-hash tier
(`reach doctor` will confirm this).

## Seam probe

`reach harness verify claude` probes whether CLAUDE_CODE_SHELL_PREFIX is
honoured by the installed Claude Code version:

1. A mock Anthropic Messages API server (DialectAnthropic) is started locally.
2. Claude Code is launched with `ANTHROPIC_BASE_URL` pointing at the mock.
3. The mock scripts a two-turn conversation: turn 1 emits a `tool_use` block
   asking Claude to run `echo <marker>; hostname`; turn 2 records the
   `tool_result` from the prefix invocation.
4. The recorded result is compared against the session target's hostname.
5. If they match: verdict **ok** (seam routes to target).
6. If the local hostname appears: verdict **BYPASSED** (seam broken).

The verdict is cached per Claude Code version. `reach claude` consults the cache
at startup and re-probes automatically when the version changes.

### Seam coverage

| Vector                | Covered? | How                              |
|-----------------------|----------|----------------------------------|
| Shell (Bash tool)     | ✓        | CLAUDE_CODE_SHELL_PREFIX hook    |
| Read (exec mode)      | ✓        | tool denied via settings file    |
| Bash paths (exec)     | ✓        | PreToolUse hook decides per command |
| Hook commands         | ✓        | kept local; never forwarded to the target |
| Write (exec mode)     | ✓        | tool denied via settings file    |
| Read (mirror mode)    | ✓        | PreToolUse hook fetches from target |
| Write (mirror mode)   | ✓        | PostToolUse hook writes to target |
| WebFetch / WebSearch  | —        | Claude's own network; not a seam |

## Doctor output

`reach doctor` reports the Claude Code seam status in the LOCAL HARNESSES
section. The seam note includes the cached verdict when one exists:

```
LOCAL HARNESSES
  Claude Code   found (claude) — seam: CLAUDE_CODE_SHELL_PREFIX → reach-shell-prefix (verified ok)
```

Run `reach harness verify claude` to populate or refresh the verdict.

## Troubleshooting

**"The scripted command ran on the local machine"** — the installed version of
Claude Code is not honouring CLAUDE_CODE_SHELL_PREFIX. Check:

1. `claude --version` — is this a version that supports the prefix mechanism?
2. `reach doctor` — confirm `reach-shell-prefix` is current.
3. Try `CLAUDE_CODE_SHELL_PREFIX=/usr/bin/env claude -c 'echo test'` manually
   to see whether the hook fires.

If the mechanism is broken in your version, either downgrade or use
`--allow-local-file-tools` with the understanding that file tools will act on
the local machine.

**"bash: line 1: /Users/…: No such file or directory" on every tool call** —
a hook command is being forwarded to the target instead of run locally, which
every reach before 0.7.0 does. Upgrade; on an older build the only workaround is
to remove the hook from `settings.json` for the duration of the session, which
also gives up reach's own `PreToolUse` path decisions.

**"Cannot determine the Claude Code version"** — `claude` is not in PATH or
does not respond to `claude --version`. Install Claude Code or confirm it is in
PATH before running `reach claude`.
