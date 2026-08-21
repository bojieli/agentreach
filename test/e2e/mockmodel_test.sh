#!/usr/bin/env bash
# Verifies the mock model server itself, independent of any harness.
#
# The mock exists so agent-level tests can run without an API key. If the mock
# is wrong, a harness test built on it fails for reasons that have nothing to do
# with reach — so the mock gets its own tests.
set -uo pipefail
cd "$(dirname "$0")" && source ./lib.sh

PORT="${REACH_MOCK_PORT:-8917}"
SERVER="$(cd .. && pwd)/mockmodel/server.py"

info "Mock model server protocol"
out_file="$(mktemp)"
python3 "$SERVER" --port "$PORT" --tool read --args '{"filePath":"/x"}' --timeout 12 > "$out_file" 2>&1 &
mock_pid=$!
sleep 1.2

models="$(curl -s --max-time 5 "http://127.0.0.1:$PORT/v1/models")"
assert_contains "$models" '"object": "list"' "GET /v1/models returns a model list"
assert_contains "$models" 'reach-mock' "the mock model is advertised"

stream="$(curl -s --max-time 8 -X POST "http://127.0.0.1:$PORT/v1/chat/completions" \
  -H 'Content-Type: application/json' \
  -d '{"model":"reach-mock","stream":true,"messages":[{"role":"user","content":"hi"}]}')"
assert_contains "$stream" 'chat.completion.chunk' "streams chat completion chunks"
assert_contains "$stream" '"tool_calls"' "emits a tool call"
assert_contains "$stream" '"name": "read"' "names the requested tool"
assert_contains "$stream" '"finish_reason": "tool_calls"' "terminates the turn with tool_calls"
assert_contains "$stream" 'data: [DONE]' "closes the stream"

# A second turn carrying a tool result must be echoed back, which is how tests
# observe what the harness actually got from the tool.
second="$(curl -s --max-time 8 -X POST "http://127.0.0.1:$PORT/v1/chat/completions" \
  -H 'Content-Type: application/json' \
  -d '{"model":"reach-mock","stream":true,"messages":[
        {"role":"user","content":"hi"},
        {"role":"assistant","tool_calls":[{"id":"call_reach_1","type":"function","function":{"name":"read","arguments":"{}"}}]},
        {"role":"tool","tool_call_id":"call_reach_1","content":"REMOTE_CONTENT_MARKER"}]}')"
assert_contains "$second" 'REMOTE_CONTENT_MARKER' "reflects the tool result back for assertion"

wait "$mock_pid" 2>/dev/null
result="$(cat "$out_file")"
assert_contains "$result" 'REMOTE_CONTENT_MARKER' "reports the observed tool result on exit"
rm -f "$out_file"

summary
