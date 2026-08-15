package fileops

import (
	"context"
	"strings"
	"testing"

	"github.com/bojieli/waldo/internal/transport"
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
	t.Logf("reported as: %v", readErr)
}
