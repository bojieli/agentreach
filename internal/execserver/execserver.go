// Package execserver speaks Codex's exec-server protocol (JSON-RPC over stdio,
// newline-delimited) and backs every method with a reach session's target.
//
// Codex 0.148 resolves its shell by absolute path, which no PATH shim can
// intercept — but it also grew a remote-environment seam: environments.toml in
// CODEX_HOME names a program codex spawns and then treats as the machine its
// tools act on. reach is that program. Every shell command, file read, write,
// directory listing and walk the agent performs arrives here as a JSON-RPC
// request and is executed on the session's target through the same transport
// and file-operation tiers as `reach exec` and `reach fs`.
//
// The governing rule applies unchanged: a missing or broken session fails
// loudly, and every target failure is returned as a JSON-RPC error value the
// agent can reason about. Nothing here ever falls back to local execution.
package execserver

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"github.com/bojieli/agentreach/internal/audit"
	"github.com/bojieli/agentreach/internal/fileops"
	"github.com/bojieli/agentreach/internal/session"
	"github.com/bojieli/agentreach/internal/transport"
	"github.com/bojieli/agentreach/internal/reach"
)

// JSON-RPC error codes, matching codex's own exec-server (rpc.rs).
const (
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternal       = -32603
)

// rpcError is a protocol-level failure, returned to the client as a JSON-RPC
// error value rather than a process crash.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return e.Message }

func invalidRequest(format string, args ...any) *rpcError {
	return &rpcError{Code: codeInvalidRequest, Message: fmt.Sprintf(format, args...)}
}

func invalidParams(format string, args ...any) *rpcError {
	return &rpcError{Code: codeInvalidParams, Message: fmt.Sprintf(format, args...)}
}

func internalError(format string, args ...any) *rpcError {
	return &rpcError{Code: codeInternal, Message: fmt.Sprintf(format, args...)}
}

// request is one inbound JSON-RPC message. The Codex dialect omits the
// "jsonrpc": "2.0" field, and the id may be a string or an integer — it is
// echoed back verbatim either way.
type request struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// Server is one exec-server connection bound to a reach session.
type Server struct {
	sess      *session.Session
	t         transport.Transport
	ops       fileops.FileOps
	workspace string // the local directory codex was launched in
	root      string // the session's root on the target

	shellName string
	shellPath string
	sessionID string

	out     io.Writer
	writeMu sync.Mutex

	mu           sync.Mutex // guards sawInitialize, processes and handles
	sawInitialize bool
	processes    map[string]*process
	handles      map[string]string
}

// New builds an exec-server for sess. workspace is the local directory codex
// was launched in; paths under it are mapped onto the session's root on the
// target, and every other absolute path is treated as a target path verbatim.
//
// The target's shell is probed eagerly: codex asks for it in environment/info
// and builds every command with it, and a target without a POSIX shell is
// better reported to the operator at startup than as a failed tool call
// mid-turn.
func New(ctx context.Context, sess *session.Session, workspace string) (*Server, error) {
	t, err := sess.Transport()
	if err != nil {
		return nil, err
	}
	sel, err := sess.FileOps(ctx, t)
	if err != nil {
		return nil, err
	}
	name, shellPath, err := probeShell(ctx, t)
	if err != nil {
		_ = sel.Ops.Close()
		return nil, err
	}
	if workspace == "" {
		workspace, _ = os.Getwd()
	}
	return &Server{
		sess:      sess,
		t:         t,
		ops:       sel.Ops,
		workspace: filepath.Clean(workspace),
		root:      sess.Target.Workspace,
		shellName: name,
		shellPath: shellPath,
		sessionID: "reach-" + randomHex(8),
		processes: map[string]*process{},
		handles:   map[string]string{},
	}, nil
}

