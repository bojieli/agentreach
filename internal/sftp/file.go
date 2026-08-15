package sftp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"sync"
	"sync/atomic"
)

// pipelineDepth is how many chunk requests are in flight at once.
//
// SFTP's throughput over a high-latency link is bounded by round trips, not
// bandwidth: one 32 KiB request per round trip on a 100 ms link is 320 KiB/s no
// matter how fast the network is. Eight in flight makes it eight times that.
// The depth is deliberately modest — the point is to fill the pipe, not to
// queue a hundred outstanding requests whose failure handling gets complicated.
const pipelineDepth = 8

// ClampSize converts a server-reported size to int64 without overflowing.
//
// Sizes arrive from the other end of the connection, which waldo does not
// trust. Anything at or above the int64 ceiling is capped rather than wrapped:
// a wrapped negative length would silently change the meaning of every read
// that used it.
func ClampSize(v uint64) int64 {
	const maxInt64 = uint64(1<<63 - 1)
	if v > maxInt64 {
		return int64(maxInt64)
	}
	return int64(v)
}

// ReadFile reads n bytes from path starting at off. n <= 0 means to the end.
func (c *Client) ReadFile(ctx context.Context, filePath string, off int64, n int64) ([]byte, error) {
	if off < 0 {
		off = 0
	}
	handle, err := c.Open(ctx, filePath, flagRead, Attrs{})
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.CloseHandle(context.WithoutCancel(ctx), handle) }()

	if n <= 0 {
		a, err := c.Fstat(ctx, handle)
		if err != nil {
			return nil, err
		}
		if !a.HasSize() {
			// Without a size there is nothing to divide into chunks, so fall
			// back to reading sequentially until EOF.
			return c.readToEOF(ctx, handle, off)
		}
		if uint64(off) >= a.Size {
			return []byte{}, nil
		}
		// The size is whatever the server said. A hostile or broken one
		// reporting 2^63 bytes would overflow into a negative length and turn
		// this loop into nonsense, so it is clamped before it is used for
		// anything.
		n = ClampSize(a.Size) - off
	}

	chunks := int((n + maxDataChunk - 1) / maxDataChunk)
	out := make([][]byte, chunks)
	errs := make([]error, chunks)

	sem := make(chan struct{}, pipelineDepth)
	var wg sync.WaitGroup
	for i := 0; i < chunks; i++ {
		start := off + int64(i)*maxDataChunk
		want := n - int64(i)*maxDataChunk
		if want > maxDataChunk {
			want = maxDataChunk
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, start, want int64) {
			defer wg.Done()
			defer func() { <-sem }()
			out[i], errs[i] = c.readExactly(ctx, handle, uint64(start), int(want))
		}(i, start, want)
	}
	wg.Wait()

	var buf []byte
	for i := range out {
		if errs[i] != nil && !errors.Is(errs[i], io.EOF) {
			return nil, errs[i]
		}
		buf = append(buf, out[i]...)
		// A short chunk means end of file. Everything after it is empty, and
		// concatenating past it would splice a hole into the content.
		if len(out[i]) < maxDataChunk && i != len(out)-1 {
			break
		}
	}
	return buf, nil
}

// readExactly fills one chunk, reissuing for short reads.
//
// SFTP permits a server to return fewer bytes than requested at any time, not
// only at end of file. Treating a short read as EOF would silently truncate
// files — the worst kind of bug this layer can have, because the content still
// looks plausible.
func (c *Client) readExactly(ctx context.Context, handle string, off uint64, want int) ([]byte, error) {
	buf := make([]byte, 0, want)
	for len(buf) < want {
		part, err := c.ReadAt(ctx, handle, off+uint64(len(buf)), uint32(want-len(buf)))
		if err != nil {
			if errors.Is(err, io.EOF) {
				return buf, nil
			}
			return nil, err
		}
		if len(part) == 0 {
			return buf, nil
		}
		buf = append(buf, part...)
	}
	return buf, nil
}

func (c *Client) readToEOF(ctx context.Context, handle string, off int64) ([]byte, error) {
	var buf []byte
	for {
		part, err := c.ReadAt(ctx, handle, uint64(off)+uint64(len(buf)), maxDataChunk)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return buf, nil
			}
			return nil, err
		}
		if len(part) == 0 {
			return buf, nil
		}
		buf = append(buf, part...)
	}
}

// WriteFile replaces a path's contents atomically.
//
// The content goes to a temporary file in the same directory and is renamed
// into place, so a concurrent reader sees either the old file or the new one
// and never a half-written one, and a failed transfer leaves the original
// intact. Same-directory placement keeps the rename within one filesystem,
// which is where POSIX guarantees it is atomic.
func (c *Client) WriteFile(ctx context.Context, filePath string, data []byte, mode uint32) error {
	dir := path.Dir(filePath)
	tmp := path.Join(dir, fmt.Sprintf(".waldo.tmp.%d", c.tempSuffix()))

	handle, err := c.Open(ctx, tmp,
		flagWrite|flagCreat|flagTrunc|flagExcl,
		Attrs{Flags: attrPermissions, Permissions: mode})
	if err != nil {
		return fmt.Errorf("create temporary file next to %s: %w", filePath, err)
	}

	cleanup := func(cause error) error {
		_ = c.CloseHandle(context.WithoutCancel(ctx), handle)
		_ = c.Remove(context.WithoutCancel(ctx), tmp)
		return cause
	}

	// Writes are pipelined for the same reason reads are, and the cost of
	// forgetting it here was larger than the cost of getting it wrong there.
	//
	// A sequential loop spends one round trip per 32 KiB chunk: on a link with
	// 258 ms of latency, writing 8 MiB took 79 seconds — eleven times slower
	// than the shell tier it is supposed to beat, and slow in a way that is
	// invisible on loopback, where a round trip is free. Measured, not guessed;
	// see docs/TRANSPORTS.md.
	chunks := (len(data) + maxDataChunk - 1) / maxDataChunk
	errs := make([]error, chunks)
	sem := make(chan struct{}, pipelineDepth)
	var wg sync.WaitGroup
	for i := 0; i < chunks; i++ {
		off := i * maxDataChunk
		end := off + maxDataChunk
		if end > len(data) {
			end = len(data)
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i, off, end int) {
			defer wg.Done()
			defer func() { <-sem }()
			errs[i] = c.WriteAt(ctx, handle, uint64(off), data[off:end])
		}(i, off, end)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return cleanup(err)
		}
	}
	if err := c.CloseHandle(ctx, handle); err != nil {
		_ = c.Remove(context.WithoutCancel(ctx), tmp)
		return err
	}
	// Some servers ignore the mode supplied at open time. Setting it explicitly
	// costs one round trip and removes the question.
	if err := c.Chmod(ctx, tmp, mode); err != nil {
		_ = c.Remove(context.WithoutCancel(ctx), tmp)
		return err
	}
	if err := c.Rename(ctx, tmp, filePath); err != nil {
		_ = c.Remove(context.WithoutCancel(ctx), tmp)
		return err
	}
	return nil
}

// tempSuffix produces a per-client counter for temporary names. It does not
// need to be unguessable, only unique among writes this client makes; the file
// is created with EXCL, so a collision fails loudly instead of clobbering.
func (c *Client) tempSuffix() uint32 {
	return atomic.AddUint32(&c.tmpSeq, 1)
}
