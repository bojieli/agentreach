package harnessprobe

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"
)

// Dialect selects which wire protocol the mock speaks.
//
// A mock speaks exactly one dialect, and 404s the other's endpoint: a harness
// pointed at the wrong one should fail loudly rather than get a plausible
// answer from a protocol it never asked for.
type Dialect string

const (
	// DialectResponses is the Responses API, the only wire format
	// Codex >= 0.148 still speaks.
	DialectResponses Dialect = "responses"
	// DialectChat is the chat-completions streaming protocol, which Kimi Code
	// speaks when KIMI_MODEL_PROVIDER_TYPE=openai.
	DialectChat Dialect = "chat"
	// DialectGemini is Google's Gemini API format
	// (POST /v1beta/models/{model}:streamGenerateContent).
	DialectGemini Dialect = "gemini"
)

// Mock is a minimal OpenAI model server that scripts exactly one tool call
// and records what the harness reports back.
//
// It exists because waldo's hardest claim — "the harness's own tools act on
// the target" — cannot be checked by reading version strings; it has to be
// observed. Checking it against a real model would need an API key, which
// means the check could never run in CI, in `waldo doctor`, or on a machine
// whose operator has no account. A scripted server keeps the harness — the
// part whose behaviour is actually in question — fully in the loop while
// removing the model, the network and the bill.
//
// The script has two turns. Turn one answers the harness's first request by
// instructing the canary command; turn two arrives when the harness POSTs the
// tool's output back, which the mock stores and replies to with a plain
// assistant message so the harness finishes its turn and exits. The wire
// shapes — response.created / response.output_item.done / response.completed
// with an all-zero usage block for the responses dialect, and the chunked
// tool_calls delta sequence ending in "data: [DONE]" for the chat dialect —
// are the minimal sets the real harnesses accept, established by the offline
// e2e probes in test/e2e against the real binaries.
//
// Harnesses name their shell tool differently across versions and feature
// flags (codex: "exec_command", "shell_command", "shell"; kimi: "Bash"), so
// the tool to call is picked adaptively from the tools array of the first
// request rather than assumed.
type Mock struct {
	srv     *httptest.Server
	marker  string
	dialect Dialect
	// command, when set, overrides the default "echo <marker>; hostname"
	// probe command. SetCommand sets it after construction.
	command string

	mu         sync.Mutex
	toolResult string
	hasResult  bool
	done       chan struct{}
	doneOnce   sync.Once
}

// toolSpec is the slice of a tool advertisement the mock reads.
type toolSpec struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

// responsesRequest is the slice of a Responses API request the mock reads.
// Everything else the harness sends is ignored by design: the narrower the
// dialect the mock speaks, the clearer it is which parts of the protocol the
// harness actually depends on.
type responsesRequest struct {
	Input []struct {
		Type   string          `json:"type"`
		Output json.RawMessage `json:"output"`
	} `json:"input"`
	Tools []toolSpec `json:"tools"`
}

