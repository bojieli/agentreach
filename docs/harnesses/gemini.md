# Gemini CLI (Google)

Open-source TypeScript agent by Google.  Installed as `gemini` CLI.

## The shell seam

Gemini CLI resolves its shell in `packages/core/src/tools/shell.ts` via
`getShellConfiguration()`, which returns the bare name `"bash"`.  Node's
`child_process.spawn` resolves that name by walking PATH — the same mechanism
every tool that calls `execvp("bash")` relies on.  The reach PATH shim places a
controlled `bash` binary earlier on PATH, so the shim intercepts every
`run_shell_command` call natively, with no patching and no env-var tricks.

This is the cleanest possible seam: the interception point is PATH itself, a
standard POSIX guarantee that every process respects.

## File tools: excluded via settings.json

Gemini CLI exposes a large set of built-in tools that call Node's `fs` module
directly, bypassing the shell: `read_file`, `write_file`, `replace` (the file
editor), `glob`, `grep_search`, `list_directory`, `read_many_files`, `web_fetch`,
`google_web_search`, `write_todos`, `activate_skill`, and others.  These must not
be advertised to the model, because they act on the local filesystem — not the
session target — and have no intercept point.

`reach gemini` sets `HOME` to a managed directory whose `.gemini/settings.json`
contains an `excludeTools` array that names every built-in tool except
`run_shell_command`:

```json
{
  "excludeTools": [
    "read_file", "write_file", "replace", "glob",
    "grep_search", "list_directory", "read_many_files",
    "web_fetch", "google_web_search",
    "write_todos", "activate_skill", "get_internal_docs",
    "ask_user", "enter_plan_mode", "exit_plan_mode",
    "update_topic", "complete_task", "invoke_agent",
    "tracker_create_task", "tracker_update_task", "tracker_get_task",
    "tracker_list_tasks", "tracker_add_dependency", "tracker_visualize",
    "read_mcp_resource", "list_mcp_resources"
  ]
}
```

**Tool name precision matters.**  Gemini CLI matches `excludeTools` entries
against the canonical `TOOL_NAME` constants from
`packages/core/src/tools/definitions/base-declarations.ts`.  Common
shorthands differ from the canonical names and are silently ignored:

| Shorthand (wrong) | Canonical name (correct)   |
|-------------------|---------------------------|
| `edit`            | `replace`                  |
| `grep`            | `grep_search`              |
| `ls`              | `list_directory`           |
| `web_search`      | `google_web_search`        |

With the managed `HOME`, only `run_shell_command` is visible to the model.
Shell commands route through the PATH shim and execute on the session target.

## Credential forwarding

Gemini CLI reads authentication from `HOME/.gemini/google-accounts.json` (OAuth
flow) or from the `GEMINI_API_KEY` environment variable.  reach symlinks
`google-accounts.json` and `installation_id` from the operator's real `~/.gemini`
into the managed `HOME/.gemini`, so OAuth logins remain valid.  `GEMINI_API_KEY`
passes through the environment unchanged.

## Seam coverage

| Tool surface          | Mechanism                         | Status                  |
|-----------------------|-----------------------------------|-------------------------|
| `run_shell_command`   | PATH shim (bare `bash` name)      | **✓ remote**            |
| `read_file`           | `excludeTools` in settings.json   | **denied** (use shell)  |
| `write_file`          | `excludeTools` in settings.json   | **denied** (use shell)  |
| `replace`             | `excludeTools` in settings.json   | **denied** (use shell)  |
| `glob`                | `excludeTools` in settings.json   | **denied** (use shell)  |
| `grep_search`         | `excludeTools` in settings.json   | **denied** (use shell)  |
| `list_directory`      | `excludeTools` in settings.json   | **denied** (use shell)  |
| `read_many_files`     | `excludeTools` in settings.json   | **denied** (use shell)  |
| `web_fetch`           | `excludeTools` in settings.json   | **denied**              |
| `google_web_search`   | `excludeTools` in settings.json   | **denied**              |
| `write_todos`         | `excludeTools` in settings.json   | **denied**              |
| `activate_skill`      | `excludeTools` in settings.json   | **denied**              |
| `get_internal_docs`   | `excludeTools` in settings.json   | **denied**              |
| `ask_user`            | `excludeTools` in settings.json   | **denied** (headless)   |
| `enter_plan_mode`     | `excludeTools` in settings.json   | **denied** (headless)   |
| `exit_plan_mode`      | `excludeTools` in settings.json   | **denied** (headless)   |
| `update_topic`        | `excludeTools` in settings.json   | **denied**              |
| `complete_task`       | `excludeTools` in settings.json   | **denied**              |
| `invoke_agent`        | `excludeTools` in settings.json   | **denied**              |
| `tracker_*` (6 tools) | `excludeTools` in settings.json  | **denied**              |
| `read_mcp_resource`   | `excludeTools` in settings.json   | **denied**              |
| `list_mcp_resources`  | `excludeTools` in settings.json   | **denied**              |

## Probe

The seam guard (`reach harness verify gemini`) runs Gemini CLI against a minimal
mock that speaks the Gemini API's `streamGenerateContent` format.  The mock uses
a `DialectGemini` handler — separate from the OpenAI chat/responses dialects —
because the Gemini API wire format (candidates / functionCall / functionResponse)
is not OpenAI-compatible.

`GOOGLE_GEMINI_BASE_URL` redirects the `@google/genai` SDK to the local mock.
The mock issues a `run_shell_command` call of `echo <marker>; hostname`; the
probe verifies that the tool output contains the session target's hostname.

The probe sets `--yolo` (ApprovalMode.YOLO) so the shell tool executes without
waiting for user confirmation — a headless probe run has no TTY to accept on.

The probe also writes its own `.gemini/settings.json` in the throwaway `HOME`
(via `geminiPrepare`) using the same deny-list as the production managed home.
This prevents `ask_user`, `enter_plan_mode`, and `exit_plan_mode` from appearing
as tool choices that could block the headless probe run.

## Implementation status

`reach gemini` is implemented.  `reach harness verify gemini` probes the PATH
shim seam end-to-end against the live session target.
