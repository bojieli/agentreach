package mirror

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/bojieli/agentreach/internal/fileops"
)

// countingOps records how much of a file crossed the "network", and can be told
// to have no digest utility at all.
type countingOps struct {
	fileops.FileOps
	mu        sync.Mutex
	reads     int
	hashes    int
	hashFails bool
}

func (c *countingOps) Read(ctx context.Context, path string, off, n int64) ([]byte, error) {
	c.mu.Lock()
	c.reads++
	c.mu.Unlock()
	return c.FileOps.Read(ctx, path, off, n)
}

func (c *countingOps) Hash(ctx context.Context, path string) (string, error) {
	c.mu.Lock()
	c.hashes++
	fails := c.hashFails
	c.mu.Unlock()
	if fails {
		return "", errors.New("target has no sha256 utility")
	}
	return c.FileOps.Hash(ctx, path)
}

func (c *countingOps) Write(ctx context.Context, path string, data []byte, mode fs.FileMode) error {
	return c.FileOps.Write(ctx, path, data, mode)
}

func (c *countingOps) counts() (reads, hashes int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reads, c.hashes
}

func newCountingMirror(t *testing.T) (*Mirror, *countingOps, string) {
	t.Helper()
	tr := localTransport(t)
	caps, err := fileops.Probe(context.Background(), tr)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	ops := &countingOps{FileOps: fileops.NewPOSIX(tr, caps)}
	return New(t.TempDir(), ops), ops, t.TempDir()
}

