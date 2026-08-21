package harnessprobe

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// These tests pin the mock's wire shape, which is a contract with the real
// codex: the SSE event sequence here is the minimal one established against
// the real binary by test/e2e/seam_test.sh, and drifting from it makes
// the probe measure its own server rather than the seam.

// postResponses issues one handcrafted Responses-API call and returns the raw
// SSE body.
func postResponses(t *testing.T, m *Mock, body string) string {
	t.Helper()
	resp, err := http.Post(m.BaseURL()+"/responses", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /responses: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /responses: status %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(data)
}

func TestMockScriptsOneToolCallAndRecordsItsOutput(t *testing.T) {
	m := StartMock("REACH_TEST_MARKER", DialectResponses)
	defer m.Close()

	// Turn one: the harness opens a turn, advertising its tools. The mock must
	// answer with the scripted function call, naming exec_command — first in
	// preference order — with the canary embedded in the arguments.
	turn1 := postResponses(t, m, `{
		"input": [{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "go"}]}],
		"tools": [
			{"type": "function", "name": "shell_command"},
			{"type": "function", "name": "exec_command"}
		]
	}`)
	for _, want := range []string{
		"event: response.created\n",
		"event: response.output_item.done\n",
		`"type":"function_call"`,
		`"call_id":"call_reach_1"`,
		`"name":"exec_command"`,
		"echo REACH_TEST_MARKER; hostname",
		"event: response.completed\n",
		`"total_tokens":0`,
	} {
		if !strings.Contains(turn1, want) {
			t.Errorf("turn-1 stream is missing %q:\n%s", want, turn1)
		}
	}
	// Arguments must be a JSON *string* holding an object, per the Responses
	// function_call shape — not an inline object.
	var ev struct {
		Item struct {
			Arguments string `json:"arguments"`
		} `json:"item"`
	}
	for _, line := range strings.Split(turn1, "\n") {
		if !strings.HasPrefix(line, "data: ") || !strings.Contains(line, "output_item.done") {
			continue
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			t.Fatalf("output_item.done data is not JSON: %v", err)
		}
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(ev.Item.Arguments), &args); err != nil {
		t.Fatalf("function_call arguments are not a JSON string holding an object: %v", err)
	}
	if args["cmd"] != "echo REACH_TEST_MARKER; hostname" {
		t.Fatalf("arguments cmd = %v, want the scripted command", args["cmd"])
	}

	if _, observed := m.Result(); observed {
		t.Fatal("no tool output should be recorded after turn one")
	}

	// Turn two: the harness reports the tool's output back as a
	// function_call_output item. The mock must store it verbatim and close the
	// turn with a plain assistant message.
	turn2 := postResponses(t, m, `{
		"input": [{"type": "function_call_output", "call_id": "call_reach_1",
			"output": "REACH_TEST_MARKER\nremote-box"}]
	}`)
	for _, want := range []string{
		`"type":"message"`,
		`"role":"assistant"`,
		`"type":"output_text"`,
		"OBSERVED: REACH_TEST_MARKER",
		"event: response.completed\n",
	} {
		if !strings.Contains(turn2, want) {
			t.Errorf("turn-2 stream is missing %q:\n%s", want, turn2)
		}
	}

	out, observed := m.Result()
	if !observed {
		t.Fatal("the tool output was not recorded")
	}
	if out != "REACH_TEST_MARKER\nremote-box" {
		t.Fatalf("recorded output = %q", out)
	}
}

func TestMockWaitUnblocksOnResult(t *testing.T) {
	m := StartMock("M", DialectResponses)
	defer m.Close()
	ctx := make(chan struct{})
	finished := make(chan struct{})
	go func() { m.Wait(ctx); close(finished) }()

	postResponses(t, m, `{"input": [], "tools": [{"type":"function","name":"shell"}]}`)
	postResponses(t, m, `{"input": [{"type":"function_call_output","output":"M\nbox"}]}`)

	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return after the tool output arrived")
	}
}

