package fileops

import (
	"bufio"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/agentreach/internal/transport"
)

// TestHandlerThatNeverStartsExplainsItself: removing the handshake ping must
// not cost the diagnosis it existed for. A target with no python3 has to say
// so, in the first error the caller sees.
func TestHandlerThatNeverStartsExplainsItself(t *testing.T) {
	tr, err := transport.NewLocal()
	if err != nil {
		t.Skipf("no POSIX shell here: %v", err)
	}
	caps, err := Probe(context.Background(), tr)
	if err != nil {
		t.Fatal(err)
	}
	// A command that is not an interpreter at all.
	ops, err := startHandler(context.Background(), tr, NewPOSIX(tr, caps), 0,
		"python handler", "exec /nonexistent/interpreter", "")
	if err != nil {
		// Failing at Open is also acceptable, as long as it says why.
		if !strings.Contains(err.Error(), "python handler") {
			t.Fatalf("error does not name the program: %v", err)
		}
		return
	}
	t.Cleanup(func() { _ = ops.Close() })

	_, readErr := ops.Read(context.Background(), "/etc/hostname", 0, 0)
	if readErr == nil {
		t.Fatal("reading through a handler that never started succeeded")
	}
	if !strings.Contains(readErr.Error(), "did not start") {
		t.Errorf("first failure does not report a startup problem: %v", readErr)
	}
	// The reason is the half an operator can act on, and it arrives on stderr
	// rather than on the pipe the failure itself came from. Reporting it used to
	// depend on which of the two won a race.
	if !strings.Contains(readErr.Error(), "/nonexistent/interpreter") {
		t.Errorf("first failure does not carry the target's own complaint: %v", readErr)
	}
	t.Logf("reported as: %v", readErr)

	// Whichever end of that request failed, the stream is no longer known to be
	// in sync, so nothing more may be sent over it.
	if _, err := ops.Read(context.Background(), "/etc/hostname", 0, 0); err == nil {
		t.Error("a second read through a handler that never started succeeded")
	}
}

// errWriter fails every write, standing in for a pipe whose reader has gone.
type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

func closedChan() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

// TestAFailedWriteTakesTheStreamOutOfService: a request that could not be
// written may still have been half accepted, which leaves the program on the
// far end waiting for the rest of the frame. Answering a later request over the
// same stream would answer it against this one's tail — one file's bytes
// returned as another's, the silent wrong answer this protocol is arranged
// throughout to prevent.
func TestAFailedWriteTakesTheStreamOutOfService(t *testing.T) {
	p := &handlerOps{
		label:        "test handler",
		stderr:       &safeBuffer{remaining: 1 << 10},
		stderrDone:   closedChan(),
		startTimeout: time.Second,
		first:        true,
		in:           bufio.NewReader(strings.NewReader("")),
		out:          errWriter{},
	}

	if _, err := p.Stat(context.Background(), "/etc/hostname"); err == nil {
		t.Fatal("a request whose write failed was reported as succeeding")
	} else if !strings.Contains(err.Error(), "did not start") {
		t.Errorf("a write that failed before the program ever answered is a startup failure: %v", err)
	}

	_, err := p.Stat(context.Background(), "/etc/hostname")
	if err == nil {
		t.Fatal("a second request went out over a stream left mid-frame")
	}
	if !strings.Contains(err.Error(), "mid-request") {
		t.Errorf("the stream was not taken out of service: %v", err)
	}
}
