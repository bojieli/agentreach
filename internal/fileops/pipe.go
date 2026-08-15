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
	"time"

	"github.com/bojieli/waldo/internal/transport"
	"github.com/bojieli/waldo/internal/waldo"
)

//go:embed handler.py
var pipeHandler string

// bootstrap is the entire command line waldo runs on the target for this tier.
//
// The obvious form — `python3 -` with the program on stdin — cannot work: the
// interpreter reads its source from stdin until EOF, leaving no stdin for the
// protocol to use afterwards. So waldo sends the program as a single base64
// line, which a one-liner decodes and executes, and everything after that first
// newline is the protocol. Nothing touches the target's disk either way.
//
// `exec` replaces the shell with the interpreter, so closing the channel kills
// the handler instead of leaving an orphan behind on someone else's machine.
const bootstrap = `import sys,base64;exec(compile(base64.b64decode(sys.stdin.buffer.readline()),'<waldo>','exec'))`

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

// pipeStartTimeout bounds the handshake, so a target that accepts the channel
// and then never answers fails as a value instead of hanging tier negotiation.
const pipeStartTimeout = 20 * time.Second

// handlerOps implements FileOps by keeping one long-lived process alive on the
// target and speaking a framed protocol to it.
//
// Tier 2 and tier 3 differ only in which program is on the other end — a Python
// interpreter holding a script that was never written to disk, or a small
// installed binary — so they share this client entirely. A protocol difference
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
	stream transport.Stream
	base   *POSIX
	tier   waldo.Tier
	// label names the program on the other end, for error messages an operator
	// has to act on: "the python handler died" and "the agent died" call for
	// different next steps.
	label string

	mu  sync.Mutex
	in  *bufio.Reader
	out io.Writer
	id  uint32

	closeOnce sync.Once
}

// NewPipe starts the Python handler on the target and verifies it answers.
func NewPipe(ctx context.Context, t transport.Transport, base *POSIX) (FileOps, error) {
	cmd := fmt.Sprintf("exec python3 -c %s", transport.ShellQuote(bootstrap))
	return startHandler(ctx, t, base, waldo.TierPipe, "python handler", cmd,
		base64.StdEncoding.EncodeToString([]byte(pipeHandler))+"\n")
}

