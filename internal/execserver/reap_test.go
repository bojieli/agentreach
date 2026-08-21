package execserver

import (
	"encoding/base64"
	"fmt"
	"testing"
	"time"
)

// A process record is kept after the command exits so its output can still be
// read, and nothing used to remove it. A server that ran a hundred commands
// held a hundred records, each with up to a mebibyte of retained output, for as
// long as the agent was running — the same session-long accumulation the
// retained-output cap prevents one level up.
func TestFinishedProcessesAreDropped(t *testing.T) {
	env := newTestEnv(t)
	env.initialize(t)

	const commands = maxFinishedProcesses + 8
	for i := 0; i < commands; i++ {
		id := fmt.Sprintf("p%d", i)
		env.startProcess(t, id, "printf hello", nil)
		if _, _, code := env.readAll(t, id); code != 0 {
			t.Fatalf("process %s exited %d", id, code)
		}
	}

	env.srv.mu.Lock()
	held := len(env.srv.processes)
	tracked := len(env.srv.finished)
	env.srv.mu.Unlock()

	if held > maxFinishedProcesses {
		t.Errorf("the server holds %d finished processes, want at most %d",
			held, maxFinishedProcesses)
	}
	if tracked > maxFinishedProcesses {
		t.Errorf("the server tracks %d finished ids, want at most %d",
			tracked, maxFinishedProcesses)
	}

	// The most recent commands are still readable, which is what the retention
	// is for.
	recent := fmt.Sprintf("p%d", commands-1)
	if f := env.client.call(t, "process/read", map[string]any{"processId": recent}); f.Error != nil {
		t.Errorf("the most recent process was not readable: %s", f.Error.Message)
	}
	// The oldest is gone, and says so the way any forgotten id does.
	if f := env.client.call(t, "process/read", map[string]any{"processId": "p0"}); f.Error == nil {
		t.Error("a process dropped long ago was still answered")
	}
}

// An interactive process can be written to for as long as the agent keeps
// talking to it. Remembering every write id it ever saw grew for the life of
// the process.
func TestRememberedWritesAreBounded(t *testing.T) {
	env := newTestEnv(t)
	env.initialize(t)

	env.startProcess(t, "cat", "cat > /dev/null", map[string]any{"pipeStdin": true})

	const writes = maxRememberedWrites + 64
	for i := 0; i < writes; i++ {
		f := env.client.call(t, "process/write", map[string]any{
			"processId": "cat",
			"writeId":   fmt.Sprintf("w%d", i),
			"chunk":     base64.StdEncoding.EncodeToString([]byte("x\n")),
		})
		if f.Error != nil {
			t.Fatalf("write %d: %s", i, f.Error.Message)
		}
	}

	env.srv.mu.Lock()
	p := env.srv.processes["cat"]
	env.srv.mu.Unlock()
	if p == nil {
		t.Fatal("the process disappeared while it was still running")
	}

	p.mu.Lock()
	remembered := len(p.writeIDs)
	ordered := len(p.writeOrder)
	p.mu.Unlock()
	if remembered > maxRememberedWrites || ordered > maxRememberedWrites {
		t.Errorf("the process remembers %d write ids (%d ordered), want at most %d",
			remembered, ordered, maxRememberedWrites)
	}

	// A recent write is still deduplicated, which is what remembering is for.
	f := env.client.call(t, "process/write", map[string]any{
		"processId": "cat",
		"writeId":   fmt.Sprintf("w%d", writes-1),
		"chunk":     base64.StdEncoding.EncodeToString([]byte("this must not be written again\n")),
	})
	if f.Error != nil {
		t.Fatalf("replayed write: %s", f.Error.Message)
	}
	p.mu.Lock()
	stillOrdered := len(p.writeOrder)
	p.mu.Unlock()
	if stillOrdered != ordered {
		t.Errorf("a replayed write was applied again: %d ids ordered, was %d", stillOrdered, ordered)
	}

	env.client.call(t, "process/terminate", map[string]any{"processId": "cat"})
	// Give the reaper a moment so the cleanup in Serve has nothing to kill.
	time.Sleep(50 * time.Millisecond)
}