// probeShell asks the target which POSIX shell it has. bash is preferred —
// codex derives its command argv from the reported shell, and bash is what its
// Linux environments report — with sh as the floor reach itself requires.
func probeShell(ctx context.Context, t transport.Transport) (name, path string, err error) {
	res, err := t.Run(ctx, shellRequest("for sh in bash sh; do p=$(command -v $sh 2>/dev/null) && { echo \"$sh $p\"; exit 0; }; done; exit 1"))
	if err != nil {
		return "", "", fmt.Errorf("probe the target's shell: %w", err)
	}
	if res.Code != 0 {
		return "", "", fmt.Errorf(
			"the target has neither bash nor sh on PATH. reach's floor is a POSIX shell —\n" +
				"there is no environment this exec-server can honestly report")
	}
	fields := strings.Fields(strings.TrimSpace(string(res.Stdout)))
	if len(fields) != 2 {
		return "", "", fmt.Errorf("unexpected answer probing the target's shell: %q", strings.TrimSpace(string(res.Stdout)))
	}
	return fields[0], fields[1], nil
}

func shellRequest(command string) reach.ExecRequest {
	return reach.ExecRequest{Command: command, MaxOutput: 4 << 10}
}

// Serve reads newline-delimited JSON-RPC messages from in until EOF and writes
// responses and notifications to out. Stderr is free for logging; stdout
// carries only protocol frames.
//
// Requests are dispatched concurrently: process/read long-polls while other
// requests (process/write, fs/*) must still be answered, so a serial dispatch
// would deadlock against unified_exec sessions. Shared state is mutex-guarded;
// the transport multiplexes concurrent target operations on its own.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	s.out = out
	sc := bufio.NewScanner(in)
	// fs/readFile answers can carry several MiB of base64 in one frame; the
	// default 64 KiB token limit would silently truncate the protocol stream.
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	var wg sync.WaitGroup
	for sc.Scan() {
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			// A frame that does not parse carries no id to answer to. Codex's
			// own server logs and drops such lines; so do we.
			fmt.Fprintf(os.Stderr, "reach exec-server: dropping unparseable frame: %v\n", err)
			continue
		}
		if len(req.ID) == 0 {
			s.notify0(req.Method, req.Params)
			continue
		}
		if req.Method == "initialize" || req.Method == "process/start" {
			// Answered synchronously: later requests address these by name
			// (the handshake, the processId), and a pipelined client must
			// never see "unknown process id" for a start it already sent.
			s.dispatch(ctx, req)
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.dispatch(ctx, req)
		}()
	}
	wg.Wait()
	// The client is gone. Codex's own server terminates child processes on
	// shutdown; leaving an agent's build running after the harness exits
	// would be exactly the orphaned-process failure the no-daemon design
	// exists to avoid.
	s.mu.Lock()
	running := make([]*process, 0, len(s.processes))
	for _, p := range s.processes {
		running = append(running, p)
	}
	s.mu.Unlock()
	for _, p := range running {
		p.mu.Lock()
		exited := p.exited
		p.mu.Unlock()
		if !exited {
			s.killStream(p)
		}
	}
	return sc.Err()
}

// notify0 handles a client notification (a message with no id). The only one
// the protocol defines inbound is `initialized`.
func (s *Server) notify0(method string, _ json.RawMessage) {
	if method == "initialized" {
		// Handshake complete. State lives in sawInitialize, set by initialize
		// itself; the notification carries nothing the server needs.
		return
	}
}

// send writes one protocol frame. All writes to stdout pass through here so a
// notification and a response can never interleave mid-line.
func (s *Server) send(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		fmt.Fprintf(os.Stderr, "reach exec-server: cannot encode frame: %v\n", err)
		return
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if _, err := s.out.Write(append(data, '\n')); err != nil {
		fmt.Fprintf(os.Stderr, "reach exec-server: cannot write frame: %v\n", err)
	}
}

func (s *Server) respond(id json.RawMessage, result any) {
	s.send(map[string]any{"id": id, "result": result})
}

func (s *Server) respondError(id json.RawMessage, e *rpcError) {
	s.send(map[string]any{"id": id, "error": e})
}

// notify sends a server->client notification (process/output, process/exited,
// process/closed).
func (s *Server) notify(method string, params any) {
	s.send(map[string]any{"method": method, "params": params})
}