// startHandler opens a channel, runs a program on it, sends any preamble the
// program expects, and confirms it answers the protocol before returning.
//
// The confirmation matters more than it looks: without it, a target where the
// program failed to start would hand back a strategy that appears healthy and
// fails on the first real file operation — in the middle of an agent's turn,
// where it reads as a broken tool rather than an unavailable tier.
func startHandler(ctx context.Context, t transport.Transport, base *POSIX, tier waldo.Tier, label, command, preamble string) (FileOps, error) {
	// The channel outlives this call, so it must not be tied to a context that
	// is cancelled once the handshake returns.
	stream, err := t.Open(context.WithoutCancel(ctx), command)
	if err != nil {
		return nil, fmt.Errorf("start %s: %w", label, err)
	}

	var errBuf strings.Builder
	go func() { _, _ = io.Copy(&limitedWriter{w: &errBuf, remaining: 8 << 10}, stream.Stderr) }()

	p := &handlerOps{
		stream: stream,
		base:   base,
		tier:   tier,
		label:  label,
		in:     bufio.NewReaderSize(stream.Stdout, 1<<16),
		out:    stream.Stdin,
	}

	if preamble != "" {
		if _, err := io.WriteString(stream.Stdin, preamble); err != nil {
			_ = stream.Close()
			return nil, fmt.Errorf("send %s: %w", label, err)
		}
	}

	type result struct{ err error }
	ch := make(chan result, 1)
	go func() {
		_, _, err := p.roundTrip(map[string]any{"op": "ping", "version": waldo.Version}, nil)
		ch <- result{err}
	}()

	select {
	case r := <-ch:
		if r.err != nil {
			_ = stream.Close()
			if msg := strings.TrimSpace(errBuf.String()); msg != "" {
				return nil, fmt.Errorf("%s did not start: %w (%s)", label, r.err, firstLine(msg))
			}
			return nil, fmt.Errorf("%s did not start: %w", label, r.err)
		}
		return p, nil
	case <-time.After(pipeStartTimeout):
		_ = stream.Close()
		return nil, fmt.Errorf("%s did not answer within %s", label, pipeStartTimeout)
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// Tier implements FileOps.
func (p *handlerOps) Tier() waldo.Tier { return p.tier }

// Close implements FileOps, ending the handler process.
func (p *handlerOps) Close() error {
	p.closeOnce.Do(func() { _ = p.stream.Close() })
	return nil
}

// roundTrip sends one request and reads its response.
func (p *handlerOps) roundTrip(req map[string]any, payload []byte) (map[string]any, []byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

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
		return nil, nil, fmt.Errorf("write request: %w", err)
	}
	if len(payload) > 0 {
		if _, err := p.out.Write(payload); err != nil {
			return nil, nil, fmt.Errorf("write payload: %w", err)
		}
	}

	respHdr, respPayload, err := p.readFrame()
	if err != nil {
		return nil, nil, err
	}
	if ok, _ := respHdr["ok"].(bool); !ok {
		return nil, nil, pipeError(respHdr, req)
	}
	return respHdr, respPayload, nil
}

func (p *handlerOps) readFrame() (map[string]any, []byte, error) {
	hdrLen, err := p.readUint32()
	if err != nil {
		return nil, nil, err
	}
	if hdrLen > maxFrame {
		return nil, nil, fmt.Errorf("handler sent a %d-byte header, above the %d-byte limit", hdrLen, maxFrame)
	}
	hdrBuf := make([]byte, hdrLen)
	if _, err := io.ReadFull(p.in, hdrBuf); err != nil {
		return nil, nil, fmt.Errorf("read response header: %w", err)
	}
	var hdr map[string]any
	if err := json.Unmarshal(hdrBuf, &hdr); err != nil {
		return nil, nil, fmt.Errorf("parse response header: %w", err)
	}
	payloadLen, err := p.readUint32()
	if err != nil {
		return nil, nil, err
	}
	if payloadLen > maxFrame {
		return nil, nil, fmt.Errorf("handler sent a %d-byte payload, above the %d-byte limit", payloadLen, maxFrame)
	}
	payload := make([]byte, payloadLen)
	if payloadLen > 0 {
		if _, err := io.ReadFull(p.in, payload); err != nil {
			return nil, nil, fmt.Errorf("read response payload: %w", err)
		}
	}
	return hdr, payload, nil
}

func (p *handlerOps) readUint32() (uint32, error) {
	var buf [4]byte
	if _, err := io.ReadFull(p.in, buf[:]); err != nil {
		if err == io.EOF {
			return 0, fmt.Errorf("the %s on the target exited", p.label)
		}
		return 0, err
	}
	return binary.BigEndian.Uint32(buf[:]), nil
}

// pipeError converts a handler failure into waldo's error vocabulary.
func pipeError(hdr map[string]any, req map[string]any) error {
	msg, _ := hdr["error"].(string)
	kind, _ := hdr["kind"].(string)
	if kind == "notfound" {
		path, _ := req["path"].(string)
		return &waldo.NotFoundError{Path: path}
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
		_, payload, err := p.roundTrip(map[string]any{
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
	_, _, err := p.roundTrip(map[string]any{
		"op": "write", "path": filePath, "mode": int(mode.Perm()),
	}, data)
	return err
}

// Stat implements FileOps.
func (p *handlerOps) Stat(ctx context.Context, filePath string) (*waldo.FileInfo, error) {
	hdr, _, err := p.roundTrip(map[string]any{"op": "stat", "path": filePath}, nil)
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
func (p *handlerOps) List(ctx context.Context, dir string) ([]waldo.FileInfo, error) {
	hdr, _, err := p.roundTrip(map[string]any{"op": "list", "path": dir}, nil)
	if err != nil {
		return nil, err
	}
	rawEntries, _ := hdr["entries"].([]any)
	out := make([]waldo.FileInfo, 0, len(rawEntries))
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
	_, _, err := p.roundTrip(map[string]any{"op": "mkdir", "path": dir, "mode": int(mode.Perm())}, nil)
	return err
}

// Remove implements FileOps.
func (p *handlerOps) Remove(ctx context.Context, filePath string, recursive bool) error {
	_, _, err := p.roundTrip(map[string]any{"op": "remove", "path": filePath, "recursive": recursive}, nil)
	return err
}

// Rename implements FileOps.
func (p *handlerOps) Rename(ctx context.Context, from, to string) error {
	_, _, err := p.roundTrip(map[string]any{"op": "rename", "from": from, "to": to}, nil)
	return err
}

// Hash implements FileOps. The digest is computed on the target, so a file that
// has not changed never crosses the network at all.
func (p *handlerOps) Hash(ctx context.Context, filePath string) (string, error) {
	hdr, _, err := p.roundTrip(map[string]any{"op": "hash", "path": filePath}, nil)
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
func (p *handlerOps) Search(ctx context.Context, req waldo.SearchRequest) ([]waldo.Match, error) {
	return p.base.Search(ctx, req)
}

// Glob implements FileOps on the target, for the same reason as Search.
func (p *handlerOps) Glob(ctx context.Context, root, pattern string) ([]string, error) {
	return p.base.Glob(ctx, root, pattern)
}

func infoFromMap(m map[string]any) waldo.FileInfo {
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
	return waldo.FileInfo{
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
