#!/usr/bin/env python3
"""A minimal OpenAI-compatible model server that drives a scripted tool call.

reach's hardest claim to verify is that a harness's *own* tools end up acting on
the target. Proving that normally needs a real model, which means an API key,
which means the test cannot run in CI and cannot run for a contributor who has
no account.

This server removes the model from the loop while keeping the harness in it. It
speaks just enough of the OpenAI chat-completions streaming protocol to tell a
harness "call this tool with these arguments", then reports what came back. Any
harness that can point at an OpenAI-compatible base URL can be tested this way.

Usage:
    server.py --port 8899 --tool read --args '{"filePath": "/srv/app/README.md"}'
    server.py --port 8899 --dialect responses --command 'hostname'

It exits after the conversation completes, printing the tool result it observed
on stdout as JSON, so a test can assert on it.

Two dialects are supported:

- chat (default): the OpenAI chat-completions streaming protocol.
- responses: the OpenAI Responses API streaming protocol, which is the only
  wire format Codex >= 0.148 still speaks. In this dialect the tool to call is
  picked adaptively from the "tools" array of the first request unless --tool
  is given explicitly, and the command embedded in the tool arguments comes
  from --command.
"""
import argparse
import json
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

STATE = {"tool_result": None, "calls": 0, "done": threading.Event()}
DEBUG_PATH = ""

# Shell-tool argument templates for the responses dialect, in preference order.
# Codex names its shell tool differently depending on version and features.
RESP_TOOL_ARGS = {
    "exec_command": lambda cmd: {"cmd": cmd, "yield_time_ms": 1000, "max_output_tokens": 4000},
    "shell_command": lambda cmd: {"command": cmd},
    "shell": lambda cmd: {"command": cmd},
    "bash": lambda cmd: {"command": cmd},
}


def chunk(payload):
    return f"data: {json.dumps(payload)}\n\n".encode()


def base(delta, finish=None):
    return {
        "id": "chatcmpl-reach",
        "object": "chat.completion.chunk",
        "created": int(time.time()),
        "model": "reach-mock",
        "choices": [{"index": 0, "delta": delta, "finish_reason": finish}],
    }


def sse(event_type, payload):
    return f"event: {event_type}\ndata: {json.dumps(payload)}\n\n".encode()


def resp_created():
    return sse("response.created", {
        "type": "response.created",
        "response": {"id": "resp_reach_1"},
    })


def resp_completed():
    return sse("response.completed", {
        "type": "response.completed",
        "response": {
            "id": "resp_reach_1",
            "usage": {
                "input_tokens": 0,
                "input_tokens_details": None,
                "output_tokens": 0,
                "output_tokens_details": None,
                "total_tokens": 0,
            },
        },
    })


