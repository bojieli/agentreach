package transport

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"

	"github.com/bojieli/agentreach/internal/reach"
)

// RunStream executes a command and streams its output as it arrives.
//
// Buffering a command's output until it exits is fine for a file read and
// wrong for a ten-minute build: the operator watches a dead terminal and
// cannot tell a slow test suite from a hung connection. Streaming keeps the
// feedback that makes a remote shell feel local.
//
// The exit status still travels in-band behind a random marker, so the
// stdout stream is filtered to remove that marker before the caller sees it.
func RunStream(ctx context.Context, t Transport, req reach.ExecRequest, stdout, stderr io.Writer) (int, error) {
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}

	sentinel := newSentinel()
	wrapped := wrapWithSentinel(BuildCommand(req), sentinel)

	st, err := t.Open(ctx, wrapped)
	if err != nil {
		return 0, err
	}
	defer func() { _ = st.Close() }()

	if len(req.Stdin) > 0 {
		go func() {
			_, _ = st.Stdin.Write(req.Stdin)
			_ = st.Stdin.Close()
		}()
	} else {
		_ = st.Stdin.Close()
	}

	filter := &sentinelFilter{out: stdout, sentinel: []byte("\n" + sentinel)}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(stderr, st.Stderr)
	}()

	_, copyErr := io.Copy(filter, st.Stdout)
	wg.Wait()
	filter.flush()

	waitCode, waitErr := st.Wait()
	if copyErr != nil && ctx.Err() == nil {
		return 0, fmt.Errorf("%s: reading output: %w", t.Describe(), copyErr)
	}
	if ctx.Err() != nil {
		return 0, fmt.Errorf("%s: %w", t.Describe(), ctx.Err())
	}
	if code, ok := filter.exitCode(); ok {
		return code, nil
	}
	if waitErr != nil {
		return 0, fmt.Errorf("%s: %w", t.Describe(), waitErr)
	}
	// The marker never arrived, so the command did not run to completion. This
	// is a transport failure and must not be reported to the agent as a plain
	// non-zero exit, which would send it debugging a command that never ran.
	return 0, fmt.Errorf("%s: connection closed before the command completed (status %d)", t.Describe(), waitCode)
}

// SentinelFilter passes a streamed command's stdout through while holding back
// enough of the tail to recognise and remove the trailing status marker
// WrapWithSentinel adds. It is the streaming form of the marker recovery Run
// does internally, exported for the exec-server, which pumps output chunks to a
// client while the command is still running.
type SentinelFilter struct {
	inner *sentinelFilter
}

// NewSentinelFilter wraps out with a filter stripping "\n"+sentinel and the
// status digits that follow it.
func NewSentinelFilter(out io.Writer, sentinel string) *SentinelFilter {
	return &SentinelFilter{inner: &sentinelFilter{out: out, sentinel: []byte("\n" + sentinel)}}
}

// Write implements io.Writer.
func (f *SentinelFilter) Write(p []byte) (int, error) { return f.inner.Write(p) }

// Flush emits whatever remains, minus the status marker if it is present. It
// must be called once the command's stdout reaches EOF.
func (f *SentinelFilter) Flush() { f.inner.flush() }

// ExitCode returns the status the marker carried, and false when the marker
// never arrived — which means the transport, not the command, failed.
func (f *SentinelFilter) ExitCode() (int, bool) { return f.inner.exitCode() }

// sentinelFilter passes bytes through while holding back enough of the tail to
// recognise and remove the trailing status marker.
type sentinelFilter struct {
	out      io.Writer
	sentinel []byte
	pending  bytes.Buffer
	code     int
	haveCode bool
}

// hold is how many bytes are withheld from the caller: the marker itself plus
// room for the digits and newline that follow it.
func (f *sentinelFilter) hold() int { return len(f.sentinel) + 24 }

func (f *sentinelFilter) Write(p []byte) (int, error) {
	f.pending.Write(p)
	if f.pending.Len() > f.hold() {
		emit := f.pending.Len() - f.hold()
		buf := f.pending.Next(emit)
		if _, err := f.out.Write(buf); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// flush emits whatever remains, minus the status marker if it is present.
func (f *sentinelFilter) flush() {
	rest := f.pending.Bytes()
	if idx := bytes.LastIndex(rest, f.sentinel); idx >= 0 {
		tail := rest[idx+len(f.sentinel):]
		end := bytes.IndexByte(tail, '\n')
		if end < 0 {
			end = len(tail)
		}
		if n, err := strconv.Atoi(strings.TrimSpace(string(tail[:end]))); err == nil {
			f.code, f.haveCode = n, true
		}
		rest = rest[:idx]
	}
	if len(rest) > 0 {
		_, _ = f.out.Write(rest)
	}
	f.pending.Reset()
}

func (f *sentinelFilter) exitCode() (int, bool) { return f.code, f.haveCode }
