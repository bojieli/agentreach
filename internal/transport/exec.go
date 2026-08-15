package transport

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bojieli/waldo/internal/waldo"
)

// defaultMaxOutput bounds captured output per stream. A command that prints
// without limit must not be able to exhaust memory or flood an agent's context.
const defaultMaxOutput = 4 << 20 // 4 MiB

// newSentinel returns an unguessable marker used to recover a remote command's
// exit status from its stdout.
//
// This exists because `ssh` reports its own failures as exit 255, which is
// indistinguishable from a remote command that genuinely exited 255. Getting
// that wrong in either direction is bad: a transport failure reported as a
// command failure sends the agent chasing a phantom bug, and a command failure
// reported as a transport failure makes waldo retry something that should not
// be retried. So the real status is carried in-band behind a random marker,
// and its absence is itself the signal that the transport, not the command,
// failed.
func newSentinel() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is not recoverable; fall back to a time-derived
		// value rather than a predictable constant.
		return "waldo" + strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return "__waldo_" + hex.EncodeToString(b[:]) + "__"
}

// wrapWithSentinel makes a command report its exit status on stdout.
//
// The leading newline is what makes the split exact regardless of whether the
// command's own output ended with one: we always cut at the final "\n"+marker,
// so output that already ended in a newline keeps it and output that did not
// does not gain one.
//
// The command runs in a subshell, not a brace group. A brace group shares the
// shell with the wrapper, so a command containing `exit` — which agents write
// routinely, and which waldo's own tier-0 helpers use to signal conditions —
// would terminate the shell before the status line was ever printed. The
// marker would be missing and waldo would misreport a perfectly ordinary
// non-zero exit as a transport failure. A subshell confines `exit` to the
// command and lets the wrapper observe its status.
func wrapWithSentinel(cmd, sentinel string) string {
	return "( " + cmd + "\n); __waldo_rc=$?; printf '\\n%s%d\\n' " +
		ShellQuote(sentinel) + " \"$__waldo_rc\""
}

// splitSentinel recovers (stdout, exitCode) from captured output.
// ok is false when the marker is absent, meaning the command never ran to
// completion and the caller should treat this as a transport failure.
func splitSentinel(out []byte, sentinel string) (stdout []byte, code int, ok bool) {
	marker := append([]byte("\n"), sentinel...)
	idx := bytes.LastIndex(out, marker)
	if idx < 0 {
		return out, 0, false
	}
	tail := out[idx+len(marker):]
	end := bytes.IndexByte(tail, '\n')
	if end < 0 {
		end = len(tail)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(tail[:end])))
	if err != nil {
		return out, 0, false
	}
	return out[:idx], n, true
}

// truncationNotice is inserted between the retained head and tail of an
// over-long stream. It is only ever added on the exec path; file content never
// passes through a truncating writer, because a marker spliced into a file
// would be silently corrupting rather than merely lossy.
const truncationNotice = "\n...[waldo: output truncated, %d bytes omitted]...\n"

// capWriter bounds a stream while always preserving both its beginning and its
// end.
//
// Keeping only the head — the obvious implementation — is wrong twice over.
// First, waldo recovers a command's exit status from a marker printed last, so
// a head-only cap silently destroys the status of exactly those commands that
// produce the most output. Second, when a build or test run floods the stream,
// the useful part is nearly always the failure at the end, so a head-only cap
// discards the very thing the agent needs.
type capWriter struct {
	mu   sync.Mutex
	head bytes.Buffer

	// tail is a ring holding the most recent tailMax bytes.
	tail     []byte
	tailPos  int
	tailFull bool

	headMax, tailMax int64
	omitted          int64
	truncated        bool
}

func newCapWriter(max int64) *capWriter {
	if max <= 0 {
		max = defaultMaxOutput
	}
	// Reserve a quarter of the budget for the tail, with an 8 KiB floor so the
	// status marker and a useful amount of trailing context always survive.
	// The floor is then clamped to half the budget: without that clamp a small
	// cap would be entirely consumed by the tail reservation and the head
	// would be dropped completely.
	tailMax := max / 4
	if tailMax < 8<<10 {
		tailMax = 8 << 10
	}
	if half := max / 2; tailMax > half {
		tailMax = half
	}
	if tailMax < 1 {
		tailMax = 1
	}
	return &capWriter{headMax: max - tailMax, tailMax: tailMax}
}

