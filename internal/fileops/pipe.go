package fileops

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bojieli/agentreach/internal/transport"
	"github.com/bojieli/agentreach/internal/reach"
)

//go:embed handler.py
var pipeHandler string

// bootstrap is the entire command line reach runs on the target for this tier.
//
// The obvious form — `python3 -` with the program on stdin — cannot work: the
// interpreter reads its source from stdin until EOF, leaving no stdin for the
// protocol to use afterwards. So reach sends the program as a single base64
// line, which a one-liner decodes and executes, and everything after that first
// newline is the protocol. Nothing touches the target's disk either way.
//
// `exec` replaces the shell with the interpreter, so closing the channel kills
// the handler instead of leaving an orphan behind on someone else's machine.
const bootstrap = `import sys,base64;exec(compile(base64.b64decode(sys.stdin.buffer.readline()),'<reach>','exec'))`

// maxFrame bounds a single protocol frame in both directions.
//
// One frame carries a whole file, which is what makes this tier fast, but it
// also means a request for a huge file would otherwise be a request to allocate
// it entirely in memory on both sides. Reads above this size are chunked;
// writes above it are refused with a clear message rather than attempted.
const maxFrame = 64 << 20

// readChunkPipe is how much one read op fetches. It is far larger than tier 0's
// chunk because there is no base64 expansion and no per-chunk process spawn.
const readChunkPipe = 8 << 20

// handlerStartTimeout bounds the first request, which doubles as the handshake,
// so a target that accepts the channel and then never answers fails as a value
// instead of hanging.
const handlerStartTimeout = 20 * time.Second

// stderrGrace bounds how long a failure waits for the far end's own complaint
// before reporting without it. The failure and the explanation for it arrive on
// two different pipes, and the explanation is usually a moment behind.
const stderrGrace = 500 * time.Millisecond

// contextWithAtLeast returns a context with at least d remaining.
func contextWithAtLeast(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) >= d {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, d)
}

// handlerOps implements FileOps by keeping one long-lived process alive on the
// target and speaking a framed protocol to it.
//
// The pipe and helper tiers differ only in which program is on the other end —
// a Python interpreter holding a script that was never written to disk, or a
// small installed binary — so they share this client entirely. A protocol difference
// between the two would be a second implementation to keep honest, and the
// conformance suite would have to prove the same properties twice.
//
// File content travels as a raw payload beside the JSON header rather than
// inside it, so binary files cross the wire unencoded — no base64 expansion,
// and no possibility of a NUL byte or invalid UTF-8 being mangled by a text
// codec on the way.
//
// Requests are serialised. The channel is a single duplex stream with ordered
// framing, and the throughput win here comes from not spawning a process per
// operation, not from overlapping requests within one session.
type handlerOps struct {
	base *POSIX
	tier reach.Tier
	// label names the program on the other end, for error messages an operator
	// has to act on: "the python handler died" and "the agent died" call for
	// different next steps.
	label string

	// t, command and preamble are everything it takes to run this program again
	// on a new channel. They are kept so that a stream broken mid-session can be
	// replaced rather than ending file access for the rest of that session. See
	// reopen.
	t        transport.Transport
	command  string
	preamble string

	// stderr collects whatever the program on the far end complains about, for
	// the error message produced if it turns out never to have started.
	stderr *safeBuffer
	// stderrDone is closed once that program's stderr reaches EOF, which is how
	// a failure knows whether the complaint explaining it has arrived yet.
	stderrDone <-chan struct{}
	// startTimeout bounds the first request, which doubles as the handshake.
	startTimeout time.Duration

	mu sync.Mutex
	in *bufio.Reader
	// first is true until a request has been answered. Until then a failure
	// means "the program never started", which needs a different explanation
	// from "the connection dropped mid-session".
	first bool
	out   io.Writer
	id    uint32
	// broken records that the stream can no longer be trusted to be in sync, so
	// the request that discovered it fails rather than reading a stale response
	// as the answer to a new one. The next request starts a new program instead
	// of inheriting the verdict; see reopen.
	broken error
	// proven records that the program currently on the other end has answered at
	// least once. Only a program that has worked is worth starting again.
	proven bool

	// streamMu guards stream alone and is never held across I/O, so Close can
	// end a stream that a request is blocked reading.
	streamMu sync.Mutex
	stream   transport.Stream
	// closed records an explicit Close, after which nothing is reopened.
	closed atomic.Bool
}

