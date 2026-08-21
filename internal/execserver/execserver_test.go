package execserver

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bojieli/agentreach/internal/fileops"
	"github.com/bojieli/agentreach/internal/session"
	"github.com/bojieli/agentreach/internal/transport"
)

// frame is one decoded protocol message from the server.
type frame struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// testClient is the codex end of the connection: it writes requests and
// collects responses (matched by id) and notifications.
type testClient struct {
	stdin io.WriteCloser

	mu     sync.Mutex
	cond   *sync.Cond
	nextID int
	frames []frame
}

func newTestClient(t *testing.T, srv *Server) *testClient {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	c := &testClient{stdin: inW}
	c.cond = sync.NewCond(&c.mu)

	serveDone := make(chan error, 1)
	go func() { serveDone <- srv.Serve(context.Background(), inR, outW) }()
	t.Cleanup(func() {
		_ = inW.Close()
		if err := <-serveDone; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})

	go func() {
		sc := bufio.NewScanner(outR)
		sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
		for sc.Scan() {
			var f frame
			if err := json.Unmarshal(sc.Bytes(), &f); err != nil {
				t.Errorf("server emitted an unparseable frame: %v: %s", err, sc.Text())
				continue
			}
			c.mu.Lock()
			c.frames = append(c.frames, f)
			c.cond.Broadcast()
			c.mu.Unlock()
		}
	}()
	return c
}

// call sends one request and waits for its response.
func (c *testClient) call(t *testing.T, method string, params any) frame {
	t.Helper()
	return c.await(t, c.send(t, method, params))
}

// send writes a request without waiting for its answer, so a test can pipeline
// the way a client is allowed to.
func (c *testClient) send(t *testing.T, method string, params any) int {
	t.Helper()
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	c.mu.Unlock()

	var raw json.RawMessage
	if params != nil {
		data, err := json.Marshal(params)
		if err != nil {
			t.Fatalf("marshal params: %v", err)
		}
		raw = data
	}
	msg, _ := json.Marshal(map[string]any{"id": id, "method": method, "params": raw})
	if _, err := c.stdin.Write(append(msg, '\n')); err != nil {
		t.Fatalf("write request: %v", err)
	}
	return id
}

