# Grok Build (xAI)

Installed as `grok` CLI. Version tested: **1.0.5**.

## The shell seam

Grok Build does **not** walk PATH for `bash`. It spawns `$SHELL` by absolute
path. Setting `GROK_SHELL` in the environment is recognised by
`xai_grok_config::shell` (binary strings in 1.0.5) but the live tool path
observed on 1.0.5 honours `$SHELL`.

`reach grok` therefore:

1. Sets `SHELL` and `GROK_SHELL` to the PATH shim's `bash`.
2. Prepends the shim directory to `PATH` — for everything grok starts, not
   for grok's own shell lookup. Measured on 1.0.5, a shim reachable only
   through `PATH` is never consulted, so it is not a fallback for this seam.

Grok resolves `$SHELL` by basename: a wrapper not named `bash` (or another
shell it recognises) is ignored in favour of a system shell. reach's shim is
named `bash`, which is what makes the override take.

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

`read_file`, `search_replace`, `list_dir`, `grep`, and `write` call the local
filesystem. `write` is advertised in a live 1.0.5 session and documented
nowhere; a list built from the documentation alone leaves the agent a local
file writer.

`reach grok` removes them with a generated **agent profile** — a `.md` file
with a `disallowedTools` frontmatter list, written to
`$REACH_HOME/grok-agents/<session>/reach-exec.md` and passed as `--agent`.

Not `--deny`, and the difference is the whole adapter. Grok classifies a shell
command that reads a file under the same permission prefix as `read_file`, so
`--deny Read` denies `cat` as well; `--deny Write` denies `cat > file` and
`--deny Edit` denies `sed -i`. Those are exactly the commands this document
tells the model to use once the native tools are gone, so a deny-rule adapter
can run `hostname` and little else — no reading, writing or editing a file on
the target by any route. `disallowedTools` removes the tools from the model's
view without teaching the permission layer anything about shell commands, so
the shell stays whole.

The profile has to exist before grok starts. `--agent` pointing at a missing
path is **not** an error to grok: it falls back to the default agent, local
file tools and all, and says nothing. That failure is indistinguishable from a
working session, so reach treats a write error as fatal rather than launching
without the profile.

`--no-subagents`, `GROK_SUBAGENTS=0` and an `Agent` entry in the profile keep a
child session from reopening the toolset the profile closed.

## Seam coverage

| Tool surface | Mechanism | Status |
|---|---|---|
| `run_terminal_command` | `$SHELL` → shim, envelope unwrapped | **✓ remote** (1.0.5) |
| `read_file` / `search_replace` / `list_dir` / `grep` / `write` | agent profile `disallowedTools` | **removed** (use shell) |
| subagents | `--no-subagents`, `GROK_SUBAGENTS=0`, profile `Agent` | **disabled** |

## Probe

`reach harness verify grok` drives one offline turn against the chat-completions
mock via `GROK_CLI_CHAT_PROXY_BASE_URL`. A match with the target hostname is
VerdictOK.

## What is verified, and what is not

Measured against grok 1.0.5: the `$SHELL` intercept, the envelope unwrap, the
login-snapshot classification, and — driven headless — the toolset the profile
leaves behind, with reading, writing and editing a file all landing on the
target and a host-only path reported as absent from inside the session.

Not yet measured: that the profile's `disallowedTools` applies in the
**interactive TUI** specifically. The evidence is good — grok marks `--tools`,
`--disallowed-tools` and `--max-turns` as headless-only and prints a warning
when they are used in the TUI, while `--agent` carries no such note and agent
profiles are also selected by `[agent]` config and `GROK_AGENT`, which only
make sense for interactive sessions — but it is inference, not measurement.

It matters because the failure is silent and looks like success: a session
whose shell reaches the target while its file tools quietly read the
operator's own disk. To check it in a live TUI, ask the model to
`use the read_file tool to read /etc/hosts`. "No such tool" is the expected
answer; the file's contents mean the profile is not applying and the adapter
needs a different mechanism.

## Implementation status

`reach grok` is implemented. Envelope unwrap and `$SHELL` intercept were
measured against grok 1.0.5 on macOS arm64 by pointing `$SHELL` at a logging
wrapper and running a real `run_terminal_command` turn.

The adapter was then run end to end against a container target: `reach harness
verify grok` returns the target's hostname, and a real session read, created
and edited files on the target, with nothing written locally and a
host-only path reported as absent from inside the session.
