package sftp

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
)

// fxpExtended carries vendor extensions. waldo uses exactly one, and only when
// the server advertises it.
const fxpExtended = 200

// extPOSIXRename is OpenSSH's rename-with-overwrite. Plain SFTP v3 rename fails
// when the destination exists, which would make every atomic write a
// remove-then-rename with a window where the file does not exist at all. When
// the server offers this extension waldo uses it and the window disappears.
const extPOSIXRename = "posix-rename@openssh.com"

// Client is an SFTP version 3 client speaking over any byte stream — in waldo's
// case, the stdin/stdout of `ssh -s <host> sftp`.
type Client struct {
	w io.WriteCloser
	r io.Reader

	writeMu sync.Mutex

	mu      sync.Mutex
	nextID  uint32
	pending map[uint32]chan packet
	// fatal records why the connection died, so a request that arrives after
	// the reader has given up gets that reason rather than blocking forever.
	fatal error

	exts map[string]bool

	closeOnce sync.Once
	done      chan struct{}
}

type packet struct {
	typ     byte
	payload []byte
}

// New performs the version handshake and starts the response reader.
func New(r io.Reader, w io.WriteCloser) (*Client, error) {
	c := &Client{
		w:       w,
		r:       r,
		pending: make(map[uint32]chan packet),
		exts:    make(map[string]bool),
		done:    make(chan struct{}),
	}

	var b builder
	b.uint32(protocolVersion)
	if err := c.writePacket(fxpInit, b.payload()); err != nil {
		return nil, fmt.Errorf("sftp: send init: %w", err)
	}
	typ, payload, err := readPacket(r)
	if err != nil {
		return nil, fmt.Errorf("sftp: no version response: %w", err)
	}
	if typ != fxpVersion {
		return nil, fmt.Errorf("sftp: expected version packet, got type %d", typ)
	}
	rd := &reader{b: payload}
	if v := rd.uint32(); v > protocolVersion {
		// A server must not answer with a version above the client's request.
		return nil, fmt.Errorf("sftp: server offered version %d, waldo speaks %d", v, protocolVersion)
	}
	for rd.err == nil && len(rd.b) > 0 {
		name := rd.string()
		rd.bytes() // extension data, unused
		if rd.err == nil {
			c.exts[name] = true
		}
	}

	go c.readLoop()
	return c, nil
}

// HasPOSIXRename reports whether atomic overwrite-rename is available.
func (c *Client) HasPOSIXRename() bool { return c.exts[extPOSIXRename] }

func (c *Client) readLoop() {
	for {
		typ, payload, err := readPacket(c.r)
		if err != nil {
			c.shutdown(err)
			return
		}
		if len(payload) < 4 {
			c.shutdown(fmt.Errorf("sftp: response type %d is too short to carry a request id", typ))
			return
		}
		id := binary.BigEndian.Uint32(payload)

		c.mu.Lock()
		ch, ok := c.pending[id]
		delete(c.pending, id)
		c.mu.Unlock()
		if !ok {
			// A response to a request nobody is waiting for means the two sides
			// disagree about the stream's framing, and every later response
			// would be misattributed — which for a file transfer means silently
			// returning one file's bytes for another. Stop.
			c.shutdown(fmt.Errorf("sftp: unsolicited response for request %d", id))
			return
		}
		ch <- packet{typ: typ, payload: payload[4:]}
	}
}

// shutdown fails every waiting request with a single cause.
func (c *Client) shutdown(cause error) {
	c.mu.Lock()
	if c.fatal == nil {
		c.fatal = cause
	}
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
	c.mu.Unlock()
	c.closeOnce.Do(func() { close(c.done) })
}

// Close releases the connection.
func (c *Client) Close() error {
	c.shutdown(errors.New("sftp: client closed"))
	return c.w.Close()
}

