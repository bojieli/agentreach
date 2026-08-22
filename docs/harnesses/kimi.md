# Kimi Code

Probed against **0.34.0** and **0.37.2**. MIT-licensed, npm-bundled TypeScript.

## Creating the seam

Kimi Code 0.37.2 spawns its shell by absolute path — it probes `/bin/bash`,
`/usr/bin/bash`, `/usr/local/bin/bash` in that order, ignoring `PATH`.  A
plain PATH shim cannot intercept this.

Kimi is open source, so reach patches the npm bundle directly.
`contrib/kimi-shell-path-patch.mjs` rewrites both probe sites inside Kimi's
compiled bundle to prepend `process.env.KIMI_SHELL_PATH` to the candidate
list, when set.  `reach kimi` sets `KIMI_SHELL_PATH` to the PATH shim's
`bash` entry, so every `Bash` tool call the model issues routes through the
shim and runs on the session's target.

The managed binary lives under `~/.reach/kimi-<version>/node_modules/.bin/kimi`
(installed once, patched in-place, never updated by Kimi's own updater).
`reach kimi` resolves the newest reach-managed binary first, then falls back to
whatever `kimi` is on PATH.  The seam guard measures the chosen binary before
launch and caches the verdict — an unpatched binary is refused.

## Kimi's cd-prefix injection

Kimi wraps every Bash call with `cd '<local-cwd>' && <command>`.  Forwarded
verbatim, this cd fails on the target (the path does not exist there) and the
command never runs.

`reach kimi` sets `REACH_EXEC_WORKSPACE` to the operator's current directory.
The PATH shim reads this variable and rewrites the cd prefix:
`cd '<local-cwd>'` becomes `cd '<session-target-workspace>'`.  The agent's
working directory on the target is correct, and the model never sees the
rewrite.

## File tools: denied via config

Kimi's native `read_file`, `write_file`, `edit`, `glob`, `grep`, and
`read_media_file` tools use Node's `fs` API directly.  There is no seam that
redirects them to the target.

`reach kimi` builds a managed `KIMI_CODE_HOME` whose `config.toml` adds a
deny rule for all six tools.  The agent reaches the remote filesystem through
the `Bash` tool instead (which does run on the target), using shell commands
like `cat`, `find`, `grep`, `sed`, and `patch`.

This costs one tool call per file operation versus zero for native tools, but
it is correct: every file read and write lands on the target, never on the
operator's machine.

## Seam coverage

| Tool surface | Mechanism | Status |
|---|---|---|
| `Bash` (shell commands) | KIMI_SHELL_PATH → shim | **✓ remote** |
| `read_file` | config.toml deny | **denied** (use shell) |
| `write_file` | config.toml deny | **denied** (use shell) |
| `edit` | config.toml deny | **denied** (use shell) |
| `glob` | config.toml deny | **denied** (use shell) |
| `grep` | config.toml deny | **denied** (use shell) |
| `read_media_file` | config.toml deny | **denied** |

## The guard

`reach harness verify kimi` drives one offline scripted turn (mock model over
the OpenAI chat-completions wire) against the resolved kimi binary with
`KIMI_SHELL_PATH` set.  The probe instructs a `Bash` tool call of
`echo <marker>; hostname` and reads the hostname from the tool output.  A match
with the session target's hostname caches a VerdictOK; a local hostname caches
VerdictBypassed and `reach kimi` refuses to launch.

Cache is keyed on the kimi binary's version string and invalidated when the
binary changes — re-running `reach harness verify kimi` after a patch or
version upgrade re-measures from scratch.