// NewPipe starts the Python handler on the target and verifies it answers.
func NewPipe(ctx context.Context, t transport.Transport, base *POSIX) (FileOps, error) {
	cmd := fmt.Sprintf("exec python3 -c %s", transport.ShellQuote(bootstrap))
	return startHandler(ctx, t, base, reach.TierPipe, "python handler", cmd,
		base64.StdEncoding.EncodeToString([]byte(pipeHandler))+"\n")
}

// startHandler opens a channel and runs a program on it, without waiting to
// hear back.
//
// It used to send a ping and wait for the pong, so that a program which failed
// to start was reported at once rather than at the first file operation. That
// was the right instinct and the wrong mechanism: it cost a round trip on every
// tool call, because reach starts a fresh process per call and so paid the
// handshake every time. Two round trips for one file read, on a tier whose
// entire argument is that it needs only one.
//
// The first real request is the handshake instead. It carries the same
// diagnosis — the captured stderr, the program's name, the timeout — so a
// target where nothing started still says so, in the same words, one round trip
// sooner.
func startHandler(ctx context.Context, t transport.Transport, base *POSIX, tier reach.Tier, label, command, preamble string) (FileOps, error) {
	p := &handlerOps{
		base:         base,
		tier:         tier,
		label:        label,
		t:            t,
		command:      command,
		preamble:     preamble,
		startTimeout: handlerStartTimeout,
	}
	if err := p.open(ctx); err != nil {
		return nil, err
	}
	return p, nil
}

// open runs the program on a fresh channel and installs it as this type's
// protocol state. The caller holds mu, or has not yet published the value.
func (p *handlerOps) open(ctx context.Context) error {
	// The channel outlives this call, so it must not be tied to a context that
	// is cancelled once this function returns.
	stream, err := p.t.Open(context.WithoutCancel(ctx), p.command)
	if err != nil {
		return fmt.Errorf("start %s: %w", p.label, err)
	}

	errBuf := &safeBuffer{remaining: 8 << 10}
	errDone := make(chan struct{})
	go func() {
		defer close(errDone)
		_, _ = io.Copy(errBuf, stream.Stderr)
	}()

	if p.preamble != "" {
		if _, err := io.WriteString(stream.Stdin, p.preamble); err != nil {
			_ = stream.Close()
			return fmt.Errorf("send %s: %w", p.label, err)
		}
	}

	p.streamMu.Lock()
	p.stream = stream
	p.streamMu.Unlock()

	p.stderr, p.stderrDone = errBuf, errDone
	p.in = bufio.NewReaderSize(stream.Stdout, 1<<16)
	p.out = stream.Stdin
	p.first, p.id, p.broken, p.proven = true, 0, nil, false
	return nil
}

// reopen replaces a stream that broke mid-session with the same program started
// again on a new channel. The caller holds mu.
//
// Breaking the stream on an abandoned or half-written request is what stops a
// stale response from being read as the answer to the next one, but that
// verdict used to be permanent. Under `reach exec-server`, where one handler
// serves an entire agent session, a single cancelled file operation ended file
// access until the harness was restarted — the error said "must be restarted"
// and nothing restarted it.
//
// The request that discovered the break still fails. Whether it reached the far
// end is genuinely unknown, and a `rename` retried on a guess is applied twice.
// Only the requests after it are spared, and they are safe because the protocol
// is stateless: every request carries its own path and offset, and nothing on
// the far end survives between them, so a newly started program is
// indistinguishable from the one it replaced.
func (p *handlerOps) reopen(ctx context.Context) error {
	if p.closed.Load() {
		return p.broken
	}
	// A program that never answered is not worth starting again: it would
	// respawn a doomed process on every call only to report the same failure a
	// round trip later. open clears proven, so one restart is all a working
	// program earns before it has to prove itself again.
	//
	// The exception is a program that never started because the connection had
	// no room for its channel. That is the connection's fault rather than the
	// program's — sshd caps concurrent sessions per connection, and reach runs
	// one channel per tool call — so it is worth one more try on another
	// connection. Overflow is bounded, so this cannot loop.
	if !p.proven && !p.refusedAChannel() {
		return p.broken
	}
	p.closeStream()

	broken := p.broken
	if err := p.open(ctx); err != nil {
		p.broken = broken
		return fmt.Errorf("%w; starting it again failed too: %v", broken, err)
	}
	return nil
}

// safeBuffer is a bounded buffer written by one goroutine and read by another.
//
// The bound stops a chatty or hostile program from growing reach's memory
// through a diagnostic buffer; the mutex is because the writer is an io.Copy in
// its own goroutine and the reader is whichever request fails first.
type safeBuffer struct {
	mu        sync.Mutex
	b         strings.Builder
	remaining int
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.remaining <= 0 {
		return len(p), nil
	}
	if len(p) > s.remaining {
		p = p[:s.remaining]
	}
	n, err := s.b.Write(p)
	s.remaining -= n
	return len(p), err
}