func TestMockAdaptiveToolSelection(t *testing.T) {
	cases := []struct {
		name     string
		tools    string
		wantName string
	}{
		{"exec_command preferred", `[{"type":"function","name":"exec_command"},{"type":"function","name":"shell"}]`, "exec_command"},
		{"shell_command over shell", `[{"type":"function","name":"shell"},{"type":"function","name":"shell_command"}]`, "shell_command"},
		{"plain shell", `[{"type":"function","name":"shell"}]`, "shell"},
		{"unknown falls back to first function tool", `[{"type":"function","name":"weird_shell"}]`, "weird_shell"},
		{"no tools falls back to shell", `[]`, "shell"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := StartMock("M", DialectResponses)
			defer m.Close()
			body := postResponses(t, m, `{"input": [], "tools": `+tc.tools+`}`)
			if !strings.Contains(body, `"name":"`+tc.wantName+`"`) {
				t.Fatalf("called the wrong tool, want %q:\n%s", tc.wantName, body)
			}
		})
	}
}

func TestMockToolOutputAsJSONValue(t *testing.T) {
	// Codex may report the tool output as a JSON value wrapping the real
	// string rather than as a plain string; the evidence must survive either
	// encoding.
	m := StartMock("M", DialectResponses)
	defer m.Close()
	postResponses(t, m, `{"input": [], "tools": [{"type":"function","name":"shell"}]}`)
	postResponses(t, m, `{"input": [{"type":"function_call_output","output":{"content":"M\nbox"}}]}`)
	out, observed := m.Result()
	if !observed || !strings.Contains(out, "box") {
		t.Fatalf("structured tool output was not preserved: %q observed=%v", out, observed)
	}
}

func TestMockRejectsUnexpectedRequests(t *testing.T) {
	m := StartMock("M", DialectResponses)
	defer m.Close()
	resp, err := http.Post(m.BaseURL()+"/chat/completions", "application/json", bytes.NewReader(nil))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for a dialect the mock does not speak", resp.StatusCode)
	}
}

// postChat issues one handcrafted chat-completions call and returns the raw
// SSE body.
func postChat(t *testing.T, m *Mock, body string) string {
	t.Helper()
	resp, err := http.Post(m.BaseURL()+"/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /chat/completions: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /chat/completions: status %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(data)
}

// chatChunks splits an SSE body into the JSON payload of each data: line,
// skipping the [DONE] sentinel.
func chatChunks(t *testing.T, body string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			continue
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			t.Fatalf("chunk data is not JSON: %v\n%s", err, data)
		}
		out = append(out, chunk)
	}
	return out
}

