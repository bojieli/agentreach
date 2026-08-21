package execserver

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// A mutex per key would give mutual exclusion and the wrong order half the
// time. This is the test that the queue is built from the order requests
// arrived in, not the order goroutines happen to be scheduled in.
func TestSequencerKeepsArrivalOrder(t *testing.T) {
	q := newSequencer()

	var mu sync.Mutex
	var ran []int

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		place := q.enter("path:/srv/app/config")
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			place.wait()
			defer place.done()
			// The first request is the slow one. Without ordering every other
			// request would overtake it.
			if i == 0 {
				time.Sleep(30 * time.Millisecond)
			}
			mu.Lock()
			ran = append(ran, i)
			mu.Unlock()
		}(i)
	}
	wg.Wait()

	for i, got := range ran {
		if got != i {
			t.Fatalf("requests ran in order %v, want them in the order they arrived", ran)
		}
	}
	if len(q.tails) != 0 {
		t.Errorf("the queue kept %d keys after everything finished", len(q.tails))
	}
}

// Ordering one path must not serialise the rest, or the handler pool has
// nothing to do.
func TestSequencerDoesNotQueueUnrelatedRequests(t *testing.T) {
	q := newSequencer()

	held := q.enter("path:/a")
	// A second request for the same path waits; one for another path does not.
	blocked := q.enter("path:/a")
	free := q.enter("path:/b")

	done := make(chan string, 2)
	go func() { blocked.wait(); done <- "same path" }()
	go func() { free.wait(); done <- "other path" }()

	select {
	case got := <-done:
		if got != "other path" {
			t.Fatalf("%q ran while the request in front of it was still going", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a request for an unrelated path was queued behind one that had not finished")
	}

	held.done()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a queued request was not released when the one in front finished")
	}
	blocked.done()
	free.done()
}

func TestOrderKey(t *testing.T) {
	for _, tc := range []struct {
		method string
		params string
		want   string
	}{
		{"fs/readFile", `{"path":"file:///srv/app/x"}`, "path:file:///srv/app/x"},
		{"fs/writeFile", `{"path":"file:///srv/app/x"}`, "path:file:///srv/app/x"},
		{"fs/remove", `{"path":"file:///srv/app/x"}`, "path:file:///srv/app/x"},
		{"fs/copy", `{"sourcePath":"file:///a","destinationPath":"file:///b"}`, "path:file:///b"},
		{"fs/readBlock", `{"handleId":"h1","offset":0}`, "handle:h1"},
		{"process/write", `{"processId":"p1","chunk":"eA=="}`, "process:p1"},

		// A long poll must never have anything queued behind it, and the escape
		// hatches must never be queued behind a write that cannot finish.
		{"process/read", `{"processId":"p1"}`, ""},
		{"process/signal", `{"processId":"p1","signal":"SIGINT"}`, ""},
		{"process/terminate", `{"processId":"p1"}`, ""},
		// A tree is not a path, and a traversal was never atomic.
		{"fs/walk", `{"path":"file:///srv"}`, ""},
		// Nothing to key on, so nothing to order.
		{"fs/readFile", `{}`, ""},
		{"fs/readFile", `{"path":""}`, ""},
		{"fs/readFile", `not json`, ""},
		{"environment/info", `{}`, ""},
	} {
		if got := orderKey(tc.method, json.RawMessage(tc.params)); got != tc.want {
			t.Errorf("orderKey(%s, %s) = %q, want %q", tc.method, tc.params, got, tc.want)
		}
	}
}

// TestPipelinedWritesAreAppliedInOrder drives the whole server the way a client
// is allowed to: several requests about one file, sent without waiting for the
// answers. The last write is what the read has to see.
func TestPipelinedWritesAreAppliedInOrder(t *testing.T) {
	env := newTestEnv(t)
	env.initialize(t)

	path := filepath.Join(env.root, "pipelined.txt")
	uri := "file://" + path

	const writes = 20
	ids := make([]int, 0, writes+1)
	for i := 0; i < writes; i++ {
		ids = append(ids, env.client.send(t, "fs/writeFile", map[string]any{
			"path":       uri,
			"dataBase64": base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("write %d", i))),
		}))
	}
	readID := env.client.send(t, "fs/readFile", map[string]any{"path": uri})

	for _, id := range ids {
		if f := env.client.await(t, id); f.Error != nil {
			t.Fatalf("write %d: %s", id, f.Error.Message)
		}
	}
	f := env.client.await(t, readID)
	if f.Error != nil {
		t.Fatalf("read: %s", f.Error.Message)
	}

	var result struct {
		DataBase64 string `json:"dataBase64"`
	}
	if err := json.Unmarshal(f.Result, &result); err != nil {
		t.Fatalf("decode read result: %v", err)
	}
	data, err := base64.StdEncoding.DecodeString(result.DataBase64)
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	want := fmt.Sprintf("write %d", writes-1)
	if string(data) != want {
		t.Errorf("the read saw %q, want %q — a request overtook the ones sent before it", data, want)
	}

	// And the file on the target agrees with what the read reported.
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) != want {
		t.Errorf("the target holds %q, want %q", onDisk, want)
	}
}
