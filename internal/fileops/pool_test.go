package fileops

import (
	"context"
	"errors"
	"io/fs"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojieli/agentreach/internal/reach"
)

// concurrency records how many operations were in flight at once across every
// member of a pool, which is the property a pool exists to change.
type concurrency struct {
	mu       sync.Mutex
	inFlight int
	peak     int
	reached  chan struct{}
	want     int
}

func (c *concurrency) enter() {
	c.mu.Lock()
	c.inFlight++
	if c.inFlight > c.peak {
		c.peak = c.inFlight
	}
	if c.reached != nil && c.inFlight == c.want {
		close(c.reached)
		c.reached = nil
	}
	c.mu.Unlock()
}

func (c *concurrency) leave() {
	c.mu.Lock()
	c.inFlight--
	c.mu.Unlock()
}

func (c *concurrency) peakSeen() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.peak
}

// fakeOps is one pool member. It blocks in every operation until gate is
// closed, standing in for a handler holding its stream across a long read.
type fakeOps struct {
	tier   reach.Tier
	gate   <-chan struct{}
	seen   *concurrency
	closed atomic.Bool
}

func (f *fakeOps) work(ctx context.Context) error {
	if f.closed.Load() {
		return errors.New("used after close")
	}
	f.seen.enter()
	defer f.seen.leave()
	if f.gate == nil {
		return nil
	}
	select {
	case <-f.gate:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *fakeOps) Read(ctx context.Context, _ string, _, _ int64) ([]byte, error) {
	return nil, f.work(ctx)
}
func (f *fakeOps) Write(ctx context.Context, _ string, _ []byte, _ fs.FileMode) error {
	return f.work(ctx)
}
func (f *fakeOps) Stat(ctx context.Context, _ string) (*reach.FileInfo, error) {
	return &reach.FileInfo{}, f.work(ctx)
}
func (f *fakeOps) List(ctx context.Context, _ string) ([]reach.FileInfo, error) {
	return nil, f.work(ctx)
}
func (f *fakeOps) Mkdir(ctx context.Context, _ string, _ fs.FileMode) error { return f.work(ctx) }
func (f *fakeOps) Remove(ctx context.Context, _ string, _ bool) error       { return f.work(ctx) }
func (f *fakeOps) Rename(ctx context.Context, _, _ string) error            { return f.work(ctx) }
func (f *fakeOps) Search(ctx context.Context, _ reach.SearchRequest) ([]reach.Match, error) {
	return nil, f.work(ctx)
}
func (f *fakeOps) Glob(ctx context.Context, _, _ string) ([]string, error) {
	return nil, f.work(ctx)
}
func (f *fakeOps) Hash(ctx context.Context, _ string) (string, error) { return "", f.work(ctx) }
func (f *fakeOps) Tier() reach.Tier                                   { return f.tier }
func (f *fakeOps) Close() error                                       { f.closed.Store(true); return nil }

type poolFixture struct {
	seen   *concurrency
	gate   chan struct{}
	opened atomic.Int32
	made   []*fakeOps
	mu     sync.Mutex
}

func newPoolFixture(want int) *poolFixture {
	return &poolFixture{
		seen: &concurrency{reached: make(chan struct{}), want: want},
		gate: make(chan struct{}),
	}
}

func (f *poolFixture) member(tier reach.Tier) *fakeOps {
	m := &fakeOps{tier: tier, gate: f.gate, seen: f.seen}
	f.mu.Lock()
	f.made = append(f.made, m)
	f.mu.Unlock()
	return m
}

func (f *poolFixture) opener(tier reach.Tier) func(context.Context) (FileOps, error) {
	return func(context.Context) (FileOps, error) {
		f.opened.Add(1)
		return f.member(tier), nil
	}
}

// awaitConcurrency waits for the fixture to see want operations in flight at
// once, which only happens if the pool actually grew.
func (f *poolFixture) awaitConcurrency(t *testing.T) {
	t.Helper()
	f.seen.mu.Lock()
	ch := f.seen.reached
	f.seen.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("only %d operations ran at once; the pool did not grow", f.seen.peakSeen())
	}
}

// TestPoolRunsOperationsAtTheSameTime: one handler answers one request at a
// time, so under the exec-server a long read used to hold every other file
// operation behind it. The pool exists to make that false.
func TestPoolRunsOperationsAtTheSameTime(t *testing.T) {
	f := newPoolFixture(3)
	pool := NewPool(f.member(reach.TierPipe), 4, f.opener(reach.TierPipe))

	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = pool.Read(context.Background(), "/f", 0, 0)
		}()
	}
	f.awaitConcurrency(t)
	close(f.gate)
	wg.Wait()

	if peak := f.seen.peakSeen(); peak != 3 {
		t.Errorf("%d operations ran at once, want 3", peak)
	}
	if got := f.opened.Load(); got != 2 {
		t.Errorf("the pool started %d more handlers, want 2", got)
	}
}

