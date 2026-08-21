package mirror

import (
	"context"
	"fmt"
	"io/fs"
	"sync"
	"testing"

	"github.com/bojieli/agentreach/internal/reach"
)

type fakeOps struct{}

func (fakeOps) Read(_ context.Context, p string, _, _ int64) ([]byte, error) {
	return []byte("content of " + p), nil
}
func (fakeOps) Write(context.Context, string, []byte, fs.FileMode) error { return nil }
func (fakeOps) Stat(context.Context, string) (*reach.FileInfo, error)    { return nil, nil }
func (fakeOps) List(context.Context, string) ([]reach.FileInfo, error)   { return nil, nil }
func (fakeOps) Mkdir(context.Context, string, fs.FileMode) error         { return nil }
func (fakeOps) Remove(context.Context, string, bool) error               { return nil }
func (fakeOps) Rename(context.Context, string, string) error             { return nil }
func (fakeOps) Search(context.Context, reach.SearchRequest) ([]reach.Match, error) {
	return nil, nil
}
func (fakeOps) Glob(context.Context, string, string) ([]string, error) { return nil, nil }
func (fakeOps) Hash(context.Context, string) (string, error)           { return "", nil }
func (fakeOps) Tier() reach.Tier                                       { return reach.TierPOSIX }
func (fakeOps) Close() error                                           { return nil }

// TestConcurrentFetchesKeepEveryDigest is a regression test for a real defect.
//
// A harness issues tool calls in parallel, so PreToolUse hooks run in parallel,
// each recording the digest of the file it fetched. While those records lived in
// one shared JSON document, each hook did a load-modify-write of the whole map
// and the last writer won: one entry survived out of twenty.
func TestConcurrentFetchesKeepEveryDigest(t *testing.T) {
	m := New(t.TempDir(), fakeOps{})
	const n = 20
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := m.Fetch(context.Background(), fmt.Sprintf("/srv/app/f%d.go", i)); err != nil {
				t.Errorf("fetch %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	// Every fetch must have left a record. A missing one is not a lost
	// optimisation: Push treats "no record" as "nothing to verify against" and
	// writes anyway, so the digest guard silently stops guarding — in exactly
	// the concurrent case where two tools are most likely to touch one tree.
	for i := 0; i < n; i++ {
		path := fmt.Sprintf("/srv/app/f%d.go", i)
		if _, known := m.expectedDigest(path); !known {
			t.Errorf("no digest recorded for %s; a later write to it would skip the guard", path)
		}
	}
}