func TestMockChatDialectScriptsOneToolCall(t *testing.T) {
	m := StartMock("REACH_CHAT_MARKER", DialectChat)
	defer m.Close()

	// Turn one: kimi advertises its tools in the chat-completions shape —
	// nested under "function" — and the mock must prefer "Bash".
	turn1 := postChat(t, m, `{
		"messages": [{"role": "user", "content": "go"}],
		"tools": [
			{"type": "function", "function": {"name": "ReadFile"}},
			{"type": "function", "function": {"name": "Bash"}}
		]
	}`)
	if !strings.HasSuffix(strings.TrimSpace(turn1), "data: [DONE]") {
		t.Errorf("chat stream must end with the [DONE] sentinel:\n%s", turn1)
	}
	chunks := chatChunks(t, turn1)
	if len(chunks) != 4 {
		t.Fatalf("turn-1 stream has %d chunks, want 4 (role, tool name, arguments, finish):\n%s",
			len(chunks), turn1)
	}
	// Every chunk carries the fixed envelope the reference mock established.
	for i, c := range chunks {
		if c["object"] != "chat.completion.chunk" || c["id"] != "chatcmpl-reach" {
			t.Errorf("chunk %d has the wrong envelope: %v", i, c)
		}
	}
	delta := func(i int) map[string]any {
		choices, _ := chunks[i]["choices"].([]any)
		c0, _ := choices[0].(map[string]any)
		d, _ := c0["delta"].(map[string]any)
		return d
	}
	finish := func(i int) any {
		choices, _ := chunks[i]["choices"].([]any)
		c0, _ := choices[0].(map[string]any)
		return c0["finish_reason"]
	}
	if delta(0)["role"] != "assistant" {
		t.Errorf("chunk 0 should open the assistant turn: %v", delta(0))
	}
	// Name and arguments arrive in separate chunks, as real OpenAI streams
	// deliver them; the name chunk's arguments are empty.
	tcs, _ := delta(1)["tool_calls"].([]any)
	tc, _ := tcs[0].(map[string]any)
	fn, _ := tc["function"].(map[string]any)
	if tc["id"] != "call_reach_1" || fn["name"] != "Bash" || fn["arguments"] != "" {
		t.Errorf("chunk 1 should name Bash with empty arguments: %v", delta(1))
	}
	tcs2, _ := delta(2)["tool_calls"].([]any)
	tc2, _ := tcs2[0].(map[string]any)
	fn2, _ := tc2["function"].(map[string]any)
	args, _ := fn2["arguments"].(string)
	var parsed map[string]any
	if err := json.Unmarshal([]byte(args), &parsed); err != nil {
		t.Fatalf("chunk 2 arguments are not a JSON string holding an object: %v", err)
	}
	if parsed["command"] != "echo REACH_CHAT_MARKER; hostname" {
		t.Errorf("arguments command = %v, want the scripted command", parsed["command"])
	}
	if finish(2) != nil || finish(3) != "tool_calls" {
		t.Errorf("finish_reason sequence = %v then %v, want null then tool_calls", finish(2), finish(3))
	}

	if _, observed := m.Result(); observed {
		t.Fatal("no tool output should be recorded after turn one")
	}

	// Turn two: kimi reports the tool result as a role:"tool" message.
	turn2 := postChat(t, m, `{
		"messages": [
			{"role": "user", "content": "go"},
			{"role": "tool", "tool_call_id": "call_reach_1", "content": "REACH_CHAT_MARKER\nremote-box"}
		]
	}`)
	chunks2 := chatChunks(t, turn2)
	// "OBSERVED: " and the tool output stream as separate content chunks, as
	// the reference mock emits them.
	if !strings.Contains(turn2, `"OBSERVED: "`) || !strings.Contains(turn2, `REACH_CHAT_MARKER\nremote-box`) {
		t.Errorf("turn-2 stream should echo the observed output:\n%s", turn2)
	}
	if got := finishOfLast(chunks2); got != "stop" {
		t.Errorf("turn-2 final finish_reason = %v, want stop", got)
	}
	if !strings.HasSuffix(strings.TrimSpace(turn2), "data: [DONE]") {
		t.Errorf("turn-2 stream must end with [DONE]:\n%s", turn2)
	}

	out, observed := m.Result()
	if !observed || out != "REACH_CHAT_MARKER\nremote-box" {
		t.Fatalf("recorded output = %q observed=%v", out, observed)
	}
}

func finishOfLast(chunks []map[string]any) any {
	choices, _ := chunks[len(chunks)-1]["choices"].([]any)
	c0, _ := choices[0].(map[string]any)
	return c0["finish_reason"]
}

func TestMockChatToolResultAsParts(t *testing.T) {
	// A tool message's content may be a list of text parts rather than a
	// plain string; the evidence must survive either encoding.
	m := StartMock("M", DialectChat)
	defer m.Close()
	postChat(t, m, `{"messages": [{"role":"user","content":"go"}],
		"tools": [{"type":"function","function":{"name":"Bash"}}]}`)
	postChat(t, m, `{"messages": [{"role":"tool","content":[{"type":"text","text":"M\nbox"}]}]}`)
	out, observed := m.Result()
	if !observed || out != "M\nbox" {
		t.Fatalf("part-list tool content was not preserved: %q observed=%v", out, observed)
	}
}

func TestMockChatDialectRejectsResponsesEndpoint(t *testing.T) {
	m := StartMock("M", DialectChat)
	defer m.Close()
	resp, err := http.Post(m.BaseURL()+"/responses", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: a chat mock must not answer the responses endpoint", resp.StatusCode)
	}
}

// postGemini issues one Gemini streamGenerateContent call and returns the raw
// SSE body. The @google/genai SDK appends /v1beta/models/{model}:streamGenerateContent
// to the base URL; the mock's BaseURL() for DialectGemini is the raw server URL,
// so we form the path as the SDK would.
func postGemini(t *testing.T, m *Mock, model, body string) string {
	t.Helper()
	url := m.BaseURL() + "/v1beta/models/" + model + ":streamGenerateContent"
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST %s: status %d; body: %s", url, resp.StatusCode, data)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(data)
}

// geminiChunks splits an SSE body into parsed JSON objects.
func geminiChunks(t *testing.T, body string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk); err != nil {
			t.Fatalf("chunk data is not JSON: %v\n%s", err, line)
		}
		out = append(out, chunk)
	}
	return out
}