// chatRequest is the slice of a chat-completions request the mock reads. The
// tool result arrives as a message with role "tool" whose content is either a
// plain string or a list of text parts.
type chatRequest struct {
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
	Tools []struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"tools"`
}

// StartMock starts a mock model server on a random localhost port, speaking
// the given dialect. The marker is the canary embedded in the scripted
// command; it must be unique per run so that seeing it in the tool output
// proves this probe's command ran, not some leftover output the harness
// happened to carry.
func StartMock(marker string, dialect Dialect) *Mock {
	m := &Mock{marker: marker, dialect: dialect, done: make(chan struct{})}
	m.srv = httptest.NewServer(m)
	return m
}

// BaseURL is the provider base_url the harness should be pointed at.
// For OpenAI-dialect mocks it includes the /v1 suffix Codex/Kimi/Goose expect.
// For the Gemini dialect it is the raw server URL; the SDK appends its own
// version path (/v1beta/models/{model}:streamGenerateContent).
func (m *Mock) BaseURL() string {
	if m.dialect == DialectGemini {
		return m.srv.URL
	}
	return m.srv.URL + "/v1"
}

// Close shuts the server down.
func (m *Mock) Close() { m.srv.Close() }

// SetCommand overrides the probe command the mock scripts in the first turn.
// The default is "echo <marker>; hostname". Call SetCommand before the first
// request arrives; changing it after the first request is a data race.
func (m *Mock) SetCommand(cmd string) { m.command = cmd }

// Result returns the tool output the harness reported, and whether any tool
// output was observed at all.
func (m *Mock) Result() (output string, observed bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.toolResult, m.hasResult
}

// Wait blocks until the harness has reported a tool output or ctx expires.
func (m *Mock) Wait(ctxDone <-chan struct{}) {
	select {
	case <-m.done:
	case <-ctxDone:
	}
}

func (m *Mock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimSuffix(r.URL.Path, "/")
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/models"):
		// Some harnesses enumerate models before starting a turn. Answering is
		// cheap; being asked is harmless.
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"object":"list","data":[{"id":"waldo-mock","object":"model","owned_by":"waldo"}]}`)
	case r.Method == http.MethodPost && m.dialect == DialectResponses && strings.HasSuffix(path, "/responses"):
		m.serveResponses(w, r)
	case r.Method == http.MethodPost && m.dialect == DialectChat && strings.HasSuffix(path, "/chat/completions"):
		m.serveChat(w, r)
	case r.Method == http.MethodPost && m.dialect == DialectGemini &&
		(strings.HasSuffix(path, ":streamGenerateContent") || strings.HasSuffix(path, ":generateContent")):
		m.serveGemini(w, r)
	default:
		http.Error(w, "waldo harnessprobe mock: unexpected request", http.StatusNotFound)
	}
}

// record stores the tool output the harness reported. That payload is the
// evidence: it is what the tool produced, seen from inside the harness.
func (m *Mock) record(out string) {
	if out == "" {
		return
	}
	m.mu.Lock()
	m.toolResult, m.hasResult = out, true
	m.mu.Unlock()
}

// startStream writes the shared SSE response headers and returns a flusher.
func startStream(w http.ResponseWriter) http.Flusher {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	return flusher
}

func (m *Mock) serveResponses(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var req responsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "waldo harnessprobe mock: malformed request: "+err.Error(), http.StatusBadRequest)
		return
	}

	// The second request of the script carries the tool's output back as a
	// function_call_output item. It may arrive as a plain string or as a JSON
	// value wrapping the real output; store it raw either way, matching the
	// reference mock in test/mockmodel/server.py.
	for _, item := range req.Input {
		if item.Type != "function_call_output" || len(item.Output) == 0 {
			continue
		}
		var out string
		if err := json.Unmarshal(item.Output, &out); err != nil {
			out = string(item.Output)
		}
		m.record(out)
	}
	// A request carrying no tool output is a fresh turn and gets the scripted
	// tool call; one carrying it is the reply, and gets a closing message.
	_, hasResult := m.Result()
	firstTurn := !hasResult

	flusher := startStream(w)

	m.writeEvent(w, flusher, "response.created", map[string]any{
		"type":     "response.created",
		"response": map[string]any{"id": "resp_waldo_1"},
	})
	if firstTurn {
		name, args := m.pickTool(req.Tools)
		m.writeEvent(w, flusher, "response.output_item.done", map[string]any{
			"type": "response.output_item.done",
			"item": map[string]any{
				"type":      "function_call",
				"call_id":   "call_waldo_1",
				"name":      name,
				"arguments": args,
			},
		})
	} else {
		out, _ := m.Result()
		if len(out) > 4000 {
			out = out[:4000]
		}
		m.writeEvent(w, flusher, "response.output_item.done", map[string]any{
			"type": "response.output_item.done",
			"item": map[string]any{
				"type": "message",
				"role": "assistant",
				"content": []map[string]any{{
					"type": "output_text",
					"text": "OBSERVED: " + out,
				}},
			},
		})
		m.doneOnce.Do(func() { close(m.done) })
	}
	// The usage block is required by codex's stream parser even though every
	// counter is zero; omitting it strands the turn in "waiting for usage".
	m.writeEvent(w, flusher, "response.completed", map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"id": "resp_waldo_1",
			"usage": map[string]any{
				"input_tokens":          0,
				"input_tokens_details":  nil,
				"output_tokens":         0,
				"output_tokens_details": nil,
				"total_tokens":          0,
			},
		},
	})
}