func (c *Client) writePacket(typ byte, payload []byte) error {
	hdr := make([]byte, 5)
	binary.BigEndian.PutUint32(hdr, uint32(len(payload))+1)
	hdr[4] = typ

	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if _, err := c.w.Write(hdr); err != nil {
		return err
	}
	if len(payload) == 0 {
		return nil
	}
	_, err := c.w.Write(payload)
	return err
}

func readPacket(r io.Reader) (byte, []byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 {
		return 0, nil, errors.New("sftp: zero-length packet")
	}
	if n > maxPacket {
		return 0, nil, fmt.Errorf("sftp: packet of %d bytes exceeds the %d-byte limit", n, maxPacket)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return 0, nil, err
	}
	return buf[0], buf[1:], nil
}

// request sends one request and waits for its response.
func (c *Client) request(ctx context.Context, typ byte, build func(*builder)) (packet, error) {
	c.mu.Lock()
	if c.fatal != nil {
		err := c.fatal
		c.mu.Unlock()
		return packet{}, err
	}
	c.nextID++
	id := c.nextID
	ch := make(chan packet, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	var b builder
	b.uint32(id)
	build(&b)

	if err := c.writePacket(typ, b.payload()); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return packet{}, fmt.Errorf("sftp: send request: %w", err)
	}

	select {
	case p, ok := <-ch:
		if !ok {
			c.mu.Lock()
			err := c.fatal
			c.mu.Unlock()
			if err == nil {
				err = errors.New("sftp: connection closed")
			}
			return packet{}, err
		}
		return p, nil
	case <-ctx.Done():
		// The request id stays registered: its response will still arrive and
		// must be consumed by readLoop, or it would be misread as the answer to
		// a later request. Leaving the entry lets readLoop discard it into the
		// buffered channel and move on.
		return packet{}, ctx.Err()
	case <-c.done:
		c.mu.Lock()
		err := c.fatal
		c.mu.Unlock()
		if err == nil {
			err = errors.New("sftp: connection closed")
		}
		return packet{}, err
	}
}

// expectStatus issues a request whose only success answer is STATUS(OK).
func (c *Client) expectStatus(ctx context.Context, typ byte, build func(*builder)) error {
	p, err := c.request(ctx, typ, build)
	if err != nil {
		return err
	}
	if p.typ != fxpStatus {
		return fmt.Errorf("sftp: expected a status response, got type %d", p.typ)
	}
	return statusFrom(p.payload)
}

func statusFrom(payload []byte) error {
	rd := &reader{b: payload}
	code := rd.uint32()
	msg := rd.string()
	if code == statusOK {
		return nil
	}
	return &StatusError{Code: code, Message: msg}
}

// Open opens a file and returns its handle.
func (c *Client) Open(ctx context.Context, path string, pflags uint32, a Attrs) (string, error) {
	p, err := c.request(ctx, fxpOpen, func(b *builder) {
		b.string(path)
		b.uint32(pflags)
		b.attrs(a)
	})
	if err != nil {
		return "", err
	}
	switch p.typ {
	case fxpHandle:
		rd := &reader{b: p.payload}
		h := rd.string()
		return h, rd.err
	case fxpStatus:
		return "", statusFrom(p.payload)
	}
	return "", fmt.Errorf("sftp: unexpected response type %d to open", p.typ)
}

// CloseHandle releases a file or directory handle.
func (c *Client) CloseHandle(ctx context.Context, handle string) error {
	return c.expectStatus(ctx, fxpClose, func(b *builder) { b.string(handle) })
}

// closeHandleAsync releases a handle without waiting for the server to confirm.
//
// A close is the last round trip of every read, and its result changes nothing:
// the bytes are already in hand, and a server that fails to close a read handle
// has a problem waldo cannot act on anyway. Waiting for it doubles the cost of
// reading a small file over a link where a round trip is the entire expense.
//
// The response still has to be *consumed*. Abandoning the request id would
// leave a reply nobody is expecting, and this client deliberately treats that
// as a framing disagreement and tears the connection down — rightly, since
// otherwise a later read could be answered with another file's bytes. So the
// wait is moved to a goroutine rather than skipped.
func (c *Client) closeHandleAsync(handle string) {
	go func() {
		_ = c.CloseHandle(context.WithoutCancel(context.Background()), handle)
	}()
}

