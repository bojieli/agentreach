//go:build integration

package integration

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/bojieli/waldo/internal/fileops"
	"github.com/bojieli/waldo/internal/waldo"
)

// TestConcurrentWritesOnOneTier mimics a harness issuing parallel tool calls:
// several writers into one directory at once, each with its own strategy, as
// separate waldo processes would have.
func TestConcurrentWritesOnOneTier(t *testing.T) {
	tr := newTransport(t)
	ctx := context.Background()
	caps, err := fileops.Probe(ctx, tr)
	if err != nil {
		t.Fatal(err)
	}
	for _, tier := range []waldo.Tier{waldo.TierSFTP, waldo.TierPipe, waldo.TierPOSIX} {
		t.Run(tier.String(), func(t *testing.T) {
			if ok, why := caps.Qualifies(tier); !ok {
				t.Skipf("not available: %s", why)
			}
			dir := filepath.Join(workspace, "concurrent-"+tier.String())
			var wg sync.WaitGroup
			errs := make([]error, 8)
			ops := make([]fileops.FileOps, 8)
			for i := 0; i < 8; i++ {
				sel, err := fileops.New(ctx, tier, tr, caps, true, nil)
				if err != nil {
					t.Fatal(err)
				}
				ops[i] = sel.Ops
				t.Cleanup(func() { _ = sel.Ops.Close() })
			}
			if err := ops[0].Mkdir(ctx, dir, 0o755); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = ops[0].Remove(context.Background(), dir, true) })

			for i := 0; i < 8; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					p := filepath.Join(dir, fmt.Sprintf("f%d", i))
					errs[i] = ops[i].Write(ctx, p, []byte(fmt.Sprintf("content %d", i)), 0o644)
				}(i)
			}
			wg.Wait()
			for i, err := range errs {
				if err != nil {
					t.Errorf("concurrent write %d failed: %v", i, err)
				}
			}
			for i := 0; i < 8; i++ {
				got, err := ops[0].Read(ctx, filepath.Join(dir, fmt.Sprintf("f%d", i)), 0, 0)
				if err != nil {
					t.Errorf("read %d: %v", i, err)
					continue
				}
				if want := fmt.Sprintf("content %d", i); string(got) != want {
					t.Errorf("file %d = %q, want %q", i, got, want)
				}
			}
		})
	}
}