func (w *capWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(p)

	if room := w.headMax - int64(w.head.Len()); room > 0 {
		if int64(len(p)) <= room {
			w.head.Write(p)
			return n, nil
		}
		w.head.Write(p[:room])
		p = p[room:]
	}

	w.truncated = true
	if w.tail == nil {
		w.tail = make([]byte, w.tailMax)
	}
	for _, b := range p {
		if w.tailFull {
			w.omitted++
		}
		w.tail[w.tailPos] = b
		w.tailPos++
		if w.tailPos == len(w.tail) {
			w.tailPos = 0
			w.tailFull = true
		}
	}
	// Report the full length: a short write would make the producing command
	// block or fail, which is not what a display-side cap should ever cause.
	return n, nil
}

// Bytes returns the retained output, with a notice between head and tail when
// anything was dropped.
func (w *capWriter) Bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.truncated {
		return w.head.Bytes()
	}
	var out bytes.Buffer
	out.Write(w.head.Bytes())
	if w.omitted > 0 {
		fmt.Fprintf(&out, truncationNotice, w.omitted)
	}
	if w.tailFull {
		out.Write(w.tail[w.tailPos:])
		out.Write(w.tail[:w.tailPos])
	} else {
		out.Write(w.tail[:w.tailPos])
	}
	return out.Bytes()
}

// Truncated reports whether any bytes were actually discarded.
//
// This is deliberately not "did the stream spill past the head buffer": output
// that spills into the tail ring but never overruns it is reconstructed
// byte-for-byte by Bytes(), so reporting it as truncated would make callers
// reject perfectly intact content. Only genuine loss counts.
func (w *capWriter) Truncated() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.omitted > 0
}

// runLocalProcess executes argv, feeding stdin and capturing bounded output.
// It is shared by every transport that ultimately spawns a local process
// (ssh, docker, kubectl, local).
func runLocalProcess(ctx context.Context, argv []string, stdin []byte, maxOut int64) (stdoutB, stderrB []byte, code int, truncated bool, err error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)

	// A command must never be left waiting on a terminal that will not arrive.
	if len(stdin) > 0 {
		cmd.Stdin = bytes.NewReader(stdin)
	} else {
		cmd.Stdin = bytes.NewReader(nil)
	}

	so := newCapWriter(maxOut)
	se := newCapWriter(maxOut)
	cmd.Stdout = so
	cmd.Stderr = se

	runErr := cmd.Run()
	exit := 0
	if runErr != nil {
		var ee *exec.ExitError
		if ok := asExitError(runErr, &ee); ok {
			exit = ee.ExitCode()
		} else {
			return so.Bytes(), se.Bytes(), 0, so.Truncated() || se.Truncated(), runErr
		}
	}
	// Surface cancellation as an error rather than as a bogus exit status.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return so.Bytes(), se.Bytes(), exit, so.Truncated() || se.Truncated(),
			fmt.Errorf("command aborted: %w", ctxErr)
	}
	return so.Bytes(), se.Bytes(), exit, so.Truncated() || se.Truncated(), nil
}

func asExitError(err error, target **exec.ExitError) bool {
	for err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			*target = ee
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// finishExec applies the sentinel protocol and timing to a raw process result.
func finishExec(start time.Time, rawOut, rawErr []byte, procCode int, truncated bool, sentinel string, procErr error, describe string) (waldo.ExecResult, error) {
	if procErr != nil {
		return waldo.ExecResult{}, fmt.Errorf("%s: %w", describe, procErr)
	}
	stdout, code, ok := splitSentinel(rawOut, sentinel)
	if !ok {
		// No marker: the remote shell never completed the wrapper. This is a
		// transport failure, not a command failure, and must not be reported
		// to the agent as a non-zero exit.
		return waldo.ExecResult{}, fmt.Errorf(
			"%s: command did not complete (exit %d): %s",
			describe, procCode, strings.TrimSpace(truncateForError(string(rawErr))))
	}
	return waldo.ExecResult{
		Stdout:    stdout,
		Stderr:    rawErr,
		Code:      code,
		Truncated: truncated,
		Duration:  time.Since(start),
	}, nil
}

func truncateForError(s string) string {
	const max = 512
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

var _ = io.Discard
