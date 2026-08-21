package transport

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/bojieli/agentreach/internal/reach"
)

// A refused channel is exit 255 with a message and nothing else to go on, so
// these are the real strings ssh produces. If OpenSSH ever changes them the
// consequence is a worse error message, not a wrong answer — but the test says
// what reach is relying on.
func TestIsChannelOpenFailure(t *testing.T) {
	refused := []string{
		"channel 0: open failed: administratively prohibited: open failed",
		"mux_client_request_session: session request failed: Session open refused by peer",
		"channel 2: open failed: resource shortage: ",
		"CHANNEL 0: OPEN FAILED: ADMINISTRATIVELY PROHIBITED",
	}
	for _, s := range refused {
		if !IsChannelOpenFailure(s) {
			t.Errorf("not recognised as a refused channel: %q", s)
		}
	}
	// A connection that failed or dropped says nothing about whether the
	// command ran, so it must never be mistaken for a refusal reach can retry.
	other := []string{
		"ssh: connect to host build-box port 22: Connection refused",
		"Permission denied (publickey).",
		"client_loop: send disconnect: Broken pipe",
		"bash: line 1: make: command not found",
		"",
	}
	for _, s := range other {
		if IsChannelOpenFailure(s) {
			t.Errorf("wrongly recognised as a refused channel: %q", s)
		}
	}
}

func TestOverflowIsBoundedAndMovesTheControlPath(t *testing.T) {
	tr := newTestSSH(t, SSHConfig{Multiplex: true})
	first := tr.controlPath()
	for i := 0; i < maxOverflow; i++ {
		if !tr.Overflow() {
			t.Fatalf("overflow %d refused while under the limit", i+1)
		}
	}
	if tr.Overflow() {
		t.Error("overflow past the limit succeeded; reach would open connections without bound")
	}
	if tr.controlPath() == first {
		t.Error("overflowing did not move the transport to another control socket")
	}
}

// Without multiplexing every command already opens its own connection, so it
// can never be refused for want of a channel and there is nothing to overflow.
func TestOverflowIsMeaninglessWithoutMultiplexing(t *testing.T) {
	if newTestSSH(t, SSHConfig{Multiplex: false}).Overflow() {
		t.Error("a non-multiplexed transport claimed it could overflow")
	}
}

func TestOverflowStopsAfterClose(t *testing.T) {
	tr := newTestSSH(t, SSHConfig{Multiplex: true})
	tr.mu.Lock()
	tr.closed = true
	tr.mu.Unlock()
	if tr.Overflow() {
		t.Error("a closed transport opened another connection")
	}
}

// sentinelRE recovers the exit-status marker from the command reach built, so a
// fake target can answer with a well-formed result the way a real one would.
var sentinelRE = regexp.MustCompile(`__reach_[0-9a-f]+__`)

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// scriptedTransport refuses the first n channels and then works, which is what
// a target at its MaxSessions limit does once an earlier command finishes.
type scriptedTransport struct {
	refuseFirst int
	canOverflow bool

	mu        sync.Mutex
	attempts  int
	overflows int
}

func (s *scriptedTransport) Open(_ context.Context, command string) (Stream, error) {
	s.mu.Lock()
	s.attempts++
	n := s.attempts
	s.mu.Unlock()

	stdout, stderr, code := "", "", 0
	if n <= s.refuseFirst {
		stderr = "channel 0: open failed: administratively prohibited: open failed\n"
		code = 255
	} else {
		sentinel := sentinelRE.FindString(command)
		if sentinel == "" {
			return Stream{}, fmt.Errorf("no sentinel in %q", command)
		}
		stdout = "hello\n\n" + sentinel + "0\n"
	}
	return Stream{
		Stdin:  nopWriteCloser{io.Discard},
		Stdout: strings.NewReader(stdout),
		Stderr: strings.NewReader(stderr),
		Wait:   func() (int, error) { return code, nil },
		Close:  func() error { return nil },
	}, nil
}

