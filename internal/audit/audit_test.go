package audit

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

// TestConcurrentAppendsStayIntact is the property the format is chosen for.
//
// A harness issues tool calls in parallel and each is its own reach process, so
// several append to one file at once. Records are kept under the size POSIX
// guarantees an O_APPEND write delivers atomically, which is what stops two
// commands from interleaving into a line that parses as neither.
func TestConcurrentAppendsStayIntact(t *testing.T) {
	dir := t.TempDir()
	const n = 50

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			Append(dir, "s", Entry{
				Action:  "exec",
				Command: fmt.Sprintf("command number %d %s", i, strings.Repeat("x", 200)),
				Code:    i % 3,
			})
		}(i)
	}
	wg.Wait()

	entries, err := Read(dir, "s", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != n {
		t.Fatalf("read %d records, want %d — concurrent appends corrupted the log", len(entries), n)
	}
	seen := map[string]bool{}
	for _, e := range entries {
		seen[e.Command] = true
		if e.Session != "s" {
			t.Errorf("record has session %q", e.Session)
		}
		if e.Time.IsZero() {
			t.Error("record has no timestamp")
		}
	}
	if len(seen) != n {
		t.Errorf("%d distinct commands recorded, want %d", len(seen), n)
	}
}

// TestOversizedFieldsAreClipped keeps a record inside the atomic-append bound.
// A command too long to record in full is truncated in the log and run in full.
func TestOversizedFieldsAreClipped(t *testing.T) {
	dir := t.TempDir()
	Append(dir, "s", Entry{Action: "exec", Command: strings.Repeat("y", 100_000)})

	entries, err := Read(dir, "s", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("read %d records, want 1", len(entries))
	}
	if len(entries[0].Command) > maxField+64 {
		t.Errorf("command recorded at %d bytes; records must stay small enough to append atomically",
			len(entries[0].Command))
	}
	if !strings.Contains(entries[0].Command, "more bytes") {
		t.Error("truncation is not disclosed in the record")
	}
}

// TestDisableEnvStopsRecording: keeping a record of every command is
// occasionally the wrong thing to do, and that judgement is the operator's.
func TestDisableEnvStopsRecording(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(DisableEnv, "1")
	Append(dir, "s", Entry{Action: "exec", Command: "secret --token abc"})

	if _, err := Read(dir, "s", 0); err == nil {
		t.Error("actions were recorded despite " + DisableEnv)
	}
}

// TestTruncatedTailIsSkipped: a process killed mid-write must not make the rest
// of the record unreadable.
func TestTruncatedTailIsSkipped(t *testing.T) {
	dir := t.TempDir()
	Append(dir, "s", Entry{Action: "exec", Command: "good"})

	f, err := openForAppend(Path(dir, "s"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"action":"exec","comm`); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	entries, err := Read(dir, "s", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Command != "good" {
		t.Errorf("a truncated final line cost us the readable records: %+v", entries)
	}
}