func (s *safeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// Tier implements FileOps.
func (p *handlerOps) Tier() reach.Tier { return p.tier }

// Close implements FileOps, ending the handler process. It also bars reopen, so
// a Close racing an in-flight request cannot be undone by that request's
// recovery path.
func (p *handlerOps) Close() error {
	if p.closed.Swap(true) {
		return nil
	}
	p.closeStream()
	return nil
}

// closeStream ends whichever stream is current. It takes only streamMu and
// never holds it across I/O, so it can end a stream that a request is blocked
// reading — which is how Close unblocks one.
func (p *handlerOps) closeStream() {
	p.streamMu.Lock()
	stream := p.stream
	p.streamMu.Unlock()
	if stream.Close != nil {
		_ = stream.Close()
	}
}

// roundTrip sends one request and waits for its response, honouring ctx.
//
// The cancellation path deliberately kills the channel rather than returning
// and leaving it open. The protocol is a strict request/response sequence over
// one stream: abandoning a response mid-flight would leave it in the pipe to be
// read as the answer to the *next* request, which for a file read means
// returning one file's bytes for another — a silent wrong answer, which is the
// one failure mode this project treats as unacceptable. Closing converts it
// into an error every later call reports.
func (p *handlerOps) roundTrip(ctx context.Context, req map[string]any, payload []byte) (map[string]any, []byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.broken != nil {
		if err := p.reopen(ctx); err != nil {
			return nil, nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	// The first request is also the handshake, so it gets at least the time a
	// program needs to start — a caller's tight deadline should not be read as
	// "python3 is missing".
	if p.first {
		var cancel context.CancelFunc
		ctx, cancel = contextWithAtLeast(ctx, p.startTimeout)
		defer cancel()
	}

	p.id++
	req["id"] = p.id

	hdr, err := json.Marshal(req)
	if err != nil {
		return nil, nil, err
	}
	if len(payload) > maxFrame {
		return nil, nil, fmt.Errorf("payload of %d bytes exceeds this tier's %d-byte frame limit", len(payload), maxFrame)
	}

	var frame []byte
	frame = binary.BigEndian.AppendUint32(frame, uint32(len(hdr)))
	frame = append(frame, hdr...)
	frame = binary.BigEndian.AppendUint32(frame, uint32(len(payload)))
	if _, err := p.out.Write(frame); err != nil {
		return nil, nil, p.writeFailed(fmt.Errorf("write request: %w", err))
	}
	if len(payload) > 0 {
		if _, err := p.out.Write(payload); err != nil {
			return nil, nil, p.writeFailed(fmt.Errorf("write payload: %w", err))
		}
	}

	type response struct {
		hdr     map[string]any
		payload []byte
		err     error
	}
	done := make(chan response, 1)
	// The reader is captured rather than read from the field inside the
	// goroutine. A request abandoned on cancellation leaves this goroutine
	// blocked on the old stream, and reopen replaces the field underneath it;
	// reading it there would be a data race against that replacement.
	in := p.in
	go func() {
		hdr, body, err := p.readFrame(in)
		done <- response{hdr, body, err}
	}()

	select {
	case r := <-done:
		if r.err != nil {
			p.broken = r.err
			return nil, nil, p.startupError(r.err)
		}
		p.first, p.proven = false, true
		if ok, _ := r.hdr["ok"].(bool); !ok {
			return nil, nil, pipeError(r.hdr, req)
		}
		return r.hdr, r.payload, nil
	case <-ctx.Done():
		p.broken = fmt.Errorf("the %s was abandoned mid-operation (%w); the next file operation starts a new one",
			p.label, ctx.Err())
		p.closeStream()
		return nil, nil, p.startupError(ctx.Err())
	}
}

// refusedAChannel reports whether this program failed to start because the
// target would not give it a channel, and moves the transport to another
// connection when it did. The caller holds mu.
func (p *handlerOps) refusedAChannel() bool {
	o, ok := p.t.(transport.Overflower)
	if !ok {
		return false
	}
	if !transport.IsChannelOpenFailure(p.stderr.String()) &&
		(p.broken == nil || !transport.IsChannelOpenFailure(p.broken.Error())) {
		return false
	}
	return o.Overflow()
}

// startupError explains a failure that happened before the program ever
// answered, which is a different problem from one that happened later.
//
// Before: "the python handler exited". After: the same, plus whatever it wrote
// to stderr on the way out, which is usually the whole answer — no python3, a
// read-only cache directory, a binary for the wrong architecture.
func (p *handlerOps) startupError(cause error) error {
	if !p.first {
		return cause
	}
	// That complaint is usually the entire answer — no python3, a helper built
	// for the wrong architecture, a cache directory that is not writable — and
	// it arrives on a different pipe from the failure being reported here.
	// Waiting for that pipe to close makes the explanation part of the error
	// instead of a race against it. The wait is bounded because a program that
	// started perfectly well and merely stopped answering never closes it.
	select {
	case <-p.stderrDone:
	case <-time.After(stderrGrace):
	}
	if msg := strings.TrimSpace(p.stderr.String()); msg != "" {
		return fmt.Errorf("%s did not start: %w (%s)", p.label, cause, firstLine(msg))
	}
	return fmt.Errorf("%s did not start: %w", p.label, cause)
}

// writeFailed reports a request that could not be handed to the program on the
// other end, and takes the stream out of service.
//
// A program that never started fails its first request at whichever end of that
// request runs first, and which one that is comes down to scheduling: if it is
// already gone the write gets EPIPE, and if it dies a moment later the write
// lands in the pipe buffer and the read gets EOF. Only the read path carried
// the startup diagnosis, so the same broken target explained itself either as
// "python handler did not start (python3: not found)" or as "write request:
// write |1: broken pipe" — the second of which names neither the program nor
// the reason, and is what an operator saw about a third of the time.
//
// The failure also leaves the stream unusable, which the old code did not
// record. A frame that was partly written leaves the far end waiting for the
// rest of it, so the next request would be read as this one's tail and answered
// against the wrong header — a read returning another file's bytes, which is
// the failure this type is arranged throughout to prevent. The stream is taken
// out of service instead, and reopen gives the next request a new one.
func (p *handlerOps) writeFailed(cause error) error {
	err := p.startupError(cause)
	p.broken = fmt.Errorf("the %s stream was left mid-request (%w); the next file operation starts a new one",
		p.label, cause)
	return err
}

func (p *handlerOps) readFrame(in *bufio.Reader) (map[string]any, []byte, error) {
	hdrLen, err := p.readUint32(in)
	if err != nil {
		return nil, nil, err
	}
	if hdrLen > maxFrame {
		return nil, nil, fmt.Errorf("handler sent a %d-byte header, above the %d-byte limit", hdrLen, maxFrame)
	}
	hdrBuf := make([]byte, hdrLen)
	if _, err := io.ReadFull(in, hdrBuf); err != nil {
		return nil, nil, fmt.Errorf("read response header: %w", err)
	}
	var hdr map[string]any
	if err := json.Unmarshal(hdrBuf, &hdr); err != nil {
		return nil, nil, fmt.Errorf("parse response header: %w", err)
	}
	payloadLen, err := p.readUint32(in)
	if err != nil {
		return nil, nil, err
	}
	if payloadLen > maxFrame {
		return nil, nil, fmt.Errorf("handler sent a %d-byte payload, above the %d-byte limit", payloadLen, maxFrame)
	}
	payload := make([]byte, payloadLen)
	if payloadLen > 0 {
		if _, err := io.ReadFull(in, payload); err != nil {
			return nil, nil, fmt.Errorf("read response payload: %w", err)
		}
	}
	return hdr, payload, nil
}

func (p *handlerOps) readUint32(in *bufio.Reader) (uint32, error) {
	var buf [4]byte
	if _, err := io.ReadFull(in, buf[:]); err != nil {
		if err == io.EOF {
			return 0, fmt.Errorf("the %s on the target exited", p.label)
		}
		return 0, err
	}
	return binary.BigEndian.Uint32(buf[:]), nil
}

// pipeError converts a handler failure into reach's error vocabulary.
func pipeError(hdr map[string]any, req map[string]any) error {
	msg, _ := hdr["error"].(string)
	kind, _ := hdr["kind"].(string)
	if kind == "notfound" {
		path, _ := req["path"].(string)
		return &reach.NotFoundError{Path: path}
	}
	if msg == "" {
		msg = "the handler reported a failure with no message"
	}
	return fmt.Errorf("%s", msg)
}

// Read implements FileOps.
func (p *handlerOps) Read(ctx context.Context, filePath string, off, n int64) ([]byte, error) {
	if off < 0 {
		off = 0
	}
	var out []byte
	for {
		want := n - int64(len(out))
		if n <= 0 {
			want = readChunkPipe
		} else if want > readChunkPipe {
			want = readChunkPipe
		}
		if n > 0 && want <= 0 {
			break
		}
		_, payload, err := p.roundTrip(ctx, map[string]any{
			"op":     "read",
			"path":   filePath,
			"offset": off + int64(len(out)),
			"limit":  want,
		}, nil)
		if err != nil {
			return nil, err
		}
		out = append(out, payload...)
		if int64(len(payload)) < want {
			break // end of file
		}
	}
	return out, nil
}

// Write implements FileOps.
func (p *handlerOps) Write(ctx context.Context, filePath string, data []byte, mode fs.FileMode) error {
	if mode == 0 {
		mode = 0o644
	}
	if len(data) > maxFrame {
		return fmt.Errorf("refusing to write %d bytes in one frame (limit %d); use the shell for files this large", len(data), maxFrame)
	}
	_, _, err := p.roundTrip(ctx, map[string]any{
		"op": "write", "path": filePath, "mode": int(mode.Perm()),
	}, data)
	return err
}

// Stat implements FileOps.
func (p *handlerOps) Stat(ctx context.Context, filePath string) (*reach.FileInfo, error) {
	hdr, _, err := p.roundTrip(ctx, map[string]any{"op": "stat", "path": filePath}, nil)
	if err != nil {
		return nil, err
	}
	raw, ok := hdr["info"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("stat %s: handler returned no file info", filePath)
	}
	fi := infoFromMap(raw)
	return &fi, nil
}

// List implements FileOps.
func (p *handlerOps) List(ctx context.Context, dir string) ([]reach.FileInfo, error) {
	hdr, _, err := p.roundTrip(ctx, map[string]any{"op": "list", "path": dir}, nil)
	if err != nil {
		return nil, err
	}
	rawEntries, _ := hdr["entries"].([]any)
	out := make([]reach.FileInfo, 0, len(rawEntries))
	for _, e := range rawEntries {
		if m, ok := e.(map[string]any); ok {
			out = append(out, infoFromMap(m))
		}
	}
	return out, nil
}

// Mkdir implements FileOps.
func (p *handlerOps) Mkdir(ctx context.Context, dir string, mode fs.FileMode) error {
	if mode == 0 {
		mode = 0o755
	}
	_, _, err := p.roundTrip(ctx, map[string]any{"op": "mkdir", "path": dir, "mode": int(mode.Perm())}, nil)
	return err
}

// Remove implements FileOps.
func (p *handlerOps) Remove(ctx context.Context, filePath string, recursive bool) error {
	_, _, err := p.roundTrip(ctx, map[string]any{"op": "remove", "path": filePath, "recursive": recursive}, nil)
	return err
}

// Rename implements FileOps.
func (p *handlerOps) Rename(ctx context.Context, from, to string) error {
	_, _, err := p.roundTrip(ctx, map[string]any{"op": "rename", "from": from, "to": to}, nil)
	return err
}

// Hash implements FileOps. The digest is computed on the target, so a file that
// has not changed never crosses the network at all.
func (p *handlerOps) Hash(ctx context.Context, filePath string) (string, error) {
	hdr, _, err := p.roundTrip(ctx, map[string]any{"op": "hash", "path": filePath}, nil)
	if err != nil {
		return "", err
	}
	digest, _ := hdr["digest"].(string)
	if digest == "" {
		return "", fmt.Errorf("hash %s: handler returned no digest", filePath)
	}
	return digest, nil
}

// Search implements FileOps by running the search on the target.
//
// ripgrep, when the target has it, beats anything this handler could do in
// Python by a wide margin, and matching its ignore-file and encoding behaviour
// in a reimplementation would be a source of quiet disagreement between tiers.
// The same search runs at every tier.
func (p *handlerOps) Search(ctx context.Context, req reach.SearchRequest) ([]reach.Match, error) {
	return p.base.Search(ctx, req)
}

// Glob implements FileOps on the target, for the same reason as Search.
func (p *handlerOps) Glob(ctx context.Context, root, pattern string) ([]string, error) {
	return p.base.Glob(ctx, root, pattern)
}

func infoFromMap(m map[string]any) reach.FileInfo {
	num := func(k string) int64 {
		if v, ok := m[k].(float64); ok {
			return int64(v)
		}
		return 0
	}
	str := func(k string) string {
		v, _ := m[k].(string)
		return v
	}
	boolean := func(k string) bool {
		v, _ := m[k].(bool)
		return v
	}
	return reach.FileInfo{
		Name:       str("name"),
		Path:       str("path"),
		Size:       num("size"),
		Mode:       fs.FileMode(num("mode")),
		ModTime:    time.Unix(num("mtime"), 0),
		IsDir:      boolean("is_dir"),
		IsLink:     boolean("is_link"),
		LinkTarget: str("link_target"),
	}
}

var _ FileOps = (*handlerOps)(nil)
