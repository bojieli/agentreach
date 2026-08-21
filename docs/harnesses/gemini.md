# Gemini CLI (Google)

Open-source TypeScript agent by Google.  Installed as `gemini` CLI.

## The shell seam

Gemini CLI resolves its shell in `packages/core/src/tools/shell.ts` via
`getShellConfiguration()`, which returns the bare name `"bash"`.  Node's
`child_process.spawn` resolves that name by walking PATH — the same mechanism
every tool that calls `execvp("bash")` relies on.  The waldo PATH shim places a
controlled `bash` binary earlier on PATH, so the shim intercepts every
`run_shell_command` call natively, with no patching and no env-var tricks.

This is the cleanest possible seam: the interception point is PATH itself, a
standard POSIX guarantee that every process respects.

## File tools: excluded via settings.json

Gemini CLI exposes `read_file`, `write_file`, `edit`, `glob`, `grep`, `ls`,
`read_many_files`, `web_fetch`, and `web_search` tools that call Node's `fs`
module directly, bypassing the shell.  `GOOSE_SHELL` has no effect on these.

`waldo gemini` sets `HOME` to a managed directory whose `.gemini/settings.json`
contains an `excludeTools` array listing every file and web tool:

```json
{
  "excludeTools": [
    "read_file", "write_file", "edit", "glob", "grep", "ls",
    "read_many_files", "web_fetch", "web_search", "memory"
  ]
}
```

Gemini CLI reads `settings.json` from `HOME/.gemini/settings.json`.  With the
managed `HOME`, the file tools are not advertised to the model; only
`run_shell_command` is available.  The agent uses shell commands for file access
instead, which run on the target.

## Credential forwarding

Gemini CLI reads authentication from `HOME/.gemini/google-accounts.json` (OAuth
flow) or from the `GEMINI_API_KEY` environment variable.  waldo symlinks
`google-accounts.json` and `installation_id` from the operator's real `~/.gemini`
into the managed `HOME/.gemini`, so OAuth logins remain valid.  `GEMINI_API_KEY`
passes through the environment unchanged.

## Seam coverage

| Tool surface    | Mechanism                     | Status      |
|-----------------|-------------------------------|-------------|
| `run_shell_command` | PATH shim (bare `bash` name) | **✓ remote** |
| `read_file`     | `excludeTools` in settings.json | **denied** (use shell) |
| `write_file`    | `excludeTools` in settings.json | **denied** (use shell) |
| `edit`          | `excludeTools` in settings.json | **denied** (use shell) |
| `glob`          | `excludeTools` in settings.json | **denied** (use shell) |
| `grep`          | `excludeTools` in settings.json | **denied** (use shell) |
| `ls`            | `excludeTools` in settings.json | **denied** (use shell) |
| `read_many_files` | `excludeTools` in settings.json | **denied** (use shell) |
| `web_fetch`     | `excludeTools` in settings.json | **denied** |
| `web_search`    | `excludeTools` in settings.json | **denied** |

## Probe

The seam guard (`waldo harness verify gemini`) runs Gemini CLI against a minimal
mock that speaks the Gemini API's `streamGenerateContent` format.  The mock uses
a `DialectGemini` handler — separate from the OpenAI chat/responses dialects —
because the Gemini API wire format (candidates / functionCall / functionResponse)
is not OpenAI-compatible.

`GOOGLE_GEMINI_BASE_URL` redirects the `@google/genai` SDK to the local mock.
The mock issues a `run_shell_command` call of `echo <marker>; hostname`; the
probe verifies that the tool output contains the session target's hostname.

The probe sets `--yolo` (ApprovalMode.YOLO) so the shell tool executes without
waiting for user confirmation — a headless probe run has no TTY to accept on.

## Implementation status

`waldo gemini` is implemented.  `waldo harness verify gemini` probes the PATH
shim seam end-to-end against the live session target.