// await collects the answer to one request.
func (c *testClient) await(t *testing.T, id int) frame {
	t.Helper()
	want := json.RawMessage(fmt.Sprintf("%d", id))
	deadline := time.Now().Add(30 * time.Second)
	c.mu.Lock()
	defer c.mu.Unlock()
	for {
		for i, f := range c.frames {
			if string(f.ID) == string(want) {
				c.frames = append(c.frames[:i], c.frames[i+1:]...)
				return f
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no response to request %d within 30s", id)
		}
		// sync.Cond has no timed wait; poll at a small interval instead.
		c.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
		c.mu.Lock()
	}
}

// notifications drains the collected notifications, optionally filtered by
// method.
func (c *testClient) notifications(method string) []frame {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out, rest []frame
	for _, f := range c.frames {
		if f.Method == method || method == "" {
			out = append(out, f)
		} else {
			rest = append(rest, f)
		}
	}
	c.frames = rest
	return out
}

// testEnv builds a server over the local transport: the "target" is a temp
// directory (the session root), and "workspace" a second temp directory
// standing in for the local directory codex was launched in.
type testEnv struct {
	srv       *Server
	client    *testClient
	root      string
	workspace string
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	t.Setenv("REACH_HOME", t.TempDir())
	root := t.TempDir()
	workspace := t.TempDir()

	tr, err := transport.NewLocal()
	if err != nil {
		t.Skipf("no local POSIX shell: %v", err)
	}
	caps, err := fileops.Probe(context.Background(), tr)
	if err != nil {
		t.Fatalf("probe local capabilities: %v", err)
	}
	sess := &session.Session{
		Name:   "test",
		Target: &session.Target{Kind: session.KindLocal, Workspace: root, Raw: "local://" + root},
		Caps:   caps,
	}
	srv, err := New(context.Background(), sess, workspace)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Order matters: the pumps write the command's audit record last, and
	// REACH_HOME — where that record goes — is a t.TempDir() this cleanup runs
	// ahead of. Without the wait, a process still exiting writes into a
	// directory being deleted and the test fails in teardown.
	t.Cleanup(func() {
		srv.terminateAll()
		srv.waitForProcesses()
		_ = srv.Close()
	})
	env := &testEnv{srv: srv, root: root, workspace: workspace}
	env.client = newTestClient(t, srv)
	return env
}

// initialize drives the handshake codex performs.
func (e *testEnv) initialize(t *testing.T) {
	t.Helper()
	f := e.client.call(t, "initialize", map[string]any{"clientName": "test"})
	if f.Error != nil {
		t.Fatalf("initialize: %v", f.Error.Message)
	}
	var result struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(f.Result, &result); err != nil || result.SessionID == "" {
		t.Fatalf("initialize result has no sessionId: %s", f.Result)
	}
	// The `initialized` notification.
	if _, err := e.client.stdin.Write([]byte("{\"method\":\"initialized\"}\n")); err != nil {
		t.Fatal(err)
	}
}

func TestInitializeAndEnvironmentInfo(t *testing.T) {
	env := newTestEnv(t)

	// Methods before initialize are rejected, as codex's own server rejects
	// them.
	f := env.client.call(t, "environment/info", map[string]any{})
	if f.Error == nil || f.Error.Code != codeInvalidRequest {
		t.Fatalf("environment/info before initialize: got %+v", f)
	}

	env.initialize(t)

	f = env.client.call(t, "environment/info", nil)
	if f.Error != nil {
		t.Fatalf("environment/info: %s", f.Error.Message)
	}
	var info struct {
		Shell struct {
			Name string `json:"name"`
			Path string `json:"path"`
		} `json:"shell"`
		Cwd          string `json:"cwd"`
		Capabilities struct {
			NetworkProxyLaunch         bool `json:"networkProxyLaunch"`
			CapabilityDiscoverySandbox bool `json:"capabilityDiscoverySandbox"`
			EnvironmentConfigRead      bool `json:"environmentConfigRead"`
			SandboxedFileStreaming     bool `json:"sandboxedFileStreaming"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(f.Result, &info); err != nil {
		t.Fatalf("environment/info result: %v: %s", err, f.Result)
	}
	if info.Shell.Name != "bash" && info.Shell.Name != "sh" {
		t.Errorf("shell name %q is not one codex accepts", info.Shell.Name)
	}
	if !strings.HasPrefix(info.Shell.Path, "/") {
		t.Errorf("shell path %q is not absolute", info.Shell.Path)
	}
	if info.Cwd != pathToURI(env.root) {
		t.Errorf("cwd = %q, want %q", info.Cwd, pathToURI(env.root))
	}
	if info.Capabilities.NetworkProxyLaunch || info.Capabilities.CapabilityDiscoverySandbox ||
		info.Capabilities.EnvironmentConfigRead || info.Capabilities.SandboxedFileStreaming {
		t.Errorf("capabilities must all be false: %+v", info.Capabilities)
	}

	f = env.client.call(t, "environment/status", nil)
	if f.Error != nil || string(f.Result) != `{"status":"ready"}` {
		t.Errorf("environment/status: %+v", f)
	}

	f = env.client.call(t, "no/suchMethod", nil)
	if f.Error == nil || f.Error.Code != codeMethodNotFound {
		t.Errorf("unknown method: got %+v", f)
	}

	f = env.client.call(t, "http/request", map[string]any{"method": "GET", "url": "https://example.test", "requestId": "r1"})
	if f.Error == nil {
		t.Errorf("http/request must be a protocol error, got %+v", f)
	}
}

// startProcess runs one process/start and returns the processId.
func (e *testEnv) startProcess(t *testing.T, id, script string, extra map[string]any) {
	t.Helper()
	params := map[string]any{
		"processId": id,
		"argv":      []string{"/bin/bash", "-lc", script},
		"cwd":       pathToURI(e.workspace),
		"env":       map[string]string{},
		"tty":       false,
	}
	for k, v := range extra {
		params[k] = v
	}
	f := e.client.call(t, "process/start", params)
	if f.Error != nil {
		t.Fatalf("process/start %s: %s", id, f.Error.Message)
	}
	var result struct {
		ProcessID string `json:"processId"`
	}
	if err := json.Unmarshal(f.Result, &result); err != nil || result.ProcessID != id {
		t.Fatalf("process/start result: %s", f.Result)
	}
}

// readRound issues one process/read and returns what it yielded. It is the
// shared half of readAll and awaitStdout: the difference between them is only
// when they stop asking.
func (e *testEnv) readRound(t *testing.T, id string, afterSeq uint64) (stdout, stderr string, next uint64, exited bool, exitCode int) {
	t.Helper()
	f := e.client.call(t, "process/read", map[string]any{
		"processId": id,
		"afterSeq":  afterSeq,
		"waitMs":    10000,
	})
	if f.Error != nil {
		t.Fatalf("process/read %s: %s", id, f.Error.Message)
	}
	var resp struct {
		Chunks []struct {
			Seq    uint64 `json:"seq"`
			Stream string `json:"stream"`
			Chunk  string `json:"chunk"`
		} `json:"chunks"`
		NextSeq  uint64  `json:"nextSeq"`
		Exited   bool    `json:"exited"`
		ExitCode *int    `json:"exitCode"`
		Failure  *string `json:"failure"`
	}
	if err := json.Unmarshal(f.Result, &resp); err != nil {
		t.Fatalf("process/read result: %v: %s", err, f.Result)
	}
	for _, c := range resp.Chunks {
		data, err := base64.StdEncoding.DecodeString(c.Chunk)
		if err != nil {
			t.Fatalf("chunk %d is not base64: %v", c.Seq, err)
		}
		if c.Stream == "stdout" {
			stdout += string(data)
		} else {
			stderr += string(data)
		}
	}
	// nextSeq is the seq the *next* chunk will get, so "everything after
	// the last chunk I have" is nextSeq-1 — the same arithmetic codex's
	// own client does (remote_process.rs).
	next = afterSeq
	if resp.NextSeq > 0 {
		next = resp.NextSeq - 1
	}
	if resp.Exited {
		if resp.Failure != nil && *resp.Failure != "" {
			t.Fatalf("process %s failed at the transport: %s", id, *resp.Failure)
		}
		if resp.ExitCode == nil {
			t.Fatalf("exited but no exitCode: %s", f.Result)
		}
		return stdout, stderr, next, true, *resp.ExitCode
	}
	return stdout, stderr, next, false, 0
}

// readAll polls process/read until the process exits and returns the
// aggregated stdout, stderr and exit code.
func (e *testEnv) readAll(t *testing.T, id string) (stdout, stderr string, exitCode int) {
	t.Helper()
	var afterSeq uint64
	for {
		out, errOut, next, exited, code := e.readRound(t, id, afterSeq)
		stdout += out
		stderr += errOut
		afterSeq = next
		if exited {
			return stdout, stderr, code
		}
	}
}

// awaitStdout reads until a still-running process has written something, and
// returns it with the sequence to carry on from. It exists so a test can
// establish that a process consumed its input before doing anything else to
// that process.
func (e *testEnv) awaitStdout(t *testing.T, id string) (string, uint64) {
	t.Helper()
	var afterSeq uint64
	var stdout string
	for range 10 {
		out, _, next, exited, _ := e.readRound(t, id, afterSeq)
		stdout += out
		afterSeq = next
		if stdout != "" {
			return stdout, afterSeq
		}
		if exited {
			t.Fatalf("%s exited without writing anything", id)
		}
	}
	t.Fatalf("%s wrote nothing", id)
	return "", 0
}

// drainFrom reads a process to exit, continuing after a sequence some earlier
// read already consumed.
func (e *testEnv) drainFrom(t *testing.T, id string, afterSeq uint64) string {
	t.Helper()
	var stdout string
	for {
		out, _, next, exited, _ := e.readRound(t, id, afterSeq)
		stdout += out
		afterSeq = next
		if exited {
			return stdout
		}
	}
}

func TestProcessEchoRunsAndExits(t *testing.T) {
	env := newTestEnv(t)
	env.initialize(t)

	env.startProcess(t, "p1", "echo hello-from-target; echo oops >&2; exit 3", nil)
	stdout, stderr, code := env.readAll(t, "p1")
	if stdout != "hello-from-target\n" {
		t.Errorf("stdout = %q", stdout)
	}
	if stderr != "oops\n" {
		t.Errorf("stderr = %q", stderr)
	}
	if code != 3 {
		t.Errorf("exit code = %d, want 3", code)
	}

	// The terminal notifications arrived as well as the pollable state.
	var sawExited, sawClosed bool
	for _, n := range env.client.notifications("") {
		if n.Method == "process/exited" {
			sawExited = true
		}
		if n.Method == "process/closed" {
			sawClosed = true
		}
	}
	if !sawExited || !sawClosed {
		t.Errorf("missing terminal notifications: exited=%v closed=%v", sawExited, sawClosed)
	}
}

func TestProcessRunsOnTheTargetCwd(t *testing.T) {
	env := newTestEnv(t)
	env.initialize(t)

	// The cwd codex sends is its local launch directory; the command must run
	// in the session root on the target — mapped, never literal.
	env.startProcess(t, "p1", "pwd -P; touch made-here.txt", nil)
	stdout, _, code := env.readAll(t, "p1")
	if code != 0 {
		t.Fatalf("exit code %d", code)
	}
	pwd := strings.TrimSpace(stdout)
	resolved, _ := filepath.EvalSymlinks(env.root)
	if pwd != env.root && pwd != resolved {
		t.Errorf("command ran in %q, want session root %q", pwd, env.root)
	}
	if _, err := os.Stat(filepath.Join(env.root, "made-here.txt")); err != nil {
		t.Errorf("the file the command made is not in the session root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(env.workspace, "made-here.txt")); err == nil {
		t.Errorf("the file landed in the local workspace — the mapping leaked")
	}
}

func TestProcessWriteAndTerminate(t *testing.T) {
	env := newTestEnv(t)
	env.initialize(t)

	// Deliberately larger than the tail the sentinel filter withholds. That
	// filter passes bytes through only once enough have arrived to rule out a
	// partial exit marker, so a one-line echo reaches no client until the
	// stream ends — and this test would then be asserting on which of cat's
	// echo and the kill below got there first. Under CI load the kill won.
	typed := strings.Repeat("typed-by-the-agent ", 256) + "\n"

	env.startProcess(t, "p1", "cat", map[string]any{"pipeStdin": true})
	f := env.client.call(t, "process/write", map[string]any{
		"processId": "p1",
		"chunk":     base64.StdEncoding.EncodeToString([]byte(typed)),
		"writeId":   "w1",
	})
	if f.Error != nil {
		t.Fatalf("process/write: %s", f.Error.Message)
	}
	if !strings.Contains(string(f.Result), "accepted") {
		t.Fatalf("process/write result: %s", f.Result)
	}
	// A retried write is acknowledged, not duplicated.
	f = env.client.call(t, "process/write", map[string]any{
		"processId": "p1",
		"chunk":     base64.StdEncoding.EncodeToString([]byte(typed)),
		"writeId":   "w1",
	})
	if !strings.Contains(string(f.Result), "accepted") {
		t.Fatalf("retried write: %s", f.Result)
	}

	// cat has echoed, so terminating now cannot be what decides the assertion
	// at the end.
	echoed, afterSeq := env.awaitStdout(t, "p1")

	f = env.client.call(t, "process/terminate", map[string]any{"processId": "p1"})
	if !strings.Contains(string(f.Result), `"running":true`) {
		t.Fatalf("process/terminate: %s", f.Result)
	}
	// The rest is the tail the filter was holding. The total is what decides
	// whether the retried write was applied a second time.
	echoed += env.drainFrom(t, "p1", afterSeq)
	if echoed != typed {
		t.Errorf("cat echoed %d bytes, want the %d typed, exactly once", len(echoed), len(typed))
	}
}

func TestFsRoundTripThroughTheMapping(t *testing.T) {
	env := newTestEnv(t)
	env.initialize(t)

	// A path under codex's local workspace maps onto the session root. The
	// parent directory is created first, as codex itself would: writeFile does
	// not create parents.
	f := env.client.call(t, "fs/createDirectory", map[string]any{
		"path":      pathToURI(filepath.Join(env.workspace, "sub")),
		"recursive": true,
	})
	if f.Error != nil {
		t.Fatalf("fs/createDirectory: %s", f.Error.Message)
	}
	content := "the word remote\n"
	f = env.client.call(t, "fs/writeFile", map[string]any{
		"path":       pathToURI(filepath.Join(env.workspace, "sub", "file.txt")),
		"dataBase64": base64.StdEncoding.EncodeToString([]byte(content)),
	})
	if f.Error != nil {
		t.Fatalf("fs/writeFile: %s", f.Error.Message)
	}
	onDisk, err := os.ReadFile(filepath.Join(env.root, "sub", "file.txt"))
	if err != nil {
		t.Fatalf("the write did not land in the session root: %v", err)
	}
	if string(onDisk) != content {
		t.Fatalf("on-disk content %q", onDisk)
	}

	f = env.client.call(t, "fs/readFile", map[string]any{
		"path": pathToURI(filepath.Join(env.workspace, "sub", "file.txt")),
	})
	if f.Error != nil {
		t.Fatalf("fs/readFile: %s", f.Error.Message)
	}
	var readResp struct {
		DataBase64 string `json:"dataBase64"`
	}
	if err := json.Unmarshal(f.Result, &readResp); err != nil {
		t.Fatal(err)
	}
	data, _ := base64.StdEncoding.DecodeString(readResp.DataBase64)
	if string(data) != content {
		t.Errorf("read back %q", data)
	}

	// A path outside the workspace is a target path, verbatim.
	f = env.client.call(t, "fs/writeFile", map[string]any{
		"path":       pathToURI(filepath.Join(env.root, "direct.txt")),
		"dataBase64": base64.StdEncoding.EncodeToString([]byte("x")),
	})
	if f.Error != nil {
		t.Fatalf("fs/writeFile direct: %s", f.Error.Message)
	}
	if _, err := os.Stat(filepath.Join(env.root, "direct.txt")); err != nil {
		t.Errorf("verbatim target path was not honoured: %v", err)
	}

	// Metadata.
	f = env.client.call(t, "fs/getMetadata", map[string]any{
		"path": pathToURI(filepath.Join(env.workspace, "sub", "file.txt")),
	})
	var meta struct {
		IsDirectory bool  `json:"isDirectory"`
		IsFile      bool  `json:"isFile"`
		Size        int64 `json:"size"`
	}
	if err := json.Unmarshal(f.Result, &meta); err != nil || !meta.IsFile || meta.IsDirectory {
		t.Fatalf("metadata: %s", f.Result)
	}
	if meta.Size != int64(len(content)) {
		t.Errorf("size = %d, want %d", meta.Size, len(content))
	}

	// Directory listing.
	f = env.client.call(t, "fs/readDirectory", map[string]any{"path": pathToURI(env.workspace)})
	var listing struct {
		Entries []struct {
			FileName    string `json:"fileName"`
			IsDirectory bool   `json:"isDirectory"`
			IsFile      bool   `json:"isFile"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(f.Result, &listing); err != nil {
		t.Fatalf("readDirectory: %v: %s", err, f.Result)
	}
	names := map[string]bool{}
	for _, e := range listing.Entries {
		names[e.FileName] = true
	}
	if !names["sub"] || !names["direct.txt"] {
		t.Errorf("listing = %v", names)
	}

	// Remove.
	f = env.client.call(t, "fs/remove", map[string]any{
		"path": pathToURI(filepath.Join(env.workspace, "sub")),
	})
	if f.Error == nil {
		t.Fatalf("removing a non-empty directory without recursive must fail")
	}
	f = env.client.call(t, "fs/remove", map[string]any{
		"path":      pathToURI(filepath.Join(env.workspace, "sub")),
		"recursive": true,
	})
	if f.Error != nil {
		t.Fatalf("fs/remove recursive: %s", f.Error.Message)
	}
	if _, err := os.Stat(filepath.Join(env.root, "sub")); !os.IsNotExist(err) {
		t.Errorf("sub still exists")
	}
}

func TestFsOpenReadBlockClose(t *testing.T) {
	env := newTestEnv(t)
	env.initialize(t)

	body := "0123456789abcdef"
	if err := os.WriteFile(filepath.Join(env.root, "block.txt"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	f := env.client.call(t, "fs/open", map[string]any{
		"handleId": "h1",
		"path":     pathToURI(filepath.Join(env.workspace, "block.txt")),
	})
	if f.Error != nil {
		t.Fatalf("fs/open: %s", f.Error.Message)
	}
	f = env.client.call(t, "fs/readBlock", map[string]any{"handleId": "h1", "offset": 4, "len": 6})
	var block struct {
		Chunk string `json:"chunk"`
		EOF   bool   `json:"eof"`
	}
	if err := json.Unmarshal(f.Result, &block); err != nil {
		t.Fatalf("fs/readBlock: %v: %s", err, f.Result)
	}
	data, _ := base64.StdEncoding.DecodeString(block.Chunk)
	if string(data) != "456789" {
		t.Errorf("block = %q", data)
	}
	if block.EOF {
		t.Errorf("eof must be false when a full block was returned")
	}

	// Reading past the end reports eof.
	f = env.client.call(t, "fs/readBlock", map[string]any{"handleId": "h1", "offset": 14, "len": 8})
	if err := json.Unmarshal(f.Result, &block); err != nil || !block.EOF {
		t.Errorf("tail read: %s", f.Result)
	}

	f = env.client.call(t, "fs/close", map[string]any{"handleId": "h1"})
	if f.Error != nil {
		t.Fatalf("fs/close: %s", f.Error.Message)
	}
	f = env.client.call(t, "fs/readBlock", map[string]any{"handleId": "h1", "offset": 0, "len": 1})
	if f.Error == nil {
		t.Errorf("readBlock on a closed handle must fail")
	}
}

func TestFsWalkAndCanonicalize(t *testing.T) {
	env := newTestEnv(t)
	env.initialize(t)

	if err := os.MkdirAll(filepath.Join(env.root, "tree", "a", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"tree/top.txt", "tree/a/mid.txt", "tree/a/b/deep.txt"} {
		if err := os.WriteFile(filepath.Join(env.root, p), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	f := env.client.call(t, "fs/walk", map[string]any{
		"path":    pathToURI(filepath.Join(env.workspace, "tree")),
		"options": map[string]any{"maxDepth": 10, "maxDirectories": 100, "maxEntries": 100},
	})
	if f.Error != nil {
		t.Fatalf("fs/walk: %s", f.Error.Message)
	}
	var outcome struct {
		Entries []struct {
			Path string `json:"path"`
			Kind string `json:"kind"`
		} `json:"entries"`
		Truncated bool `json:"truncated"`
	}
	if err := json.Unmarshal(f.Result, &outcome); err != nil {
		t.Fatalf("walk result: %v: %s", err, f.Result)
	}
	got := map[string]string{}
	for _, e := range outcome.Entries {
		got[e.Path] = e.Kind
	}
	for _, p := range []string{"tree/a", "tree/a/b"} {
		if got[pathToURI(filepath.Join(env.root, p))] != "directory" {
			t.Errorf("walk missing directory %s: %v", p, got)
		}
	}
	for _, p := range []string{"tree/top.txt", "tree/a/mid.txt", "tree/a/b/deep.txt"} {
		if got[pathToURI(filepath.Join(env.root, p))] != "file" {
			t.Errorf("walk missing file %s: %v", p, got)
		}
	}
	if outcome.Truncated {
		t.Errorf("walk of 6 entries must not truncate")
	}

	// Depth bound: maxDepth 1 sees the root's children only.
	f = env.client.call(t, "fs/walk", map[string]any{
		"path":    pathToURI(filepath.Join(env.workspace, "tree")),
		"options": map[string]any{"maxDepth": 1, "maxDirectories": 100, "maxEntries": 100},
	})
	if err := json.Unmarshal(f.Result, &outcome); err != nil {
		t.Fatal(err)
	}
	for _, e := range outcome.Entries {
		if strings.Contains(strings.TrimPrefix(e.Path, pathToURI(filepath.Join(env.root, "tree"))+"/"), "/") {
			t.Errorf("maxDepth 1 returned nested entry %s", e.Path)
		}
	}

	// Canonicalize resolves on the target: /var vs /private/var on macOS is
	// the local proof the answer came from resolution, not string cleaning.
	f = env.client.call(t, "fs/canonicalize", map[string]any{
		"path": pathToURI(filepath.Join(env.workspace, "tree", "a", "..")),
	})
	if f.Error != nil {
		t.Fatalf("fs/canonicalize: %s", f.Error.Message)
	}
	var canon struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(f.Result, &canon); err != nil {
		t.Fatal(err)
	}
	resolved, _ := filepath.EvalSymlinks(filepath.Join(env.root, "tree"))
	if canon.Path != pathToURI(resolved) && canon.Path != pathToURI(filepath.Join(env.root, "tree")) {
		t.Errorf("canonicalize = %q, want %q", canon.Path, pathToURI(resolved))
	}
}

func TestFsCopyStaysOnTheTarget(t *testing.T) {
	env := newTestEnv(t)
	env.initialize(t)

	if err := os.WriteFile(filepath.Join(env.root, "src.txt"), []byte("copy me"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := env.client.call(t, "fs/copy", map[string]any{
		"sourcePath":      pathToURI(filepath.Join(env.workspace, "src.txt")),
		"destinationPath": pathToURI(filepath.Join(env.workspace, "dst.txt")),
		"recursive":       false,
	})
	if f.Error != nil {
		t.Fatalf("fs/copy: %s", f.Error.Message)
	}
	data, err := os.ReadFile(filepath.Join(env.root, "dst.txt"))
	if err != nil || string(data) != "copy me" {
		t.Errorf("copy result: %v %q", err, data)
	}
}

func TestMapPath(t *testing.T) {
	s := &Server{workspace: "/Users/boj/project", root: "/home/ubuntu/reach-verify"}
	for _, tc := range []struct{ in, want string }{
		{"/Users/boj/project", "/home/ubuntu/reach-verify"},
		{"/Users/boj/project/src/main.go", "/home/ubuntu/reach-verify/src/main.go"},
		{"/home/ubuntu/reach-verify/file.txt", "/home/ubuntu/reach-verify/file.txt"},
		{"/etc/hostname", "/etc/hostname"},
		// A sibling whose name shares a prefix is not beneath the workspace.
		{"/Users/boj/project-other/x", "/Users/boj/project-other/x"},
		{"relative/file.txt", "/home/ubuntu/reach-verify/relative/file.txt"},
	} {
		if got := s.mapPath(tc.in); got != tc.want {
			t.Errorf("mapPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestUriRoundTrip(t *testing.T) {
	p, rerr := uriToPath("file:///home/ubuntu/some%20dir/f.txt")
	if rerr != nil {
		t.Fatalf("uriToPath: %v", rerr)
	}
	if p != "/home/ubuntu/some dir/f.txt" {
		t.Errorf("uriToPath decoded %q", p)
	}
	if _, rerr := uriToPath("/plain/path"); rerr == nil {
		t.Errorf("a native path must be rejected — codex speaks PathUri")
	}
	if got := pathToURI("/home/ubuntu/some dir/f.txt"); got != "file:///home/ubuntu/some%20dir/f.txt" {
		t.Errorf("pathToURI = %q", got)
	}
}

func TestEnvironmentsTOMLShape(t *testing.T) {
	toml := EnvironmentsTOML("/usr/local/bin/reach", "rtx")
	for _, want := range []string{
		`default = "reach"`,
		`include_local = false`,
		`id = "reach"`,
		`program = "/usr/local/bin/reach"`,
		`args = ["exec-server"]`,
		`REACH_SESSION = "rtx"`,
	} {
		if !strings.Contains(toml, want) {
			t.Errorf("environments.toml missing %q:\n%s", want, toml)
		}
	}
}