// An agent reads the same file several times in a turn — before an edit, after
// it, while following a reference. Each of those used to pull the whole file
// across the network to produce bytes the mirror already held.
func TestFetchDoesNotTransferAFileItAlreadyHas(t *testing.T) {
	m, ops, target := newCountingMirror(t)
	ctx := context.Background()
	remote := filepath.Join(target, "f.txt")
	if err := os.WriteFile(remote, []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	local, err := m.Fetch(ctx, remote)
	if err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	firstReads, _ := ops.counts()
	if firstReads != 1 {
		t.Fatalf("the first fetch read %d times, want 1", firstReads)
	}

	for i := 0; i < 3; i++ {
		got, err := m.Fetch(ctx, remote)
		if err != nil {
			t.Fatalf("fetch %d: %v", i, err)
		}
		if got != local {
			t.Fatalf("fetch %d returned %s, want %s", i, got, local)
		}
	}
	reads, hashes := ops.counts()
	if reads != 1 {
		t.Errorf("re-fetching an unchanged file read it %d times, want 1", reads)
	}
	if hashes == 0 {
		t.Error("the mirror never asked the target for a digest")
	}
	if data, _ := os.ReadFile(local); string(data) != "original\n" {
		t.Errorf("mirrored content = %q", data)
	}
}

// The saving must never cost correctness: a file that changed on the target has
// to be fetched again.
func TestFetchTransfersAFileThatChangedOnTheTarget(t *testing.T) {
	m, ops, target := newCountingMirror(t)
	ctx := context.Background()
	remote := filepath.Join(target, "f.txt")
	if err := os.WriteFile(remote, []byte("first\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Fetch(ctx, remote); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(remote, []byte("second\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	local, err := m.Fetch(ctx, remote)
	if err != nil {
		t.Fatalf("fetch after change: %v", err)
	}
	if data, _ := os.ReadFile(local); string(data) != "second\n" {
		t.Fatalf("mirrored content = %q, want %q — a changed file was not refetched", data, "second\n")
	}
	if reads, _ := ops.counts(); reads != 2 {
		t.Errorf("%d reads, want 2 (one per change)", reads)
	}
}

// A mirrored file the agent edited but has not pushed is not what the target
// holds, whatever the target's digest says about the target's own copy.
func TestFetchRefetchesOverALocallyEditedCopy(t *testing.T) {
	m, _, target := newCountingMirror(t)
	ctx := context.Background()
	remote := filepath.Join(target, "f.txt")
	if err := os.WriteFile(remote, []byte("target\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	local, err := m.Fetch(ctx, remote)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(local, []byte("edited locally\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := m.Fetch(ctx, remote); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if data, _ := os.ReadFile(local); string(data) != "target\n" {
		t.Errorf("mirrored content = %q, want the target's %q", data, "target\n")
	}
}

// Verifying a write used to read the whole file back to hash it locally, so
// every edit moved the file across the network three times.
func TestPushVerifiesWithADigestRatherThanATransfer(t *testing.T) {
	m, ops, target := newCountingMirror(t)
	ctx := context.Background()
	remote := filepath.Join(target, "f.txt")
	if err := os.WriteFile(remote, []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	local, err := m.Fetch(ctx, remote)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(local, []byte("edited\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	before, _ := ops.counts()
	if err := m.Push(ctx, remote); err != nil {
		t.Fatalf("push: %v", err)
	}
	after, _ := ops.counts()
	if after != before {
		t.Errorf("push read the target %d times to verify it; the target can hash it instead", after-before)
	}
	if data, _ := os.ReadFile(remote); string(data) != "edited\n" {
		t.Errorf("target content = %q, want %q", data, "edited\n")
	}
}

// The guarantee the verification exists for must survive the change.
func TestPushStillRefusesAFileThatChangedOnTheTarget(t *testing.T) {
	m, _, target := newCountingMirror(t)
	ctx := context.Background()
	remote := filepath.Join(target, "f.txt")
	if err := os.WriteFile(remote, []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	local, err := m.Fetch(ctx, remote)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(local, []byte("my edit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Something else changes it on the target in the meantime.
	if err := os.WriteFile(remote, []byte("someone else's work\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err = m.Push(ctx, remote)
	if err == nil {
		t.Fatal("push overwrote a file that had changed on the target")
	}
	if data, _ := os.ReadFile(remote); string(data) != "someone else's work\n" {
		t.Errorf("the other change was lost: target holds %q", data)
	}
}

// Tier 0 needs a sha256 utility on the target and some hosts have none. The
// mirror has to keep working there, by the route it always used.
func TestMirrorWorksOnATargetThatCannotHash(t *testing.T) {
	m, ops, target := newCountingMirror(t)
	ops.hashFails = true
	ctx := context.Background()
	remote := filepath.Join(target, "f.txt")
	if err := os.WriteFile(remote, []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	local, err := m.Fetch(ctx, remote)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if data, _ := os.ReadFile(local); string(data) != "original\n" {
		t.Fatalf("mirrored content = %q", data)
	}
	if err := os.WriteFile(local, []byte("edited\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Push(ctx, remote); err != nil {
		t.Fatalf("push: %v", err)
	}
	if data, _ := os.ReadFile(remote); string(data) != "edited\n" {
		t.Errorf("target content = %q, want %q", data, "edited\n")
	}

	// And the guarantee still holds without a digest utility.
	if err := os.WriteFile(remote, []byte("changed underneath\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(local, []byte("another edit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Push(ctx, remote); err == nil {
		t.Error("without a digest utility, push overwrote a file that had changed")
	}
}

// emptyDigestOps stands in for a tier that answers a hash request with nothing
// and no error — a shape no tier reach ships produces, and one that must not be
// believed if one ever does.
type emptyDigestOps struct{ fileops.FileOps }

func (e emptyDigestOps) Hash(context.Context, string) (string, error) { return "", nil }

// An empty digest taken at face value compares unequal to the recorded one, so
// Push would refuse the write as "something else modified it" — naming a cause
// that did not happen, for a file nothing had touched.
func TestAnEmptyDigestIsNotAnAnswer(t *testing.T) {
	tr := localTransport(t)
	caps, err := fileops.Probe(context.Background(), tr)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	m := New(t.TempDir(), emptyDigestOps{FileOps: fileops.NewPOSIX(tr, caps)})

	ctx := context.Background()
	target := t.TempDir()
	remote := filepath.Join(target, "f.txt")
	if err := os.WriteFile(remote, []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	local, err := m.Fetch(ctx, remote)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if err := os.WriteFile(local, []byte("edited\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Push(ctx, remote); err != nil {
		t.Fatalf("push was refused against an unchanged file: %v", err)
	}
	if data, _ := os.ReadFile(remote); string(data) != "edited\n" {
		t.Errorf("target content = %q, want %q", data, "edited\n")
	}
}
