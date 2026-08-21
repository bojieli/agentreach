import json, sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
out = sys.argv[2]
done = {"wrote": False}
class H(BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def do_POST(self):
        req = json.loads(self.rfile.read(int(self.headers.get("Content-Length", 0)) or b"{}"))
        if not done["wrote"] and req.get("tools"):
            json.dump(req, open(out, "w"))
            done["wrote"] = True
        body = json.dumps({"id":"c","object":"chat.completion","created":0,"model":"m",
            "choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
            "usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}).encode()
        self.send_response(200); self.send_header("Content-Type","application/json")
        self.send_header("Content-Length",str(len(body))); self.end_headers()
        self.wfile.write(body)
ThreadingHTTPServer(("127.0.0.1", int(sys.argv[1])), H).serve_forever()