// writeEvent emits one server-sent event and flushes it: the harness consumes
// the stream incrementally, and an event sitting in a buffer reads as a hung
// model.
func (m *Mock) writeEvent(w http.ResponseWriter, f http.Flusher, eventType string, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, data); err != nil {
		return // the harness went away mid-stream; nothing to do about it
	}
	if f != nil {
		f.Flush()
	}
}

// pickTool chooses which function tool to call and builds its arguments.
//
// Codex renames its shell tool across versions and behind feature flags —
// 0.148 advertises "exec_command", and `--disable unified_exec` swaps in
// "shell_command" — so the choice is adaptive: the first known shell tool the
// request actually advertises wins. If none is advertised, the first function
// tool is called with a generic {"command": ...} argument; that is wrong often
// enough to be loud about in the arguments' shape, but right often enough
// (every known codex shell tool accepts some spelling of a command) to be a
// better fallback than giving up.
func (m *Mock) pickTool(tools []toolSpec) (name string, arguments string) {
	command := m.command
	if command == "" {
		command = "echo " + m.marker + "; hostname"
	}
	advertised := map[string]bool{}
	var firstFunction string
	for _, t := range tools {
		if t.Type != "function" {
			continue
		}
		advertised[t.Name] = true
		if firstFunction == "" {
			firstFunction = t.Name
		}
	}
	known := []struct {
		name string
		args func(string) map[string]any
	}{
		{"exec_command", func(c string) map[string]any {
			return map[string]any{"cmd": c, "yield_time_ms": 1000, "max_output_tokens": 4000}
		}},
		{"shell_command", func(c string) map[string]any {
			return map[string]any{"command": c}
		}},
		{"shell", func(c string) map[string]any {
			return map[string]any{"command": c}
		}},
	}
	for _, k := range known {
		if advertised[k.name] {
			data, _ := json.Marshal(k.args(command))
			return k.name, string(data)
		}
	}
	if firstFunction != "" {
		data, _ := json.Marshal(map[string]any{"command": command})
		return firstFunction, string(data)
	}
	data, _ := json.Marshal(map[string]any{"command": command})
	return "shell", string(data)
}

// serveChat implements the chat-completions dialect.
//
// The chunk shapes mirror test/mockmodel/server.py exactly: turn one streams
// an empty assistant role chunk, a tool_calls chunk carrying the function name
// with empty arguments, a second tool_calls chunk carrying the arguments
// string alone, a final chunk with finish_reason "tool_calls", and the
// literal sentinel "data: [DONE]". Splitting name and arguments across chunks
// is deliberate — it is how real OpenAI streams arrive, and a harness that
// only handles whole-item delivery is exactly the kind of divergence this
// probe exists to surface. Turn two answers with the observed output as
// assistant content and finish_reason "stop".
func (m *Mock) serveChat(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var req chatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "waldo harnessprobe mock: malformed request: "+err.Error(), http.StatusBadRequest)
		return
	}

	for _, msg := range req.Messages {
		if msg.Role != "tool" || len(msg.Content) == 0 {
			continue
		}
		var out string
		if err := json.Unmarshal(msg.Content, &out); err != nil {
			// Content arrived as a list of parts rather than a plain string.
			var parts []struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal(msg.Content, &parts); err == nil {
				var sb strings.Builder
				for _, p := range parts {
					sb.WriteString(p.Text)
				}
				out = sb.String()
			}
		}
		m.record(out)
	}
	_, hasResult := m.Result()
	firstTurn := !hasResult

	flusher := startStream(w)

	if firstTurn {
		name, args := m.pickChatTool(req.Tools)
		m.writeChunk(w, flusher, map[string]any{"role": "assistant", "content": ""}, nil)
		m.writeChunk(w, flusher, map[string]any{"tool_calls": []map[string]any{{
			"index": 0, "id": "call_waldo_1", "type": "function",
			"function": map[string]any{"name": name, "arguments": ""},
		}}}, nil)
		m.writeChunk(w, flusher, map[string]any{"tool_calls": []map[string]any{{
			"index":    0,
			"function": map[string]any{"arguments": args},
		}}}, nil)
		m.writeChunk(w, flusher, map[string]any{}, "tool_calls")
	} else {
		out, _ := m.Result()
		if len(out) > 4000 {
			out = out[:4000]
		}
		m.writeChunk(w, flusher, map[string]any{"role": "assistant", "content": "OBSERVED: "}, nil)
		m.writeChunk(w, flusher, map[string]any{"content": out}, nil)
		m.writeChunk(w, flusher, map[string]any{}, "stop")
		m.doneOnce.Do(func() { close(m.done) })
	}
	// The [DONE] sentinel, not connection close, ends a chat-completions
	// stream; a harness waiting for it reads a missing one as a hang.
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