// A pool that grew for every operation would start a handler per tool call,
// which is the cost this tier exists to avoid.
func TestPoolDoesNotGrowWithoutContention(t *testing.T) {
	f := newPoolFixture(1)
	close(f.gate) // nothing blocks; every operation finishes before the next
	pool := NewPool(f.member(reach.TierPipe), 4, f.opener(reach.TierPipe))

	for i := 0; i < 8; i++ {
		if _, err := pool.Stat(context.Background(), "/f"); err != nil {
			t.Fatalf("stat %d: %v", i, err)
		}
	}
	if got := f.opened.Load(); got != 0 {
		t.Errorf("a serial workload started %d extra handlers, want 0", got)
	}
}

// The bound is what keeps a fan-out from opening a process on the target per
// request.
func TestPoolRespectsItsLimit(t *testing.T) {
	f := newPoolFixture(2)
	pool := NewPool(f.member(reach.TierPipe), 2, f.opener(reach.TierPipe))

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = pool.List(context.Background(), "/d")
		}()
	}
	f.awaitConcurrency(t)
	// Everything past the limit has to be waiting rather than running.
	time.Sleep(50 * time.Millisecond)
	if peak := f.seen.peakSeen(); peak > 2 {
		t.Errorf("%d operations ran at once against a limit of 2", peak)
	}
	close(f.gate)
	wg.Wait()
	if got := f.opened.Load(); got != 1 {
		t.Errorf("the pool started %d more handlers against a limit of 2, want 1", got)
	}
}

// A member that came back at a different tier would make the pool report a tier
// only some of its operations actually used.
func TestPoolRefusesAMemberAtAnotherTier(t *testing.T) {
	f := newPoolFixture(1)
	pool := NewPool(f.member(reach.TierPipe), 4, f.opener(reach.TierPOSIX))

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = pool.Read(context.Background(), "/f", 0, 0)
	}()
	f.awaitConcurrency(t)

	// This one finds the pool empty, is offered a member at the wrong tier, and
	// must wait for the right one instead of using it.
	done := make(chan error, 1)
	go func() {
		_, err := pool.Stat(context.Background(), "/f")
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("the second operation was served by a mismatched member: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	close(f.gate)
	wg.Wait()
	if err := <-done; err != nil {
		t.Fatalf("the second operation did not fall back to the working member: %v", err)
	}
	if pool.Tier() != reach.TierPipe {
		t.Errorf("pool reports tier %s, want %s", pool.Tier(), reach.TierPipe)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range f.made {
		if m.tier == reach.TierPOSIX && !m.closed.Load() {
			t.Error("the mismatched member was left running on the target")
		}
	}
}

func TestPoolCloseEndsEveryMember(t *testing.T) {
	f := newPoolFixture(2)
	pool := NewPool(f.member(reach.TierPipe), 4, f.opener(reach.TierPipe))

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = pool.Read(context.Background(), "/f", 0, 0)
		}()
	}
	f.awaitConcurrency(t)
	close(f.gate)
	wg.Wait()

	if err := pool.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	f.mu.Lock()
	made := append([]*fakeOps(nil), f.made...)
	f.mu.Unlock()
	if len(made) != 2 {
		t.Fatalf("fixture made %d members, want 2", len(made))
	}
	for i, m := range made {
		if !m.closed.Load() {
			t.Errorf("member %d was left running on the target after Close", i)
		}
	}
	if _, err := pool.Stat(context.Background(), "/f"); err == nil {
		t.Error("an operation after Close was served")
	}
}

// A caller that gives up waiting must not be stuck behind a member that never
// comes back.
func TestPoolHonoursACancelledContext(t *testing.T) {
	f := newPoolFixture(1)
	pool := NewPool(f.member(reach.TierPipe), 1, f.opener(reach.TierPipe))

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = pool.Read(context.Background(), "/f", 0, 0)
	}()
	f.awaitConcurrency(t)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := pool.Stat(ctx, "/f")
	if err == nil {
		t.Fatal("a waiter served past its deadline")
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("the waiter took %v to notice its context had ended", took)
	}
	close(f.gate)
	wg.Wait()
}
