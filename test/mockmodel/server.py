#!/usr/bin/env python3
"""A minimal OpenAI-compatible model server that drives a scripted tool call.

waldo's hardest claim to verify is that a harness's *own* tools end up acting on
the target. Proving that normally needs a real model, which means an API key,
which means the test cannot run in CI and cannot run for a contributor who has
no account.

This server removes the model from the loop while keeping the harness in it. It
speaks just enough of the OpenAI chat-completions streaming protocol to tell a
harness "call this tool with these arguments", then reports what came back. Any
harness that can point at an OpenAI-compatible base URL can be tested this way.

Usage:
    server.py --port 8899 --tool read --args '{"filePath": "/srv/app/README.md"}'

It exits after the conversation completes, printing the tool result it observed
on stdout as JSON, so a test can assert on it.
"""
import argparse
import json
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

STATE = {"tool_result": None, "calls": 0, "done": threading.Event()}
DEBUG_PATH = ""


def chunk(payload):
    return f"data: {json.dumps(payload)}\n\n".encode()


def base(delta, finish=None):
    return {
        "id": "chatcmpl-waldo",
        "object": "chat.completion.chunk",
        "created": int(time.time()),
        "model": "waldo-mock",
        "choices": [{"index": 0, "delta": delta, "finish_reason": finish}],
    }


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *a):  # keep test output readable
        pass

    def do_GET(self):
        if self.path.rstrip("/").endswith("/models"):
            body = json.dumps({
                "object": "list",
                "data": [{"id": "waldo-mock", "object": "model", "owned_by": "waldo"}],
            }).encode()
            self._send_json(body)
            return
        self.send_error(404)

    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        req = json.loads(self.rfile.read(length) or b"{}")
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
                "index": 0, "id": "call_waldo_1", "type": "function",
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
    ap.add_argument("--tool", required=True)
    ap.add_argument("--args", required=True, help="JSON arguments for the tool call")
    ap.add_argument("--timeout", type=float, default=120.0)
    ap.add_argument("--debug-dump", default="", help="append each  request's messages to this file")
    ARGS = ap.parse_args()
    global DEBUG_PATH
    DEBUG_PATH = ARGS.debug_dump

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