// writeChunk emits one chat-completions chunk. finish_reason is always
// present — null on all but the final chunk — because the reference
// implementation includes it and harness parsers have been built against that
// shape.
func (m *Mock) writeChunk(w http.ResponseWriter, f http.Flusher, delta map[string]any, finish any) {
	data, err := json.Marshal(map[string]any{
		"id":      "chatcmpl-waldo",
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   "waldo-mock",
		"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": finish}},
	})
	if err != nil {
		return
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
		return // the harness went away mid-stream; nothing to do about it
	}
	if f != nil {
		f.Flush()
	}
}

// pickChatTool chooses which tool to call in the chat dialect. Kimi Code's
// shell tool is "Bash" with a {"command": ...} argument; "bash" and "shell"
// are accepted spellings in case a version renames it. The fallback mirrors
// pickTool: the first advertised function tool with a generic command
// argument.
func (m *Mock) pickChatTool(tools []struct {
	Type     string `json:"type"`
	Function struct {
		Name string `json:"name"`
	} `json:"function"`
}) (name string, arguments string) {
	command := m.command
	if command == "" {
		command = "echo " + m.marker + "; hostname"
	}
	advertised := map[string]bool{}
	var firstFunction string
	for _, t := range tools {
		if t.Type != "function" {
			continue
		}
		advertised[t.Function.Name] = true
		if firstFunction == "" {
			firstFunction = t.Function.Name
		}
	}
	for _, known := range []string{"Bash", "bash", "shell"} {
		if advertised[known] {
			data, _ := json.Marshal(map[string]any{"command": command})
			return known, string(data)
		}
	}
	data, _ := json.Marshal(map[string]any{"command": command})
	if firstFunction != "" {
		return firstFunction, string(data)
	}
	return "Bash", string(data)
}

// geminiRequest is the slice of a Gemini API request the mock reads.
// Tools arrive under functionDeclarations; function results arrive as parts
// with a functionResponse field inside the last user-role content.
type geminiRequest struct {
	Contents []struct {
		Role  string `json:"role"`
		Parts []struct {
			Text         *string `json:"text,omitempty"`
			FunctionCall *struct {
				Name string         `json:"name"`
				Args map[string]any `json:"args"`
			} `json:"functionCall,omitempty"`
			FunctionResponse *struct {
				Name     string         `json:"name"`
				Response map[string]any `json:"response"`
			} `json:"functionResponse,omitempty"`
		} `json:"parts"`
	} `json:"contents"`
	Tools []struct {
		FunctionDeclarations []struct {
			Name string `json:"name"`
		} `json:"functionDeclarations"`
	} `json:"tools"`
}