func TestMockGeminiDialectScriptsOneToolCall(t *testing.T) {
	m := StartMock("REACH_GEMINI_MARKER", DialectGemini)
	defer m.Close()

	// The Gemini mock's BaseURL() should be the raw server URL (no /v1 suffix),
	// because the @google/genai SDK appends the version path itself.
	if strings.HasSuffix(m.BaseURL(), "/v1") {
		t.Fatalf("Gemini mock BaseURL() = %q: should NOT have /v1 suffix", m.BaseURL())
	}

	// Turn one: Gemini CLI sends a request with functionDeclarations.
	turn1 := postGemini(t, m, "reach-mock", `{
		"contents": [{"role":"user","parts":[{"text":"go"}]}],
		"tools": [{
			"functionDeclarations": [
				{"name": "read_file", "description": "read a file"},
				{"name": "run_shell_command", "description": "run a shell command",
				 "parameters": {"type": "object", "properties": {"command": {"type": "string"}}}}
			]
		}]
	}`)
	chunks1 := geminiChunks(t, turn1)
	if len(chunks1) == 0 {
		t.Fatalf("turn-1 stream has no chunks:\n%s", turn1)
	}
	// Extract the functionCall from the first chunk.
	candidates, _ := chunks1[0]["candidates"].([]any)
	if len(candidates) == 0 {
		t.Fatalf("turn-1 chunk has no candidates: %v", chunks1[0])
	}
	c0, _ := candidates[0].(map[string]any)
	content, _ := c0["content"].(map[string]any)
	if content["role"] != "model" {
		t.Fatalf("content role = %v, want model", content["role"])
	}
	parts, _ := content["parts"].([]any)
	if len(parts) == 0 {
		t.Fatalf("content has no parts: %v", content)
	}
	part0, _ := parts[0].(map[string]any)
	fc, _ := part0["functionCall"].(map[string]any)
	if fc == nil {
		t.Fatalf("first part has no functionCall: %v", part0)
	}
	if fc["name"] != "run_shell_command" {
		t.Fatalf("functionCall name = %v, want run_shell_command", fc["name"])
	}
	fcArgs, _ := fc["args"].(map[string]any)
	if !strings.Contains(fmt.Sprintf("%v", fcArgs["command"]), "REACH_GEMINI_MARKER") {
		t.Fatalf("functionCall args.command = %v, want it to contain the marker", fcArgs["command"])
	}

	if _, observed := m.Result(); observed {
		t.Fatal("no tool output should be recorded after turn one")
	}

	// Turn two: Gemini CLI sends the tool result as functionResponse.
	turn2 := postGemini(t, m, "reach-mock", `{
		"contents": [
			{"role":"user","parts":[{"text":"go"}]},
			{"role":"model","parts":[{"functionCall":{"name":"run_shell_command","args":{"command":"echo REACH_GEMINI_MARKER; hostname"}}}]},
			{"role":"user","parts":[{"functionResponse":{"id":"call_1","name":"run_shell_command","response":{"output":"REACH_GEMINI_MARKER\nremote-box"}}}]}
		]
	}`)
	chunks2 := geminiChunks(t, turn2)
	if len(chunks2) == 0 {
		t.Fatalf("turn-2 stream has no chunks:\n%s", turn2)
	}
	if !strings.Contains(turn2, "OBSERVED:") || !strings.Contains(turn2, "REACH_GEMINI_MARKER") {
		t.Errorf("turn-2 stream should echo the observed output:\n%s", turn2)
	}

	out, observed := m.Result()
	if !observed {
		t.Fatal("the tool output was not recorded")
	}
	if out != "REACH_GEMINI_MARKER\nremote-box" {
		t.Fatalf("recorded output = %q", out)
	}
}

