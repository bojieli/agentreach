package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"strings"
	"testing"

	"github.com/bojieli/agentreach/internal/fileops"
	"github.com/bojieli/agentreach/internal/mirror"
	"github.com/bojieli/agentreach/internal/session"
	"github.com/bojieli/agentreach/internal/reach"
)

// The hook decides, inside an agent's turn, whether a tool call reads the
// target's file or the operator's own. Every wrong answer here is the failure
// this project exists to prevent, and none of it was tested: the routing was
// welded to stdin and to a live transport, so the only way to exercise it was
// to run an agent against a real host.

// fakeOps is the target as far as the mirror is concerned.
//
// It embeds the interface rather than implementing all of it, so a call the
// mirror is not supposed to make panics instead of quietly succeeding — the
// hook doing more work on the target than we think is itself a defect.
type fakeOps struct {
	fileops.FileOps
	files    map[string][]byte
	readErr  error
	writeErr error
	writes   int
}

func (f *fakeOps) Read(_ context.Context, p string, off, n int64) ([]byte, error) {
	if f.readErr != nil {
		return nil, f.readErr
	}
	data, ok := f.files[p]
	if !ok {
		return nil, &reach.NotFoundError{Path: p}
	}
	if off > int64(len(data)) {
		return nil, nil
	}
	data = data[off:]
	if n > 0 && n < int64(len(data)) {
		data = data[:n]
	}
	return data, nil
}

func (f *fakeOps) Write(_ context.Context, p string, data []byte, _ fs.FileMode) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	f.writes++
	f.files[p] = append([]byte(nil), data...)
	return nil
}