// dispatch routes one request. Every handler returns either a result value or
// a *rpcError; a panic in a handler is converted to an internal error rather
// than allowed to kill the connection the agent is working over.
func (s *Server) dispatch(ctx context.Context, req request) {
	var result any
	var rerr *rpcError
	func() {
		defer func() {
			if r := recover(); r != nil {
				rerr = internalError("internal error: %v", r)
			}
		}()
		result, rerr = s.handle(ctx, req.Method, req.Params)
	}()
	if rerr != nil {
		s.respondError(req.ID, rerr)
		return
	}
	s.respond(req.ID, result)
}

func (s *Server) handle(ctx context.Context, method string, params json.RawMessage) (any, *rpcError) {
	if method == "initialize" {
		return s.handleInitialize(params)
	}
	if rerr := s.requireInitialized(); rerr != nil {
		return nil, rerr
	}
	switch method {
	case "environment/info":
		return s.handleEnvironmentInfo()
	case "environment/status":
		return map[string]any{"status": "ready"}, nil
	case "process/start":
		return s.handleProcessStart(ctx, params)
	case "process/read":
		return s.handleProcessRead(params)
	case "process/write":
		return s.handleProcessWrite(params)
	case "process/signal":
		return s.handleProcessSignal(params)
	case "process/terminate":
		return s.handleProcessTerminate(params)
	case "fs/readFile":
		return s.handleFsReadFile(ctx, params)
	case "fs/writeFile":
		return s.handleFsWriteFile(ctx, params)
	case "fs/createDirectory":
		return s.handleFsCreateDirectory(ctx, params)
	case "fs/getMetadata":
		return s.handleFsGetMetadata(ctx, params)
	case "fs/canonicalize":
		return s.handleFsCanonicalize(ctx, params)
	case "fs/readDirectory":
		return s.handleFsReadDirectory(ctx, params)
	case "fs/walk":
		return s.handleFsWalk(ctx, params)
	case "fs/remove":
		return s.handleFsRemove(ctx, params)
	case "fs/copy":
		return s.handleFsCopy(ctx, params)
	case "fs/open":
		return s.handleFsOpen(params)
	case "fs/readBlock":
		return s.handleFsReadBlock(ctx, params)
	case "fs/close":
		return s.handleFsClose(params)
	case "capabilityRoots/discoverV1":
		return s.handleCapabilityDiscover(params)
	case "http/request":
		// reach does not proxy the local network on the target's behalf. The
		// error is a value codex can route around, per the governing rule.
		return nil, internalError("http/request is not supported by the reach exec-server")
	}
	return nil, &rpcError{Code: codeMethodNotFound, Message: "unknown method " + method}
}

// requireInitialized enforces the handshake order codex's own server enforces:
// initialize once, everything else only after it. The `initialized`
// notification itself carries no state the server needs.
func (s *Server) requireInitialized() *rpcError {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.sawInitialize {
		return invalidRequest("client must call initialize before using methods")
	}
	return nil
}

// --- initialize / environment ---

type initializeParams struct {
	ClientName string `json:"clientName"`
}

func (s *Server) handleInitialize(raw json.RawMessage) (any, *rpcError) {
	var p initializeParams
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, invalidParams("initialize: %v", err)
		}
	}
	s.mu.Lock()
	s.sawInitialize = true
	s.mu.Unlock()
	return map[string]any{"sessionId": s.sessionID}, nil
}

func (s *Server) handleEnvironmentInfo() (any, *rpcError) {
	return map[string]any{
		"shell": map[string]any{
			"name": s.shellName,
			"path": s.shellPath,
		},
		"cwd":                  pathToURI(s.root),
		"temporaryDirectories": []string{pathToURI("/tmp")},
		"tempDir":              pathToURI("/tmp"),
		// Every optional capability is reported absent: no network proxy, no
		// sandboxed discovery, no environmentConfig/read, no sandboxed file
		// streaming. Codex gates newer request fields on these; false keeps it
		// on the protocol surface this server implements.
		"capabilities": map[string]any{
			"networkProxyLaunch":        false,
			"capabilityDiscoverySandbox": false,
			"environmentConfigRead":      false,
			"sandboxedFileStreaming":     false,
		},
	}, nil
}

