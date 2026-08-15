package waldo

import (
	"path"
	"strings"
	"sync"
	"testing"
)

// TestTempPathsNeverCollide is the regression test for a class of bug, not for
// one tier.
//
// The sftp tier numbered temporaries from a per-client counter that restarted
// in every process, so parallel waldo processes all chose `.waldo.tmp.1` and
// the exclusive create refused all but one: three of eight concurrent writes
// failed against a real host. The rule was never written down, so each tier
// invented its own naming and one of them invented it badly. It is written down
// here now, and this is what holds it.
func TestTempPathsNeverCollide(t *testing.T) {
	const goroutines, each = 16, 500

	var mu sync.Mutex
	seen := make(map[string]bool, goroutines*each)

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := make([]string, 0, each)
			for i := 0; i < each; i++ {
				local = append(local, TempPath("/srv/app"))
			}
			mu.Lock()
			defer mu.Unlock()
			for _, p := range local {
				if seen[p] {
					t.Errorf("temporary path %q drawn twice; a concurrent write would fail on an exclusive create", p)
					return
				}
				seen[p] = true
			}
		}()
	}
	wg.Wait()

	if len(seen) != goroutines*each {
		t.Fatalf("got %d distinct paths from %d draws", len(seen), goroutines*each)
	}
}

// TestTempPathShape pins the properties the tiers depend on.
func TestTempPathShape(t *testing.T) {
	for _, dir := range []string{"/srv/app", "/", "/a/b/c"} {
		p := TempPath(dir)

		// Same directory as the destination: rename is only atomic within one
		// filesystem, and a temporary elsewhere would silently become a copy.
		if got := path.Dir(p); got != path.Clean(dir) {
			t.Errorf("TempPath(%q) landed in %q, not in the destination's directory", dir, got)
		}
		// Identifiable as waldo's, so debris from an interrupted write can be
		// recognised rather than puzzled over.
		if !strings.HasPrefix(path.Base(p), ".waldo.tmp.") {
			t.Errorf("TempPath(%q) = %q, which nothing would recognise as waldo's", dir, p)
		}
		// Hidden, so it does not show up in a listing the agent is reading.
		if !strings.HasPrefix(path.Base(p), ".") {
			t.Errorf("TempPath(%q) = %q is not a dotfile", dir, p)
		}
		// POSIX separators: these are the target's paths, whatever waldo runs on.
		if strings.Contains(p, "\\") {
			t.Errorf("TempPath(%q) = %q contains a backslash", dir, p)
		}
	}
}