def pick_responses_tool(tools, command):
    """Choose which function tool to call and the arguments to pass it.

    Codex names its shell tool differently across versions and feature flags;
    pick the first known shell tool present, else fall back to the first
    function tool with a generic {"command": ...} argument.
    """
    names = [t.get("name") for t in tools if isinstance(t, dict) and t.get("type") == "function"]
    if ARGS.tool is not None:
        return ARGS.tool, ARGS.args
    for name, make_args in RESP_TOOL_ARGS.items():
        if name in names:
            return name, json.dumps(make_args(command))
    if names:
        print(f"mockmodel: no known shell tool in {names}; using {names[0]} "
              f"with generic {{'command': ...}} args", file=sys.stderr)
        return names[0], json.dumps({"command": command})
    print("mockmodel: request carried no function tools; falling back to 'shell'",
          file=sys.stderr)
    return "shell", json.dumps({"command": command})


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *a):  # keep test output readable
        pass

    def do_GET(self):
        if self.path.rstrip("/").endswith("/models"):
            body = json.dumps({
                "object": "list",
                "data": [{"id": "reach-mock", "object": "model", "owned_by": "reach"}],
            }).encode()
            self._send_json(body)
            return
        self.send_error(404)

    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        req = json.loads(self.rfile.read(length) or b"{}")
        if ARGS.dialect == "responses" and self.path.rstrip("/").endswith("/responses"):
            self._do_responses(req)
            return
        self._do_chat(req)

    def _do_chat(self, req):
        messages = req.get("messages", [])
        if DEBUG_PATH:
            with open(DEBUG_PATH, "a") as fh:
                fh.write(json.dumps(messages) + "\n")

        # Capture whatever the harness reported back from running the tool. That
        # payload is the actual evidence: it is what the tool produced, seen
        # from inside the harness.
        for m in messages:
            if m.get("role") == "tool":
                content = m.get("content")
                if isinstance(content, list):
                    content = "".join(
                        p.get("text", "") for p in content if isinstance(p, dict)
                    )
                if content:
                    STATE["tool_result"] = content

        STATE["calls"] += 1
        first_turn = STATE["tool_result"] is None and STATE["calls"] == 1

        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.send_header("Connection", "keep-alive")
        self.end_headers()

        if first_turn:
            self.wfile.write(chunk(base({"role": "assistant", "content": ""})))
            self.wfile.write(chunk(base({"tool_calls": [{
                "index": 0, "id": "call_reach_1", "type": "function",
                "function": {"name": ARGS.tool, "arguments": ""},
            }]})))
            self.wfile.write(chunk(base({"tool_calls": [{
                "index": 0, "function": {"arguments": ARGS.args},
            }]})))
            self.wfile.write(chunk(base({}, finish="tool_calls")))
        else:
            self.wfile.write(chunk(base({"role": "assistant", "content": "OBSERVED: "})))
            text = (STATE["tool_result"] or "")[:4000]
            self.wfile.write(chunk(base({"content": text})))
            self.wfile.write(chunk(base({}, finish="stop")))
            STATE["done"].set()

        self.wfile.write(b"data: [DONE]\n\n")
        self.wfile.flush()

    def _do_responses(self, req):
        if DEBUG_PATH:
            with open(DEBUG_PATH, "a") as fh:
                fh.write(json.dumps(req) + "\n")

        # The second request carries the tool's output back as a
        # function_call_output item. Its output may be a plain string or a JSON
        # string wrapping the real output; store it raw, either way.
        for item in req.get("input") or []:
            if isinstance(item, dict) and item.get("type") == "function_call_output":
                out = item.get("output")
                if out is not None and not isinstance(out, str):
                    out = json.dumps(out)
                if out:
                    STATE["tool_result"] = out

        STATE["calls"] += 1
        first_turn = STATE["tool_result"] is None

        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.send_header("Connection", "keep-alive")
        self.end_headers()

        self.wfile.write(resp_created())
        if first_turn:
            name, args = pick_responses_tool(req.get("tools") or [], ARGS.command)
            self.wfile.write(sse("response.output_item.done", {
                "type": "response.output_item.done",
                "item": {
                    "type": "function_call",
                    "call_id": "call_reach_1",
                    "name": name,
                    "arguments": args,
                },
            }))
        else:
            text = (STATE["tool_result"] or "")[:4000]
            self.wfile.write(sse("response.output_item.done", {
                "type": "response.output_item.done",
                "item": {
                    "type": "message",
                    "role": "assistant",
                    "content": [{
                        "type": "output_text",
                        "text": f"OBSERVED: {text}",
                    }],
                },
            }))
        self.wfile.write(resp_completed())
        if not first_turn:
            STATE["done"].set()
        self.wfile.flush()

    def _send_json(self, body):
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


def main():
    global ARGS
    ap = argparse.ArgumentParser()
    ap.add_argument("--port", type=int, default=8899)
    ap.add_argument("--dialect", choices=["chat", "responses"], default="chat")
    ap.add_argument("--tool", help="tool name (required for chat; overrides auto-selection in responses)")
    ap.add_argument("--args", help="JSON arguments for the tool call (required for chat)")
    ap.add_argument("--command", default="echo REACH_MOCK_MARKER; hostname",
                    help="command embedded in the tool call (responses dialect)")
    ap.add_argument("--timeout", type=float, default=120.0)
    ap.add_argument("--debug-dump", default="", help="append each  request's messages to this file")
    ARGS = ap.parse_args()
    global DEBUG_PATH
    DEBUG_PATH = ARGS.debug_dump
    if ARGS.dialect == "chat" and (ARGS.tool is None or ARGS.args is None):
        ap.error("--tool and --args are required for the chat dialect")

    # Threading matters: harnesses issue concurrent requests (a title-
    # generation agent alongside the main one), and a single-threaded server
    # deadlocks the harness instead of answering it.
    srv = ThreadingHTTPServer(("127.0.0.1", ARGS.port), Handler)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    STATE["done"].wait(timeout=ARGS.timeout)
    srv.shutdown()

    json.dump({
        "tool_called": STATE["calls"] > 1,
        "tool_result": STATE["tool_result"],
    }, sys.stdout)
    sys.stdout.write("\n")


if __name__ == "__main__":
    main()