// ReadAt reads up to n bytes at off. It returns io.EOF at end of file.
func (c *Client) ReadAt(ctx context.Context, handle string, off uint64, n uint32) ([]byte, error) {
	if n > maxDataChunk {
		n = maxDataChunk
	}
	p, err := c.request(ctx, fxpRead, func(b *builder) {
		b.string(handle)
		b.uint64(off)
		b.uint32(n)
	})
	if err != nil {
		return nil, err
	}
	switch p.typ {
	case fxpData:
		rd := &reader{b: p.payload}
		data := rd.bytes()
		if rd.err != nil {
			return nil, rd.err
		}
		out := make([]byte, len(data))
		copy(out, data)
		return out, nil
	case fxpStatus:
		if err := statusFrom(p.payload); err != nil {
			var se *StatusError
			if errors.As(err, &se) && se.Code == statusEOF {
				return nil, io.EOF
			}
			return nil, err
		}
		return nil, io.EOF
	}
	return nil, fmt.Errorf("sftp: unexpected response type %d to read", p.typ)
}

// WriteAt writes data at off.
func (c *Client) WriteAt(ctx context.Context, handle string, off uint64, data []byte) error {
	return c.expectStatus(ctx, fxpWrite, func(b *builder) {
		b.string(handle)
		b.uint64(off)
		b.bytes(data)
	})
}

// Stat follows symlinks; Lstat does not.
func (c *Client) Stat(ctx context.Context, path string) (Attrs, error) {
	return c.statLike(ctx, fxpStat, path)
}

// Lstat describes a path without following a final symlink.
func (c *Client) Lstat(ctx context.Context, path string) (Attrs, error) {
	return c.statLike(ctx, fxpLstat, path)
}

func (c *Client) statLike(ctx context.Context, typ byte, path string) (Attrs, error) {
	p, err := c.request(ctx, typ, func(b *builder) { b.string(path) })
	if err != nil {
		return Attrs{}, err
	}
	switch p.typ {
	case fxpAttrs:
		rd := &reader{b: p.payload}
		a := rd.attrs()
		return a, rd.err
	case fxpStatus:
		return Attrs{}, statusFrom(p.payload)
	}
	return Attrs{}, fmt.Errorf("sftp: unexpected response type %d to stat", p.typ)
}

// Fstat describes an open handle.
func (c *Client) Fstat(ctx context.Context, handle string) (Attrs, error) {
	p, err := c.request(ctx, fxpFstat, func(b *builder) { b.string(handle) })
	if err != nil {
		return Attrs{}, err
	}
	switch p.typ {
	case fxpAttrs:
		rd := &reader{b: p.payload}
		a := rd.attrs()
		return a, rd.err
	case fxpStatus:
		return Attrs{}, statusFrom(p.payload)
	}
	return Attrs{}, fmt.Errorf("sftp: unexpected response type %d to fstat", p.typ)
}

// Chmod sets a path's permission bits.
func (c *Client) Chmod(ctx context.Context, path string, mode uint32) error {
	return c.expectStatus(ctx, fxpSetstat, func(b *builder) {
		b.string(path)
		b.attrs(Attrs{Flags: attrPermissions, Permissions: mode})
	})
}

// DirEntry is one entry from a directory listing.
type DirEntry struct {
	Name  string
	Attrs Attrs
}

