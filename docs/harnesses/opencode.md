# opencode

Probed against **1.18.18**.

## Seam

opencode documents that a custom tool whose filename matches a built-in **takes
precedence over it**. That makes opencode the only harness where reach needs no
workaround at all — no envelope parsing, no denied tools, no mirror. The model
still sees `read` and `bash`, and they act on the target.

Confirmed by inspecting the shipped binary and the published plugin package:

- built-in tool names present: `bash`, `read`, `write`, `list`, `glob`, `patch`
- plugin hooks present: `tool.execute.before`, `tool.execute.after`,
  `chat.message`, `chat.params`, `permission.ask`
- tool directories: `.opencode/tool/` (project), `~/.config/opencode/tool/` (global)
- the `tool()` helper signature, taken from `@opencode-ai/plugin@1.18.18`:

```ts
tool({
  description: string,
  args: ZodRawShape,          // note: a raw shape, not a ZodObject
  execute(args, ctx): Promise<ToolResult>,
})
// ToolResult = string | { title?, output, metadata?, attachments? }
// tool.schema is the zod namespace, so no separate zod import is needed
```

## Install

```console
reach up ssh://box/srv/app
reach opencode install          # writes ./.opencode/tool/*.ts
reach opencode install --global # or ~/.config/opencode/tool/
reach opencode uninstall        # restore the built-ins
```

reach *generates* the tool files rather than shipping them to be copied. Each is
self-contained: opencode treats every file in the tool directory as a tool, so a
shared helper module placed there would itself be loaded as one.

## Verification status

**Partially verified. Do not assume this path works end to end.**

What was confirmed on this machine:

- The generated files are accepted by opencode: it loads the project config and
  the tool directory and proceeds to model selection without a parse or
  registration error.
- The `reach fs` verbs the tools invoke (`read`, `write`, `ls`, `grep`, `glob`)
  are covered by unit and integration tests against a real sshd, including
  binary-safe round trips.

What was **not** confirmed:

- That opencode routes its built-in tool *names* to these files and executes
  them against the target in a real turn. One run using the mock model server
  (`test/mockmodel/server.py`) did produce a second conversation turn after the
  tool call, but the tool result could not be captured, and subsequent runs hung
  during opencode's initialisation before reaching the model. No valid model
  credential was available to complete the round trip another way.

Until an end-to-end run exists, treat the opencode adapter as untested code
written against a documented API — which is exactly the standard of evidence the
rest of this project avoids relying on.

## Mock model server

`test/mockmodel/server.py` speaks enough of the OpenAI chat-completions
streaming protocol to tell a harness "call this tool with these arguments" and
report what came back. It exists so that agent-level tests can run in CI and for
contributors without an API key.

```console
python3 test/mockmodel/server.py --port 8911 --tool read \
    --args '{"filePath":"/srv/app/README.md"}'
```

Point any harness that accepts an OpenAI-compatible base URL at it. The server's
protocol output is verified directly with curl in
`test/e2e/mockmodel_test.sh`; what remains unproven is opencode's side of the
exchange, not the server's.
