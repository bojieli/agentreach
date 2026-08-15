package sftp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"sync"

	"github.com/bojieli/waldo/internal/waldo"
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
//
// The shape of this function is dictated by round trips, because on a real link
// that is the whole cost. The obvious implementation — open, stat the handle for
// the size, read, close — spends four of them serially, and measurement said a
// small read over SFTP was losing to a single shell command doing the same job.
//
// It spends two now, because none of the four were actually dependent on each
// other in the way the obvious ordering assumes:
//
//   - `open` and `stat` both take a *path*. The size does not have to be
//     fetched through the handle, so both go out together and the first round
//     trip returns a handle and a size at once.
//   - Knowing the size means the reads that follow are exact and fully
//     pipelined, with no extra request spent discovering where the file ends.
//   - The close is not waited for at all; see closeHandleAsync.
//
// A file that shrinks between the stat and the reads is handled by the short-read
// rule below. A file that *grows* is not chased: reading the length the stat
// reported returns the file as it was at that moment, which is a consistent
// view rather than a truncated one, and chasing the tail would cost a round
// trip on every read to catch a case whose answer would be torn regardless.
func (c *Client) ReadFile(ctx context.Context, filePath string, off int64, n int64) ([]byte, error) {
	if off < 0 {
		off = 0
	}

	type openResult struct {
		handle string
		err    error
	}
	type statResult struct {
		attrs Attrs
		err   error
	}
	openCh := make(chan openResult, 1)
	statCh := make(chan statResult, 1)

	go func() {
		h, err := c.Open(ctx, filePath, flagRead, Attrs{})
		openCh <- openResult{h, err}
	}()
	go func() {
		// Only worth asking when the caller did not say how much it wants.
		if n > 0 {
			statCh <- statResult{}
			return
		}
		a, err := c.Stat(ctx, filePath)
		statCh <- statResult{a, err}
	}()

	opened := <-openCh
	stated := <-statCh
	if opened.err != nil {
		return nil, opened.err
	}
	handle := opened.handle
	defer c.closeHandleAsync(handle)

	// A stat that failed is not fatal: the read can still discover the end of
	// the file for itself, at the cost of one extra request.
	want := n
	sized := false
	if want <= 0 && stated.err == nil && stated.attrs.HasSize() {
		if size := ClampSize(stated.attrs.Size); size > off {
			want, sized = size-off, true
		} else if size <= off {
			return []byte{}, nil
		}
	}

	var out []byte
	batch := pipelineDepth
	if !sized && want <= 0 {
		batch = 1 // unknown length: probe before committing to a wide read
	}
	for {
		remaining := want - int64(len(out))
		if want <= 0 {
			remaining = int64(batch) * maxDataChunk
		}
		if remaining <= 0 {
			break
		}
		if count := int((remaining + maxDataChunk - 1) / maxDataChunk); count < batch {
			batch = count
		}

		parts := make([][]byte, batch)
		errs := make([]error, batch)
		var wg sync.WaitGroup
		for i := 0; i < batch; i++ {
			start := off + int64(len(out)) + int64(i)*maxDataChunk
			chunk := maxDataChunk
			if left := remaining - int64(i)*maxDataChunk; left < int64(chunk) {
				chunk = int(left)
			}
			if chunk <= 0 {
				continue
			}
			wg.Add(1)
			go func(i int, start int64, chunk int) {
				defer wg.Done()
				parts[i], errs[i] = c.readExactly(ctx, handle, uint64(start), chunk)
			}(i, start, chunk)
		}
		wg.Wait()

		short := false
		for i := range parts {
			if errs[i] != nil && !errors.Is(errs[i], io.EOF) {
				return nil, errs[i]
			}
			out = append(out, parts[i]...)
			// A chunk shorter than asked for is the end of the file. Anything
			// after it is empty, and appending past it would splice a hole into
			// the content.
			if len(parts[i]) < maxDataChunk && (want <= 0 || int64(len(out)) < want) {
				short = true
				break
			}
		}
		if short {
			break
		}
		if len(out) == 0 {
			break
		}
	}
	return out, nil
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

// WriteFile replaces a path's contents atomically.
//
// The content goes to a temporary file in the same directory and is renamed
// into place, so a concurrent reader sees either the old file or the new one
// and never a half-written one, and a failed transfer leaves the original
// intact. Same-directory placement keeps the rename within one filesystem,
// which is where POSIX guarantees it is atomic.
func (c *Client) WriteFile(ctx context.Context, filePath string, data []byte, mode uint32) error {
	dir := path.Dir(filePath)

	// Draw a name, and re-draw if the target says it is taken. The exclusive
	// create is what makes a collision detectable at all; retrying is what
	// keeps it from reaching the operator as an error they cannot reproduce.
	var (
		tmp    string
		handle string
		err    error
	)
	for attempt := 0; attempt < waldo.TempAttempts; attempt++ {
		tmp = waldo.TempPath(dir)
		handle, err = c.Open(ctx, tmp,
			flagWrite|flagCreat|flagTrunc|flagExcl,
			Attrs{Flags: attrPermissions, Permissions: mode})
		if err == nil {
			break
		}
		var se *StatusError
		if !errors.As(err, &se) || (se.Code != statusFailure && se.Code != statusPermissionDenied) {
			break // not a collision; the target is telling us something else
		}
	}
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
