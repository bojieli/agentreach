package sftp

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// The tests here drive the client against a scripted server rather than a real
// sshd, because what needs proving is behaviour a real sshd will not produce:
// truncated packets, impossible lengths, responses to requests nobody made.
// Everything on the other end of this protocol is input waldo does not control,
// and the process parsing it holds the operator's credentials — so a malformed
// packet must become an error, never a panic and never a wrong answer.
//
// Correct-path behaviour against a genuine OpenSSH server is covered by
// test/integration.

// fakeServer is a scripted SFTP server over a pair of pipes.
type fakeServer struct {
	toClient   *io.PipeWriter
	fromClient *io.PipeReader

	mu       sync.Mutex
	requests []packet
}

func newFakeServer(t *testing.T, handle func(s *fakeServer, typ byte, id uint32, r *reader)) (*Client, *fakeServer) {
	t.Helper()

	// io.Pipe returns (reader, writer). Two of them: one carrying requests to
	// the server, one carrying responses back.
	serverReader, clientWriter := io.Pipe() // client -> server
	clientReader, serverWriter := io.Pipe() // server -> client

	fs := &fakeServer{toClient: serverWriter, fromClient: serverReader}

	go func() {
		for {
			typ, payload, err := readPacket(fs.fromClient)
			if err != nil {
				return
			}
			if typ == fxpInit {
				var b builder
				b.uint32(protocolVersion)
				b.string(extPOSIXRename)
				b.string("1")
				fs.send(fxpVersion, b.payload())
				continue
			}
			r := &reader{b: payload}
			id := r.uint32()
			fs.mu.Lock()
			fs.requests = append(fs.requests, packet{typ: typ, payload: payload})
			fs.mu.Unlock()
			handle(fs, typ, id, r)
		}
	}()

	c, err := New(clientReader, clientWriter)
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, fs
}

func (s *fakeServer) send(typ byte, payload []byte) {
	hdr := make([]byte, 5)
	binary.BigEndian.PutUint32(hdr, uint32(len(payload))+1)
	hdr[4] = typ
	_, _ = s.toClient.Write(hdr)
	_, _ = s.toClient.Write(payload)
}

func (s *fakeServer) sendStatus(id, code uint32) {
	var b builder
	b.uint32(id)
	b.uint32(code)
	b.string("")
	b.string("")
	s.send(fxpStatus, b.payload())
}

func TestHandshakeReadsExtensions(t *testing.T) {
	c, _ := newFakeServer(t, func(s *fakeServer, _ byte, id uint32, _ *reader) {
		s.sendStatus(id, statusOK)
	})
	if !c.HasPOSIXRename() {
		t.Error("the advertised posix-rename extension was not recorded")
	}
}

// TestHandshakeRejectsHigherVersion: a server must not answer above the version
// the client asked for. Accepting one would mean parsing packets with a layout
// this code does not implement.
func TestHandshakeRejectsHigherVersion(t *testing.T) {
	serverReader, clientWriter := io.Pipe()
	clientReader, serverWriter := io.Pipe()
	go func() {
		_, _, _ = readPacket(serverReader)
		var b builder
		b.uint32(6)
		hdr := make([]byte, 5)
		binary.BigEndian.PutUint32(hdr, uint32(len(b.payload()))+1)
		hdr[4] = fxpVersion
		_, _ = serverWriter.Write(hdr)
		_, _ = serverWriter.Write(b.payload())
	}()
	if _, err := New(clientReader, clientWriter); err == nil {
		t.Fatal("client accepted a server speaking version 6")
	}
}

func TestReadFileReassemblesChunks(t *testing.T) {
	content := make([]byte, maxDataChunk*2+512)
	for i := range content {
		content[i] = byte(i % 251)
	}

	c, _ := newFakeServer(t, func(s *fakeServer, typ byte, id uint32, r *reader) {
		switch typ {
		case fxpOpen:
			var b builder
			b.uint32(id)
			b.string("h")
			s.send(fxpHandle, b.payload())
		case fxpFstat:
			var b builder
			b.uint32(id)
			b.attrs(Attrs{Flags: attrSize, Size: uint64(len(content))})
			s.send(fxpAttrs, b.payload())
		case fxpRead:
			r.string() // handle
			off := r.uint64()
			n := r.uint32()
			if off >= uint64(len(content)) {
				s.sendStatus(id, statusEOF)
				return
			}
			end := off + uint64(n)
			if end > uint64(len(content)) {
				end = uint64(len(content))
			}
			var b builder
			b.uint32(id)
			b.bytes(content[off:end])
			s.send(fxpData, b.payload())
		default:
			s.sendStatus(id, statusOK)
		}
	})

	got, err := c.ReadFile(context.Background(), "/x", 0, 0)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(got) != len(content) {
		t.Fatalf("read %d bytes, want %d", len(got), len(content))
	}
	for i := range content {
		if got[i] != content[i] {
			t.Fatalf("byte %d differs: got %#x want %#x", i, got[i], content[i])
		}
	}
}