// --- path mapping ---

// uriToPath decodes a file:// URI from codex into a plain path. Codex sends
// every filesystem path and cwd as a PathUri; percent-encoding is undone here.
func uriToPath(uri string) (string, *rpcError) {
	u, err := url.Parse(uri)
	if err != nil {
		return "", invalidParams("path %q is not a valid URI: %v", uri, err)
	}
	if u.Scheme != "file" {
		return "", invalidParams("path %q is not a file:// URI", uri)
	}
	p := u.Path
	if p == "" {
		return "", invalidParams("path %q has no path", uri)
	}
	return p, nil
}

// pathToURI renders a target path as the PathUri codex expects back.
func pathToURI(p string) string {
	return (&url.URL{Scheme: "file", Path: p}).String()
}

// mapPath translates a path codex sent into a path on the target.
//
// The session's root on the target is the ground truth: a path equal to or
// beneath the local directory codex was launched in maps to the
// session-root-relative equivalent, and any other absolute path is a target
// path verbatim — the agent thinks in target paths once it is working. A
// relative path is resolved against the session root, mirroring how the
// target's own shell would see it from the workspace.
func (s *Server) mapPath(p string) string {
	if s.workspace != "" && s.workspace != string(filepath.Separator) {
		if p == s.workspace {
			return s.root
		}
		if strings.HasPrefix(p, s.workspace+string(filepath.Separator)) {
			rel := p[len(s.workspace)+1:]
			// Local separators are the operator's; the target is POSIX.
			return path.Join(s.root, filepath.ToSlash(rel))
		}
	}
	if strings.HasPrefix(p, "/") {
		return path.Clean(p)
	}
	return path.Join(s.root, p)
}

// mapURI decodes a PathUri and maps it onto the target in one step.
func (s *Server) mapURI(uri string) (string, *rpcError) {
	p, err := uriToPath(uri)
	if err != nil {
		return "", err
	}
	return s.mapPath(p), nil
}

// --- audit ---

// record appends one entry to the session's audit log, exactly as the exec and
// fs paths do. The path or command recorded is the one the agent asked about,
// not the mapping or wrapper reach derived from it.
func (s *Server) record(e audit.Entry) {
	dir, err := session.Dir()
	if err != nil {
		return
	}
	e.Target = s.sess.Target.Describe()
	audit.Append(dir, s.sess.Name, e)
}

// operationContext bounds one target operation with the session's timeout.
func (s *Server) operationContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return s.sess.OperationContext(ctx)
}

// Close releases the file-operation strategy and the transport.
func (s *Server) Close() error {
	err := s.ops.Close()
	if cerr := s.t.Close(); err == nil {
		err = cerr
	}
	return err
}

// EnvironmentsTOML renders the environments.toml that points codex at this
// reach binary as its exec-server. It is shared by `reach codex` (managed
// CODEX_HOME) and the offline harness probe (throwaway CODEX_HOME), so the two
// can never drift into different shapes.
func EnvironmentsTOML(reachPath, sessName string) string {
	var b strings.Builder
	b.WriteString("default = \"reach\"\ninclude_local = false\n\n")
	b.WriteString("[[environments]]\n")
	b.WriteString("id = \"reach\"\n")
	b.WriteString("program = " + tomlString(reachPath) + "\n")
	b.WriteString("args = [\"exec-server\"]\n")
	b.WriteString("env = { REACH_SESSION = " + tomlString(sessName) + " }\n")
	return b.String()
}

// tomlString renders s as a TOML basic string.
func tomlString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		case '\r':
			b.WriteString(`\r`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\u%04X`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// randomHex returns n random bytes, hex-encoded.
func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "fallback"
	}
	return hex.EncodeToString(buf)
}