// ReadDir lists a directory in full.
func (c *Client) ReadDir(ctx context.Context, path string) ([]DirEntry, error) {
	handle, err := c.opendir(ctx, path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.CloseHandle(context.WithoutCancel(ctx), handle) }()

	var out []DirEntry
	for {
		p, err := c.request(ctx, fxpReaddir, func(b *builder) { b.string(handle) })
		if err != nil {
			return nil, err
		}
		if p.typ == fxpStatus {
			serr := statusFrom(p.payload)
			var se *StatusError
			if serr == nil || (errors.As(serr, &se) && se.Code == statusEOF) {
				return out, nil
			}
			return nil, serr
		}
		if p.typ != fxpName {
			return nil, fmt.Errorf("sftp: unexpected response type %d to readdir", p.typ)
		}
		rd := &reader{b: p.payload}
		count := rd.uint32()
		for i := uint32(0); i < count && rd.err == nil; i++ {
			name := rd.string()
			rd.bytes() // longname, an ls -l rendering waldo does not parse
			a := rd.attrs()
			if name == "." || name == ".." {
				continue
			}
			out = append(out, DirEntry{Name: name, Attrs: a})
		}
		if rd.err != nil {
			return nil, rd.err
		}
	}
}

func (c *Client) opendir(ctx context.Context, path string) (string, error) {
	p, err := c.request(ctx, fxpOpendir, func(b *builder) { b.string(path) })
	if err != nil {
		return "", err
	}
	switch p.typ {
	case fxpHandle:
		rd := &reader{b: p.payload}
		h := rd.string()
		return h, rd.err
	case fxpStatus:
		return "", statusFrom(p.payload)
	}
	return "", fmt.Errorf("sftp: unexpected response type %d to opendir", p.typ)
}

// Remove deletes a file.
func (c *Client) Remove(ctx context.Context, path string) error {
	return c.expectStatus(ctx, fxpRemove, func(b *builder) { b.string(path) })
}

// Rmdir removes an empty directory.
func (c *Client) Rmdir(ctx context.Context, path string) error {
	return c.expectStatus(ctx, fxpRmdir, func(b *builder) { b.string(path) })
}

// Mkdir creates a directory.
func (c *Client) Mkdir(ctx context.Context, path string, mode uint32) error {
	return c.expectStatus(ctx, fxpMkdir, func(b *builder) {
		b.string(path)
		b.attrs(Attrs{Flags: attrPermissions, Permissions: mode})
	})
}

// Rename moves a path, overwriting the destination when the server supports
// POSIX rename semantics.
func (c *Client) Rename(ctx context.Context, from, to string) error {
	if c.HasPOSIXRename() {
		return c.expectStatus(ctx, fxpExtended, func(b *builder) {
			b.string(extPOSIXRename)
			b.string(from)
			b.string(to)
		})
	}
	// Plain v3 rename refuses an existing destination. Removing it first opens
	// a window in which the file does not exist; that is worse than atomic, and
	// it is why waldo prefers the extension. The caller is told which happened
	// through Atomic().
	err := c.expectStatus(ctx, fxpRename, func(b *builder) {
		b.string(from)
		b.string(to)
	})
	if err == nil {
		return nil
	}
	var se *StatusError
	if !errors.As(err, &se) || se.Code != statusFailure {
		return err
	}
	if rmErr := c.Remove(ctx, to); rmErr != nil {
		return err
	}
	return c.expectStatus(ctx, fxpRename, func(b *builder) {
		b.string(from)
		b.string(to)
	})
}

// Readlink resolves a symlink's target.
func (c *Client) Readlink(ctx context.Context, path string) (string, error) {
	return c.nameOne(ctx, fxpReadlink, path)
}

// Realpath canonicalises a path on the server.
func (c *Client) Realpath(ctx context.Context, path string) (string, error) {
	return c.nameOne(ctx, fxpRealpath, path)
}

func (c *Client) nameOne(ctx context.Context, typ byte, path string) (string, error) {
	p, err := c.request(ctx, typ, func(b *builder) { b.string(path) })
	if err != nil {
		return "", err
	}
	switch p.typ {
	case fxpName:
		rd := &reader{b: p.payload}
		if rd.uint32() < 1 {
			return "", errors.New("sftp: empty name response")
		}
		name := rd.string()
		return name, rd.err
	case fxpStatus:
		return "", statusFrom(p.payload)
	}
	return "", fmt.Errorf("sftp: unexpected response type %d", p.typ)
}