// serveGemini implements the Gemini API streaming dialect.
//
// The Gemini CLI uses the @google/genai SDK which sends POST requests to
// /v1beta/models/{model}:streamGenerateContent with Server-Sent Events (SSE)
// streaming. Function calls arrive in candidates[0].content.parts[0].functionCall;
// function results are sent back in the next request as contents[N].parts[0].functionResponse.
//
// The response format uses `data: {...}\n\n` chunks (same SSE envelope as
// OpenAI, but different payload structure). The stream ends when the connection
// closes — no [DONE] sentinel.
func (m *Mock) serveGemini(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var req geminiRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "waldo harnessprobe mock: malformed request: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Scan every content for a functionResponse part. The SDK wraps the
	// shell tool's output as {functionResponse: {name, response: {output: "..."}}}
	// in a user-role content immediately following the model's functionCall.
	for _, content := range req.Contents {
		for _, part := range content.Parts {
			if part.FunctionResponse == nil {
				continue
			}
			out := ""
			if s, ok := part.FunctionResponse.Response["output"].(string); ok {
				out = s
			}
			if out == "" {
				// Fallback: marshal the whole response object in case the SDK
				// uses a different key (e.g. a future version may change the schema).
				data, _ := json.Marshal(part.FunctionResponse.Response)
				out = string(data)
			}
			m.record(out)
		}
	}

	_, hasResult := m.Result()
	firstTurn := !hasResult

	flusher := startStream(w)

	if firstTurn {
		name, args := m.pickGeminiTool(req.Tools)
		argsMap := map[string]any{}
		_ = json.Unmarshal([]byte(args), &argsMap)
		m.writeGeminiChunk(w, flusher, map[string]any{
			"candidates": []map[string]any{{
				"content": map[string]any{
					"parts": []map[string]any{{
						"functionCall": map[string]any{
							"name": name,
							"args": argsMap,
						},
					}},
					"role": "model",
				},
				"finishReason": "STOP",
				"index":        0,
			}},
			"usageMetadata": map[string]any{
				"promptTokenCount":     0,
				"candidatesTokenCount": 0,
				"totalTokenCount":      0,
			},
		})
	} else {
		out, _ := m.Result()
		if len(out) > 4000 {
			out = out[:4000]
		}
		m.writeGeminiChunk(w, flusher, map[string]any{
			"candidates": []map[string]any{{
				"content": map[string]any{
					"parts": []map[string]any{{"text": "OBSERVED: " + out}},
					"role":  "model",
				},
				"finishReason": "STOP",
				"index":        0,
			}},
			"usageMetadata": map[string]any{
				"promptTokenCount":     0,
				"candidatesTokenCount": 0,
				"totalTokenCount":      0,
			},
		})
		m.doneOnce.Do(func() { close(m.done) })
	}
}

// writeGeminiChunk emits one Gemini SSE chunk and flushes it.
func (m *Mock) writeGeminiChunk(w http.ResponseWriter, f http.Flusher, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
		return
	}
	if f != nil {
		f.Flush()
	}
}

// pickGeminiTool chooses which function tool to call in the Gemini dialect.
// Gemini CLI's shell tool is "run_shell_command" with a {"command": ...} argument.
func (m *Mock) pickGeminiTool(tools []struct {
	FunctionDeclarations []struct {
		Name string `json:"name"`
	} `json:"functionDeclarations"`
}) (name string, arguments string) {
	command := m.command
	if command == "" {
		command = "echo " + m.marker + "; hostname"
	}
	advertised := map[string]bool{}
	var firstFunction string
	for _, tool := range tools {
		for _, decl := range tool.FunctionDeclarations {
			advertised[decl.Name] = true
			if firstFunction == "" {
				firstFunction = decl.Name
			}
		}
	}
	// Gemini CLI's shell tool is "run_shell_command"; accept "shell" as a
	// fallback for harnesses that rename it.
	for _, known := range []string{"run_shell_command", "shell", "bash"} {
		if advertised[known] {
			data, _ := json.Marshal(map[string]any{"command": command})
			return known, string(data)
		}
	}
	data, _ := json.Marshal(map[string]any{"command": command})
	if firstFunction != "" {
		return firstFunction, string(data)
	}
	return "run_shell_command", string(data)
}
