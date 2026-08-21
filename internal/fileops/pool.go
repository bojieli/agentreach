package fileops

import (
	"context"
	"fmt"
	"io/fs"
	"sync"

	"github.com/bojieli/agentreach/internal/reach"
)

// NewPool spreads concurrent file operations across several strategies, each
// with its own channel to the target.
//
// The handler protocol is deliberately serialised: one duplex stream, ordered
// framing, one request in flight. That costs nothing where reach runs a process
// per tool call, because such a process issues its operations one after another
// anyway. Under `reach exec-server`, where one process serves an entire agent
// session and answers requests concurrently, it is head-of-line blocking: a
// 100 MB read holds the stream across a dozen sequential chunk round trips
// while every other file operation waits behind it.
//
// Pipelining would not fix that. The program on the far end is a single loop
// reading one frame at a time, so overlapping requests on one stream would only
// move the queue to the other end — and it would reintroduce the ambiguity the
// serialised protocol exists to remove, where an abandoned response can be read
// as the answer to the next request. More streams is the fix that keeps both
// properties: concurrency comes from channels, exactly as it does everywhere
// else in reach.
//
// Members are created only under contention, so a session that never overlaps
// two file operations never starts a second handler.
func NewPool(first FileOps, limit int, open func(context.Context) (FileOps, error)) FileOps {
	if limit < 1 {
		limit = 1
	}
	p := &poolOps{max: limit, open: open, tier: first.Tier(), idle: []FileOps{first}, live: 1}
	p.free = sync.NewCond(&p.mu)
	return p
}

type poolOps struct {
	max  int
	open func(context.Context) (FileOps, error)
	tier reach.Tier

	mu   sync.Mutex
	free *sync.Cond
	idle []FileOps
	// live counts members that exist or are being created, so two callers
	// arriving at once cannot both decide they are the one allowed to grow the
	// pool past max.
	live   int
	closed bool
}

// get checks out a member, creating one if the pool may still grow and waiting
// for one to come back if it may not.
func (p *poolOps) get(ctx context.Context) (FileOps, error) {
	p.mu.Lock()
	for {
		if p.closed {
			p.mu.Unlock()
			return nil, fmt.Errorf("file access for this session is closed")
		}
		if n := len(p.idle); n > 0 {
			ops := p.idle[n-1]
			p.idle = p.idle[:n-1]
			p.mu.Unlock()
			return ops, nil
		}
		if p.live < p.max {
			p.live++
			p.mu.Unlock()
			ops, err := p.grow(ctx)
			if err == nil {
				return ops, nil
			}
			// Growing failed — a full connection, a target that will not start
			// another interpreter. That is a reason to wait for a member that
			// already works, not to fail an operation the pool can still serve.
			p.mu.Lock()
			p.live--
			p.free.Broadcast()
			if p.live == 0 {
				p.mu.Unlock()
				return nil, err
			}
			continue
		}
		p.waitForFree(ctx)
		if err := ctx.Err(); err != nil {
			p.mu.Unlock()
			return nil, err
		}
	}
}

// waitForFree blocks until a member is returned, the pool closes, or ctx ends.
// It is called with mu held and returns with mu held.
func (p *poolOps) waitForFree(ctx context.Context) {
	// sync.Cond cannot be waited on with a context, so cancellation is
	// delivered by waking every waiter when ctx ends. The watcher exits with
	// the wait, so a long-lived pool does not accumulate one per operation.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			p.mu.Lock()
			p.free.Broadcast()
			p.mu.Unlock()
		case <-done:
		}
	}()
	p.free.Wait()
}

// grow starts one more member and returns it checked out.
func (p *poolOps) grow(ctx context.Context) (FileOps, error) {
	ops, err := p.open(ctx)
	if err != nil {
		return nil, err
	}
	// A member that negotiated a different tier from the rest would make this
	// pool report a tier that only some of its operations actually use, and
	// reach's rule is that a strategy never reports a tier it did not get.
	if ops.Tier() != p.tier {
		_ = ops.Close()
		return nil, fmt.Errorf("a second %s strategy came back as %s; not mixing tiers within one session",
			p.tier, ops.Tier())
	}
	return ops, nil
}