func TestMockGeminiAdaptiveToolSelection(t *testing.T) {
	cases := []struct {
		name     string
		decls    string
		wantName string
	}{
		{"run_shell_command preferred", `[{"name":"read_file"},{"name":"run_shell_command"}]`, "run_shell_command"},
		{"shell fallback", `[{"name":"read_file"},{"name":"shell"}]`, "shell"},
		{"first function if none known", `[{"name":"weird_tool"}]`, "weird_tool"},
		{"no tools falls back to run_shell_command", `[]`, "run_shell_command"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := StartMock("M", DialectGemini)
			defer m.Close()
			body := postGemini(t, m, "reach-mock", `{"contents":[{"role":"user","parts":[{"text":"go"}]}],"tools":[{"functionDeclarations":`+tc.decls+`}]}`)
			if !strings.Contains(body, `"name":"`+tc.wantName+`"`) {
				t.Fatalf("called the wrong tool, want %q:\n%s", tc.wantName, body)
			}
		})
	}
}

func TestMockGeminiDialectRejectsChatEndpoint(t *testing.T) {
	m := StartMock("M", DialectGemini)
	defer m.Close()
	resp, err := http.Post(m.BaseURL()+"/v1/chat/completions", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: a Gemini mock must not answer the chat endpoint", resp.StatusCode)
	}
}

// postAnthropic issues one Anthropic Messages API call and returns the raw SSE
// body and HTTP status code.
func postAnthropic(t *testing.T, url string, body any) (string, int) {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	resp, err := http.Post(url, "application/json", strings.NewReader(string(data)))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	return string(raw), resp.StatusCode
}

func TestMockAnthropicDialectScriptsOneToolCall(t *testing.T) {
	mock := StartMock("PROBE_MARKER", DialectAnthropic)
	defer mock.Close()

	// Turn 1: first request should produce a tool_use response for Bash.
	body1, status1 := postAnthropic(t, mock.BaseURL()+"/v1/messages", map[string]any{
		"model":      "claude-reach",
		"max_tokens": 1024,
		"messages":   []map[string]any{{"role": "user", "content": "run a command"}},
		"tools":      []map[string]any{{"name": "Bash", "description": "run bash", "input_schema": map[string]any{}}},
	})
	if status1 != 200 {
		t.Fatalf("turn 1: expected 200, got %d; body: %s", status1, body1)
	}
	if !strings.Contains(body1, "tool_use") {
		t.Fatalf("turn 1: expected tool_use in response, got: %s", body1)
	}
	if !strings.Contains(body1, "PROBE_MARKER") {
		t.Fatalf("turn 1: expected marker in tool arguments, got: %s", body1)
	}
	if !strings.Contains(body1, "Bash") {
		t.Fatalf("turn 1: expected Bash tool name, got: %s", body1)
	}
	if _, observed := mock.Result(); observed {
		t.Fatal("no tool output should be recorded after turn one")
	}

	// Turn 2: send tool_result, expect a text response echoing the output.
	body2, status2 := postAnthropic(t, mock.BaseURL()+"/v1/messages", map[string]any{
		"model":      "claude-reach",
		"max_tokens": 1024,
		"messages": []map[string]any{
			{"role": "user", "content": "run a command"},
			{"role": "assistant", "content": []map[string]any{
				{"type": "tool_use", "id": "toolu_reach_1", "name": "Bash", "input": map[string]any{"command": "echo PROBE_MARKER; hostname"}},
			}},
			{"role": "user", "content": []map[string]any{
				{"type": "tool_result", "tool_use_id": "toolu_reach_1", "content": "PROBE_MARKER\nremote-host\n"},
			}},
		},
		"tools": []map[string]any{{"name": "Bash", "description": "run bash", "input_schema": map[string]any{}}},
	})
	if status2 != 200 {
		t.Fatalf("turn 2: expected 200, got %d; body: %s", status2, body2)
	}
	if !strings.Contains(body2, "OBSERVED") {
		t.Fatalf("turn 2: expected OBSERVED in response, got: %s", body2)
	}

	out, observed := mock.Result()
	if !observed {
		t.Fatal("mock.Result(): expected observed=true after turn 2")
	}
	if !strings.Contains(out, "PROBE_MARKER") {
		t.Fatalf("mock.Result(): expected marker in output, got: %q", out)
	}
}

func TestMockAnthropicDialectRejectsChatEndpoint(t *testing.T) {
	mock := StartMock("MARKER", DialectAnthropic)
	defer mock.Close()
	_, status := postAnthropic(t, mock.BaseURL()+"/v1/chat/completions", map[string]any{
		"model": "reach-mock",
	})
	if status != 404 {
		t.Errorf("anthropic mock should 404 the chat endpoint, got %d", status)
	}
}