func (s *scriptedTransport) Overflow() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.canOverflow {
		return false
	}
	s.overflows++
	return true
}

func (s *scriptedTransport) Run(context.Context, reach.ExecRequest) (reach.ExecResult, error) {
	return reach.ExecResult{}, fmt.Errorf("not used")
}
func (s *scriptedTransport) Describe() string { return "ssh://fake" }
func (s *scriptedTransport) Close() error     { return nil }

func (s *scriptedTransport) counts() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts, s.overflows
}

// TestRunStreamRetriesARefusedChannel: eleven parallel tool calls against a
// stock sshd means the eleventh is refused. It used to surface as "command did
// not complete (exit 255)" — a transport failure the agent could only read as
// its command having failed.
func TestRunStreamRetriesARefusedChannel(t *testing.T) {
	tr := &scriptedTransport{refuseFirst: 1, canOverflow: true}
	var out, errOut bytes.Buffer

	code, err := RunStream(context.Background(), tr, reach.ExecRequest{Command: "echo hello"}, &out, &errOut)
	if err != nil {
		t.Fatalf("a refused channel was not retried: %v", err)
	}
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if out.String() != "hello\n" {
		t.Errorf("stdout = %q, want %q", out.String(), "hello\n")
	}
	attempts, overflows := tr.counts()
	if attempts != 2 || overflows != 1 {
		t.Errorf("%d attempts and %d overflows, want 2 and 1", attempts, overflows)
	}
	// ssh has already printed its own refusal, which on its own reads like the
	// command failed. What reach did about it has to be visible too.
	if !strings.Contains(errOut.String(), "opened another one") {
		t.Errorf("the retry was silent; stderr was %q", errOut.String())
	}
}

// A target that refuses every channel is not a capacity problem reach can work
// around, and the operator has to be told what to change.
func TestRunStreamReportsARefusalItCannotWorkAround(t *testing.T) {
	tr := &scriptedTransport{refuseFirst: 99, canOverflow: false}
	var out, errOut bytes.Buffer

	if _, err := RunStream(context.Background(), tr, reach.ExecRequest{Command: "echo hello"}, &out, &errOut); err == nil {
		t.Fatal("a command that never ran was reported as succeeding")
	} else if !strings.Contains(err.Error(), "administratively prohibited") {
		t.Errorf("the failure does not carry the target's own complaint: %v", err)
	}
	if attempts, _ := tr.counts(); attempts != 1 {
		t.Errorf("%d attempts against a transport that cannot overflow, want 1", attempts)
	}
}

// failingTransport fails for a reason that says nothing about whether the
// command ran.
type failingTransport struct{ scriptedTransport }

func (f *failingTransport) Open(_ context.Context, _ string) (Stream, error) {
	f.mu.Lock()
	f.attempts++
	f.mu.Unlock()
	return Stream{
		Stdin:  nopWriteCloser{io.Discard},
		Stdout: strings.NewReader(""),
		Stderr: strings.NewReader("client_loop: send disconnect: Broken pipe\n"),
		Wait:   func() (int, error) { return 255, nil },
		Close:  func() error { return nil },
	}, nil
}

// TestRunStreamDoesNotRetryADroppedConnection: the retry is safe only because a
// refused channel proves the command never started. A connection that dropped
// proves nothing, and running the command twice is the failure reach's
// in-band status marker exists to prevent.
func TestRunStreamDoesNotRetryADroppedConnection(t *testing.T) {
	tr := &failingTransport{scriptedTransport{canOverflow: true}}
	var out, errOut bytes.Buffer

	if _, err := RunStream(context.Background(), tr, reach.ExecRequest{Command: "make install"}, &out, &errOut); err == nil {
		t.Fatal("a dropped connection was reported as success")
	}
	attempts, overflows := tr.counts()
	if attempts != 1 || overflows != 0 {
		t.Errorf("%d attempts and %d overflows after a dropped connection, want 1 and 0", attempts, overflows)
	}
}
