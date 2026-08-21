# Grok Build (xAI)

Installed as `grok` CLI. Version tested: **1.0.5**.

## The shell seam

Grok Build does **not** walk PATH for `bash`. It spawns `$SHELL` by absolute
path. Setting `GROK_SHELL` in the environment is recognised by
`xai_grok_config::shell` (binary strings in 1.0.5) but the live tool path
observed on 1.0.5 honours `$SHELL`.

`reach grok` therefore:

1. Sets `SHELL` and `GROK_SHELL` to the PATH shim's `bash`.
2. Prepends the shim directory to `PATH` as a fallback.

### Envelope (verified on 1.0.5)

A `run_terminal_command` call arrives as:

```
$SHELL -O extglob -c '<envelope>' -- <user command>
```

The envelope sources a local snapshot from fd 3, then evals
`__grok_user_cmd="$1"`. Forwarding that script to the target fails. The
shim unwraps the payload after `--` and runs only that command on the
session target.

Before any tool call, Grok snapshots the login environment with
`$SHELL -lc 'source "$HOME/.bashrc"; …'`. Those invocations stay local.

## File tools

`read_file`, `search_replace`, `list_dir`, and `grep` call the local
filesystem. `reach grok` denies them with `--deny Read/Edit/Write/Grep`
(permission prefixes that work in the TUI) and `--no-subagents` so a
child cannot reopen them. The model uses `run_terminal_command` instead.

## Seam coverage

| Tool surface | Mechanism | Status |
|---|---|---|
| `run_terminal_command` | `$SHELL` → shim, envelope unwrapped | **✓ remote** (1.0.5) |
| `read_file` / `search_replace` / `list_dir` / `grep` | `--deny` | **denied** (use shell) |
| subagents | `--no-subagents`, `GROK_SUBAGENTS=0` | **disabled** |

## Probe

`reach harness verify grok` drives one offline turn against the chat-completions
mock via `GROK_CLI_CHAT_PROXY_BASE_URL`. A match with the target hostname is
VerdictOK.

## Implementation status

`reach grok` is implemented. Envelope unwrap and `$SHELL` intercept were
measured against grok 1.0.5 on macOS arm64 by pointing `$SHELL` at a logging
wrapper and running a real `run_terminal_command` turn.
