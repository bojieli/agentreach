package fileops

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/bojieli/agentreach/internal/reach"
	"github.com/bojieli/agentreach/internal/transport"
)

// countingTransport records how many channels were opened through it, so a test
// can tell a handler that was started again from one that was never restarted.
type countingTransport struct {
	transport.Transport
	mu    sync.Mutex
	opens int
}

func (c *countingTransport) Open(ctx context.Context, command string) (transport.Stream, error) {
	c.mu.Lock()
	c.opens++
	c.mu.Unlock()
	return c.Transport.Open(ctx, command)
}

func (c *countingTransport) Opens() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.opens
}

func localPipeTransport(t *testing.T) (*countingTransport, *POSIX) {
	t.Helper()
	base, err := transport.NewLocal()
	if err != nil {
		t.Skipf("no POSIX shell here: %v", err)
	}
	caps, err := Probe(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	if ok, why := caps.Qualifies(reach.TierPipe); !ok {
		t.Skipf("this machine cannot host the pipe tier: %s", why)
	}
	tr := &countingTransport{Transport: base}
	return tr, NewPOSIX(tr, caps)
}

// TestABrokenHandlerIsStartedAgain: a stream broken mid-session used to end file
// access for the rest of that session — under the exec-server, where one handler
// serves a whole agent session, one cancelled operation was permanent. The
// request that discovered the break must still fail, because whether it reached
// the far end is unknown, but the next one must get a working handler.
func TestABrokenHandlerIsStartedAgain(t *testing.T) {
	tr, base := localPipeTransport(t)
	ctx := context.Background()

	file := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(file, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}

	ops, err := NewPipe(ctx, tr, base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ops.Close() })

	if got, err := ops.Read(ctx, file, 0, 0); err != nil || string(got) != "before" {
		t.Fatalf("first read = %q, %v; want %q", got, err, "before")
	}
	if tr.Opens() != 1 {
		t.Fatalf("starting one handler opened %d channels, want 1", tr.Opens())
	}

	// The far end dies underneath a working session: a killed interpreter, a
	// dropped channel, an operation abandoned on a timeout.
	p, ok := ops.(*handlerOps)
	if !ok {
		t.Fatalf("pipe tier is %T, not *handlerOps", ops)
	}
	p.closeStream()

	if _, err := ops.Read(ctx, file, 0, 0); err == nil {
		t.Fatal("the read that discovered the broken stream reported success")
	}

	if got, err := ops.Read(ctx, file, 0, 0); err != nil || string(got) != "before" {
		t.Fatalf("read after the handler was restarted = %q, %v; want %q", got, err, "before")
	}
	if tr.Opens() != 2 {
		t.Errorf("recovery opened %d channels in total, want 2", tr.Opens())
	}

	// The restarted handler is a full one, not a read-only rescue: writes reach
	// the target through it too.
	if err := ops.Write(ctx, file, []byte("after"), 0o644); err != nil {
		t.Fatalf("write through the restarted handler: %v", err)
	}
	got, err := os.ReadFile(file)
	if err != nil || string(got) != "after" {
		t.Fatalf("target file = %q, %v; want %q", got, err, "after")
	}
}

// TestAHandlerThatNeverStartedIsNotRespawned: restarting is earned by having
// worked. A target with no interpreter must fail once and stay failed, rather
// than spawning a doomed process for every file operation the agent attempts.
func TestAHandlerThatNeverStartedIsNotRespawned(t *testing.T) {
	tr, base := localPipeTransport(t)
	ctx := context.Background()

	ops, err := startHandler(ctx, tr, base, reach.TierPipe,
		"python handler", "exec /nonexistent/interpreter", "")
	if err != nil {
		t.Skipf("this transport rejects the channel outright: %v", err)
	}
	t.Cleanup(func() { _ = ops.Close() })

	for i := 0; i < 3; i++ {
		if _, err := ops.Read(ctx, "/etc/hostname", 0, 0); err == nil {
			t.Fatalf("read %d through a handler that never started succeeded", i)
		}
	}
	if tr.Opens() != 1 {
		t.Errorf("a handler that never answered was started %d times, want 1", tr.Opens())
	}
}

// TestCloseBarsRestart: Close is a decision, not a fault. A request racing it
// must not resurrect the handler the caller just ended.
func TestCloseBarsRestart(t *testing.T) {
	tr, base := localPipeTransport(t)
	ctx := context.Background()

	file := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	ops, err := NewPipe(ctx, tr, base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ops.Read(ctx, file, 0, 0); err != nil {
		t.Fatalf("read before close: %v", err)
	}
	if err := ops.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Two attempts: the first discovers the closed stream, and the second would
	// be the one restart normally happens on.
	for i := 0; i < 2; i++ {
		if _, err := ops.Read(ctx, file, 0, 0); err == nil {
			t.Fatalf("read %d after Close succeeded", i)
		}
	}
	if tr.Opens() != 1 {
		t.Errorf("Close was followed by %d channel opens in total, want 1", tr.Opens())
	}
}