// TestShortReadsAreReissued pins the trap in this protocol: a server may return
// fewer bytes than asked for at any time, not only at end of file. Treating a
// short read as EOF silently truncates files, and truncated content still looks
// plausible to whoever reads it next.
func TestShortReadsAreReissued(t *testing.T) {
	content := []byte(strings.Repeat("abcdefgh", 1024)) // 8 KiB

	c, _ := newFakeServer(t, func(s *fakeServer, typ byte, id uint32, r *reader) {
		switch typ {
		case fxpOpen:
			var b builder
			b.uint32(id)
			b.string("h")
			s.send(fxpHandle, b.payload())
		case fxpRead:
			r.string()
			off := r.uint64()
			if off >= uint64(len(content)) {
				s.sendStatus(id, statusEOF)
				return
			}
			// Deliberately dribble out 100 bytes at a time.
			end := off + 100
			if end > uint64(len(content)) {
				end = uint64(len(content))
			}
			var b builder
			b.uint32(id)
			b.bytes(content[off:end])
			s.send(fxpData, b.payload())
		default:
			s.sendStatus(id, statusOK)
		}
	})

	got, err := c.ReadFile(context.Background(), "/x", 0, int64(len(content)))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != string(content) {
		t.Fatalf("short reads were not reissued: got %d bytes, want %d", len(got), len(content))
	}
}

func TestNotFoundIsDistinguishable(t *testing.T) {
	c, _ := newFakeServer(t, func(s *fakeServer, _ byte, id uint32, _ *reader) {
		s.sendStatus(id, statusNoSuchFile)
	})
	_, err := c.Stat(context.Background(), "/missing")
	var se *StatusError
	if !errors.As(err, &se) {
		t.Fatalf("got %T (%v), want *StatusError", err, err)
	}
	if !se.IsNotFound() {
		t.Errorf("status %d not reported as not-found", se.Code)
	}
}

// TestOversizedPacketIsRefused: a length field is four bytes, so a hostile
// server can claim four gigabytes. Allocating that on its word is a denial of
// service against the operator's own machine.
func TestOversizedPacketIsRefused(t *testing.T) {
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], maxPacket+1)
	_, _, err := readPacket(strings.NewReader(string(hdr[:])))
	if err == nil {
		t.Fatal("a packet above the size limit was accepted")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error does not explain the limit: %v", err)
	}
}

// TestUnsolicitedResponseKillsTheConnection is the misattribution guard. If the
// two sides ever disagree about framing, every later response would answer the
// wrong request — which for a file read means returning one file's bytes for
// another. Stopping is the only safe reaction.
func TestUnsolicitedResponseKillsTheConnection(t *testing.T) {
	c, fs := newFakeServer(t, func(s *fakeServer, _ byte, id uint32, _ *reader) {
		// Answer with an id nobody asked for.
		s.sendStatus(id+9999, statusOK)
	})
	_ = fs

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Remove(ctx, "/x"); err == nil {
		t.Fatal("a response to an unknown request id was accepted")
	}
	// Every later request must fail too, rather than reading a stale response.
	if err := c.Remove(ctx, "/y"); err == nil {
		t.Fatal("the client kept using a connection it knows is out of sync")
	}
}

// TestTruncatedPayloadIsAnErrorNotAPanic feeds a response whose declared string
// length runs past the packet.
func TestTruncatedPayloadIsAnErrorNotAPanic(t *testing.T) {
	c, _ := newFakeServer(t, func(s *fakeServer, _ byte, id uint32, _ *reader) {
		var b builder
		b.uint32(id)
		b.uint32(64) // claims a 64-byte handle...
		b.b = append(b.b, 'x')
		s.send(fxpHandle, b.payload()) // ...and supplies one byte
	})
	if _, err := c.Open(context.Background(), "/x", flagRead, Attrs{}); err == nil {
		t.Fatal("a truncated handle response was accepted")
	}
}

func TestContextCancellationIsReported(t *testing.T) {
	c, _ := newFakeServer(t, func(_ *fakeServer, _ byte, _ uint32, _ *reader) {
		// Never answer.
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := c.Remove(ctx, "/x")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want a deadline error", err)
	}
}

func TestClampSizeDoesNotWrap(t *testing.T) {
	for _, tc := range []struct {
		in   uint64
		want int64
	}{
		{0, 0},
		{1024, 1024},
		{1<<63 - 1, 1<<63 - 1},
		{1 << 63, 1<<63 - 1},
		{^uint64(0), 1<<63 - 1},
	} {
		if got := ClampSize(tc.in); got != tc.want {
			t.Errorf("ClampSize(%d) = %d, want %d", tc.in, got, tc.want)
		}
		if ClampSize(tc.in) < 0 {
			t.Errorf("ClampSize(%d) produced a negative length", tc.in)
		}
	}
}

func TestAttrsParsingSkipsExtensions(t *testing.T) {
	var b builder
	b.uint32(attrSize | attrPermissions | attrExtended)
	b.uint64(4096)
	b.uint32(0o100644)
	b.uint32(1) // one extension
	b.string("vendor@example.com")
	b.string("value")

	r := &reader{b: b.payload()}
	a := r.attrs()
	if r.err != nil {
		t.Fatalf("parsing attrs with an extension failed: %v", r.err)
	}
	if a.Size != 4096 {
		t.Errorf("size = %d", a.Size)
	}
	if !a.HasPermissions() || a.Permissions&0o777 != 0o644 {
		t.Errorf("permissions = %#o", a.Permissions)
	}
}