// put returns a member to the pool.
//
// A member whose stream broke is returned like any other: the handler restarts
// itself on its next use, so the pool holds a slot that works again rather than
// shrinking every time an operation is cancelled.
func (p *poolOps) put(ops FileOps) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		_ = ops.Close()
		return
	}
	p.idle = append(p.idle, ops)
	p.free.Signal()
	p.mu.Unlock()
}

// Tier implements FileOps.
func (p *poolOps) Tier() reach.Tier { return p.tier }

// Close implements FileOps, ending every member. Members that are checked out
// are closed as they are returned.
func (p *poolOps) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	idle := p.idle
	p.idle = nil
	p.free.Broadcast()
	p.mu.Unlock()

	var err error
	for _, ops := range idle {
		if cerr := ops.Close(); err == nil {
			err = cerr
		}
	}
	return err
}

// Read implements FileOps.
func (p *poolOps) Read(ctx context.Context, path string, off, n int64) ([]byte, error) {
	ops, err := p.get(ctx)
	if err != nil {
		return nil, err
	}
	defer p.put(ops)
	return ops.Read(ctx, path, off, n)
}

// Write implements FileOps.
func (p *poolOps) Write(ctx context.Context, path string, data []byte, mode fs.FileMode) error {
	ops, err := p.get(ctx)
	if err != nil {
		return err
	}
	defer p.put(ops)
	return ops.Write(ctx, path, data, mode)
}

// Stat implements FileOps.
func (p *poolOps) Stat(ctx context.Context, path string) (*reach.FileInfo, error) {
	ops, err := p.get(ctx)
	if err != nil {
		return nil, err
	}
	defer p.put(ops)
	return ops.Stat(ctx, path)
}

// List implements FileOps.
func (p *poolOps) List(ctx context.Context, path string) ([]reach.FileInfo, error) {
	ops, err := p.get(ctx)
	if err != nil {
		return nil, err
	}
	defer p.put(ops)
	return ops.List(ctx, path)
}

// Mkdir implements FileOps.
func (p *poolOps) Mkdir(ctx context.Context, path string, mode fs.FileMode) error {
	ops, err := p.get(ctx)
	if err != nil {
		return err
	}
	defer p.put(ops)
	return ops.Mkdir(ctx, path, mode)
}

// Remove implements FileOps.
func (p *poolOps) Remove(ctx context.Context, path string, recursive bool) error {
	ops, err := p.get(ctx)
	if err != nil {
		return err
	}
	defer p.put(ops)
	return ops.Remove(ctx, path, recursive)
}

// Rename implements FileOps.
func (p *poolOps) Rename(ctx context.Context, from, to string) error {
	ops, err := p.get(ctx)
	if err != nil {
		return err
	}
	defer p.put(ops)
	return ops.Rename(ctx, from, to)
}

// Search implements FileOps.
func (p *poolOps) Search(ctx context.Context, req reach.SearchRequest) ([]reach.Match, error) {
	ops, err := p.get(ctx)
	if err != nil {
		return nil, err
	}
	defer p.put(ops)
	return ops.Search(ctx, req)
}

// Glob implements FileOps.
func (p *poolOps) Glob(ctx context.Context, root, pattern string) ([]string, error) {
	ops, err := p.get(ctx)
	if err != nil {
		return nil, err
	}
	defer p.put(ops)
	return ops.Glob(ctx, root, pattern)
}

// Hash implements FileOps.
func (p *poolOps) Hash(ctx context.Context, path string) (string, error) {
	ops, err := p.get(ctx)
	if err != nil {
		return "", err
	}
	defer p.put(ops)
	return ops.Hash(ctx, path)
}

var _ FileOps = (*poolOps)(nil)