// Hash is what the mirror asks for instead of reading a whole file back to
// compare it. A broken transport fails it the same way it fails a read.
func (f *fakeOps) Hash(_ context.Context, p string) (string, error) {
	if f.readErr != nil {
		return "", f.readErr
	}
	data, ok := f.files[p]
	if !ok {
		return "", &reach.NotFoundError{Path: p}
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func (f *fakeOps) Close() error { return nil }

func mirrorSession() *session.Session {
	return &session.Session{
		Name: "test",
		Mode: session.ModeMirror,
		Target: &session.Target{
			Kind:      session.KindSSH,
			Host:      "example.invalid",
			Workspace: "/srv/app",
			Raw:       "ssh://example.invalid/srv/app",
		},
	}
}

func event(name, tool string, input map[string]any) hookEvent {
	raw, err := json.Marshal(input)
	if err != nil {
		panic(err)
	}
	return hookEvent{HookEventName: name, ToolName: tool, ToolInput: raw}
}

// --- routing: the decisions that must not need the target ------------------

func TestHookRouteDeniesSearchTools(t *testing.T) {
	s := mirrorSession()
	for _, tool := range []string{"Grep", "Glob"} {
		reply, _, ok := hookRoute(event("PreToolUse", tool, map[string]any{"pattern": "x"}), s)
		if ok {
			t.Fatalf("%s was routed to the mirror; it must be denied outright", tool)
		}
		out := reply.HookSpecificOutput
		if out == nil || out.PermissionDecision != "deny" {
			t.Fatalf("%s: got %+v, want a deny", tool, out)
		}
		// A denial that does not say what to do instead just costs the agent a
		// turn. It must name the workspace and a command that runs on the target.
		if !strings.Contains(out.PermissionDecisionReason, s.Target.Workspace) {
			t.Errorf("%s denial does not mention the workspace: %s", tool, out.PermissionDecisionReason)
		}
		if !strings.Contains(out.PermissionDecisionReason, "rg ") {
			t.Errorf("%s denial offers no shell alternative: %s", tool, out.PermissionDecisionReason)
		}
	}
}

// PostToolUse for a search tool has nothing to decide: the call never ran.
// Denying there would be a denial the harness reports after the fact.
func TestHookRouteIgnoresSearchToolsAfterTheFact(t *testing.T) {
	reply, _, ok := hookRoute(event("PostToolUse", "Grep", map[string]any{"pattern": "x"}), mirrorSession())
	if ok || reply.HookSpecificOutput != nil {
		t.Fatalf("PostToolUse Grep produced %+v, want an empty reply", reply)
	}
}

func TestHookRoutePassesThroughUnrelatedTools(t *testing.T) {
	s := mirrorSession()
	// Bash is the one that matters most: in mirror mode it still runs on the
	// target through the shell prefix, and a hook that rewrote or denied it would
	// break the half of the session that already works.
	//
	// The MCP entries carry a file_path deliberately. Rewriting a path belonging
	// to a tool reach knows nothing about would point it at a mirror directory
	// the tool has no reason to understand — and an MCP server's file_path may
	// not even name a file on the target. Recognition has to be by tool name, not
	// by the presence of the field.
	for _, tc := range []struct {
		tool  string
		input map[string]any
	}{
		{"Bash", map[string]any{"command": "ls"}},
		{"WebFetch", map[string]any{"url": "https://example.invalid"}},
		{"TodoWrite", map[string]any{"todos": []any{}}},
		{"", map[string]any{}},
		{"mcp__fs__read", map[string]any{"file_path": "/srv/app/main.go"}},
		{"ReadNotebook", map[string]any{"file_path": "/srv/app/x.ipynb"}},
	} {
		reply, _, ok := hookRoute(event("PreToolUse", tc.tool, tc.input), s)
		if ok {
			t.Errorf("%s was routed to the mirror; reach only rewrites tools it knows", tc.tool)
		}
		if reply.HookSpecificOutput != nil {
			t.Errorf("%s got a decision (%+v); reach must not interfere", tc.tool, reply.HookSpecificOutput)
		}
	}
}

func TestHookRouteIgnoresMalformedInput(t *testing.T) {
	s := mirrorSession()
	for _, tc := range []struct {
		name  string
		input json.RawMessage
	}{
		{"not an object", json.RawMessage(`"a string"`)},
		{"truncated", json.RawMessage(`{"file_path":`)},
		{"empty", json.RawMessage(``)},
		{"no file_path", json.RawMessage(`{"offset":3}`)},
		{"empty file_path", json.RawMessage(`{"file_path":""}`)},
		{"file_path is not a string", json.RawMessage(`{"file_path":42}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ev := hookEvent{HookEventName: "PreToolUse", ToolName: "Read", ToolInput: tc.input}
			reply, _, ok := hookRoute(ev, s)
			if ok {
				t.Fatalf("routed to the mirror with no usable path")
			}
			// Silence, not a denial. Malformed input is the harness's problem;
			// blocking the tool over it would break a session reach could have
			// simply stayed out of.
			if reply.HookSpecificOutput != nil {
				t.Fatalf("got %+v, want an empty reply", reply.HookSpecificOutput)
			}
		})
	}
}

// --- mirror: fetch before a read, push after a write -----------------------

func newMirror(t *testing.T, ops *fakeOps) *mirror.Mirror {
	t.Helper()
	return mirror.New(t.TempDir(), ops)
}

// readFile and writeFile stand in for the agent's own tool touching the
// mirrored copy between PreToolUse and PostToolUse.
func readFile(t *testing.T, p string) string {
	t.Helper()
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read mirrored file: %v", err)
	}
	return string(data)
}

func writeFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write mirrored file: %v", err)
	}
}

func TestHookMirrorRewritesReadToTheMirroredCopy(t *testing.T) {
	ops := &fakeOps{files: map[string][]byte{"/srv/app/main.go": []byte("package main\n")}}
	m := newMirror(t, ops)
	s := mirrorSession()

	input := map[string]any{"file_path": "/srv/app/main.go", "offset": float64(1)}
	reply := hookMirror(context.Background(), event("PreToolUse", "Read", input), s, m, input)

	out := reply.HookSpecificOutput
	if out == nil || out.PermissionDecision != "allow" {
		t.Fatalf("got %+v, want an allow", out)
	}
	var updated map[string]any
	if err := json.Unmarshal(out.UpdatedInput, &updated); err != nil {
		t.Fatalf("updatedInput is not JSON: %v", err)
	}
	local, _ := updated["file_path"].(string)
	if local == "/srv/app/main.go" {
		t.Fatal("file_path was not rewritten; the agent would read its own machine")
	}
	if !strings.HasPrefix(local, m.Root()) {
		t.Fatalf("rewritten path %q is outside the mirror root %q", local, m.Root())
	}
	// The rest of the tool's arguments have to survive the rewrite, or a
	// windowed read silently becomes a whole-file read.
	if updated["offset"] != float64(1) {
		t.Errorf("offset was lost in the rewrite: %+v", updated)
	}
	if got := readFile(t, local); got != "package main\n" {
		t.Errorf("mirrored copy holds %q, want the target's content", got)
	}
}

// A relative path is resolved against the workspace, not against whatever
// directory the harness happens to have started in.
func TestHookMirrorResolvesRelativePathsAgainstTheWorkspace(t *testing.T) {
	ops := &fakeOps{files: map[string][]byte{"/srv/app/internal/x.go": []byte("x")}}
	m := newMirror(t, ops)

	input := map[string]any{"file_path": "internal/x.go"}
	reply := hookMirror(context.Background(), event("PreToolUse", "Read", input), mirrorSession(), m, input)

	out := reply.HookSpecificOutput
	if out == nil || out.PermissionDecision != "allow" {
		t.Fatalf("got %+v, want an allow", out)
	}
	var updated map[string]any
	_ = json.Unmarshal(out.UpdatedInput, &updated)
	if got := readFile(t, updated["file_path"].(string)); got != "x" {
		t.Errorf("mirrored copy holds %q, want the target's content", got)
	}
}

// Files outside the workspace belong to the operator's own machine — the
// harness's settings, a scratch note. Rewriting those would make reach reach
// across to a target for a file that has nothing to do with it.
func TestHookMirrorLeavesPathsOutsideTheWorkspaceAlone(t *testing.T) {
	ops := &fakeOps{files: map[string][]byte{}}
	m := newMirror(t, ops)

	for _, p := range []string{"/etc/passwd", "/home/me/.claude/settings.json", "/srv/appendix/x"} {
		input := map[string]any{"file_path": p}
		reply := hookMirror(context.Background(), event("PreToolUse", "Read", input), mirrorSession(), m, input)
		if reply.HookSpecificOutput != nil {
			t.Errorf("%s got a decision (%+v); it is local and must be left alone", p, reply.HookSpecificOutput)
		}
	}
}

// A Write to a path that does not exist yet must still be allowed: refusing it
// would make it impossible for an agent to create a file.
func TestHookMirrorAllowsWritingANewFile(t *testing.T) {
	ops := &fakeOps{files: map[string][]byte{}}
	m := newMirror(t, ops)

	input := map[string]any{"file_path": "/srv/app/new.go", "content": "hello"}
	reply := hookMirror(context.Background(), event("PreToolUse", "Write", input), mirrorSession(), m, input)

	out := reply.HookSpecificOutput
	if out == nil || out.PermissionDecision != "allow" {
		t.Fatalf("got %+v, want an allow for a file that does not exist yet", out)
	}
}

// The fetch failing is the case that must never become a hang or a wrong
// answer: the agent is told, in a value it can read, that the file did not come
// from the target.
func TestHookMirrorDeniesWhenTheTargetIsUnreachable(t *testing.T) {
	ops := &fakeOps{files: map[string][]byte{}, readErr: context.DeadlineExceeded}
	m := newMirror(t, ops)
	s := mirrorSession()

	input := map[string]any{"file_path": "/srv/app/main.go"}
	reply := hookMirror(context.Background(), event("PreToolUse", "Read", input), s, m, input)

	out := reply.HookSpecificOutput
	if out == nil || out.PermissionDecision != "deny" {
		t.Fatalf("got %+v, want a deny", out)
	}
	if !strings.Contains(out.PermissionDecisionReason, "/srv/app/main.go") ||
		!strings.Contains(out.PermissionDecisionReason, s.Target.Describe()) {
		t.Errorf("denial does not say which file or which target: %s", out.PermissionDecisionReason)
	}
}

func TestHookMirrorPushesAfterAWrite(t *testing.T) {
	ops := &fakeOps{files: map[string][]byte{"/srv/app/main.go": []byte("old\n")}}
	m := newMirror(t, ops)
	s := mirrorSession()

	// Pre fetches the file and hands the agent a local path.
	input := map[string]any{"file_path": "/srv/app/main.go"}
	pre := hookMirror(context.Background(), event("PreToolUse", "Edit", input), s, m, input)
	var updated map[string]any
	_ = json.Unmarshal(pre.HookSpecificOutput.UpdatedInput, &updated)
	local := updated["file_path"].(string)

	// The agent edits that local path.
	writeFile(t, local, "new\n")

	// Post is handed the rewritten path back, and must recognise it as standing
	// for the target's file rather than treating it as a stray local path.
	postInput := map[string]any{"file_path": local}
	post := hookMirror(context.Background(), event("PostToolUse", "Edit", postInput), s, m, postInput)
	if post.HookSpecificOutput != nil {
		t.Fatalf("push reported a problem: %+v", post.HookSpecificOutput)
	}
	if got := string(ops.files["/srv/app/main.go"]); got != "new\n" {
		t.Errorf("target holds %q, want the edit to have landed", got)
	}
}

// The edit has already happened on the mirror by the time PostToolUse runs, so
// a failed push is the one case where the agent's belief and reality diverge.
// It has to be told loudly, or it moves on believing the change is saved.
func TestHookMirrorReportsAFailedPush(t *testing.T) {
	ops := &fakeOps{files: map[string][]byte{"/srv/app/main.go": []byte("old\n")}}
	m := newMirror(t, ops)
	s := mirrorSession()

	input := map[string]any{"file_path": "/srv/app/main.go"}
	pre := hookMirror(context.Background(), event("PreToolUse", "Edit", input), s, m, input)
	var updated map[string]any
	_ = json.Unmarshal(pre.HookSpecificOutput.UpdatedInput, &updated)
	local := updated["file_path"].(string)
	writeFile(t, local, "new\n")

	ops.writeErr = context.DeadlineExceeded

	postInput := map[string]any{"file_path": local}
	post := hookMirror(context.Background(), event("PostToolUse", "Edit", postInput), s, m, postInput)
	out := post.HookSpecificOutput
	if out == nil || out.AdditionalContext == "" {
		t.Fatalf("a failed push produced %+v; the agent would believe the write landed", out)
	}
	if !strings.Contains(out.AdditionalContext, "NOT SAVED") {
		t.Errorf("failure message is not unmissable: %q", out.AdditionalContext)
	}
}

// PostToolUse for a Read has nothing to push. Pushing there would write the
// file back to the target on every read — turning a read-only operation into a
// write, and bumping its mtime for every build watching the tree.
func TestHookMirrorDoesNotPushAfterARead(t *testing.T) {
	ops := &fakeOps{files: map[string][]byte{"/srv/app/main.go": []byte("x")}}
	m := newMirror(t, ops)
	s := mirrorSession()

	input := map[string]any{"file_path": "/srv/app/main.go"}
	pre := hookMirror(context.Background(), event("PreToolUse", "Read", input), s, m, input)
	var updated map[string]any
	_ = json.Unmarshal(pre.HookSpecificOutput.UpdatedInput, &updated)

	postInput := map[string]any{"file_path": updated["file_path"]}
	post := hookMirror(context.Background(), event("PostToolUse", "Read", postInput), s, m, postInput)
	if post.HookSpecificOutput != nil {
		t.Fatalf("got %+v, want an empty reply", post.HookSpecificOutput)
	}
	if ops.writes != 0 {
		t.Errorf("a Read caused %d write(s) to the target", ops.writes)
	}
}

// An event name reach does not know must be inert. Harnesses add hook events
// over time, and a new one arriving must not turn into a denial.
func TestHookMirrorIgnoresUnknownEvents(t *testing.T) {
	ops := &fakeOps{files: map[string][]byte{"/srv/app/main.go": []byte("x")}}
	m := newMirror(t, ops)

	input := map[string]any{"file_path": "/srv/app/main.go"}
	reply := hookMirror(context.Background(), event("SomethingNew", "Read", input), mirrorSession(), m, input)
	if reply.HookSpecificOutput != nil {
		t.Fatalf("got %+v, want an empty reply", reply.HookSpecificOutput)
	}
	if ops.writes != 0 {
		t.Errorf("an unknown event caused %d write(s) to the target", ops.writes)
	}
}

// --- the top level ---------------------------------------------------------

// With no session loaded, the hook must be invisible: emit an empty reply and
// exit 0. A hook that errors out here would break every harness whose config
// still mentions reach after the session is gone.
func TestHookDecideWithoutASession(t *testing.T) {
	t.Setenv("REACH_HOME", t.TempDir())
	t.Setenv("REACH_SESSION", "does-not-exist")

	for _, raw := range []string{
		`{"hook_event_name":"PreToolUse","tool_name":"Read","tool_input":{"file_path":"/srv/app/x"}}`,
		`{"hook_event_name":"PreToolUse","tool_name":"Grep","tool_input":{"pattern":"x"}}`,
		`not json at all`,
		``,
	} {
		reply := hookDecide(context.Background(), []byte(raw))
		if reply.HookSpecificOutput != nil || reply.SystemMessage != "" {
			t.Errorf("input %q produced %+v, want an empty reply", raw, reply)
		}
	}
}

// Whatever the hook decides, the harness has to be able to parse it.
func TestHookRepliesAreAlwaysValidJSON(t *testing.T) {
	s := mirrorSession()
	for _, ev := range []hookEvent{
		event("PreToolUse", "Grep", map[string]any{"pattern": "x"}),
		event("PreToolUse", "Bash", map[string]any{"command": "ls"}),
		event("PreToolUse", "Read", map[string]any{"file_path": "/srv/app/x"}),
	} {
		reply, _, _ := hookRoute(ev, s)
		if _, err := json.Marshal(reply); err != nil {
			t.Errorf("%s/%s reply does not marshal: %v", ev.HookEventName, ev.ToolName, err)
		}
	}
}
