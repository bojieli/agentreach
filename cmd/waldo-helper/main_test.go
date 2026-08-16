package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// The agent is the only thing waldo ever writes to a target, and it parses
// framed input for a living. These tests drive serve() directly over pipes,
// which is the whole surface: no socket, no configuration, no state.
//
// The interchangeability of this tier with the other three is proved elsewhere,
// by the shared conformance suite in internal/fileops/fileopstest. What is
// tested here is the framing itself, and the promise that a bad request becomes
// a response rather than a dead agent — an agent that exits on one bad path
// takes the whole session's file access with it.

type conversation struct {
	t   *testing.T
	in  *io.PipeWriter
	out *io.PipeReader
	err chan error
}

func start(t *testing.T) *conversation {
	t.Helper()
	serverIn, clientOut := io.Pipe()
	clientIn, serverOut := io.Pipe()

	c := &conversation{t: t, in: clientOut, out: clientIn, err: make(chan error, 1)}
	go func() { c.err <- serve(serverIn, serverOut) }()
	t.Cleanup(func() {
		_ = clientOut.Close()
		_ = clientIn.Close()
	})
	return c
}

func (c *conversation) send(req map[string]any, payload []byte) {
	c.t.Helper()
	hdr, err := json.Marshal(req)
	if err != nil {
		c.t.Fatal(err)
	}
	var frame []byte
	frame = binary.BigEndian.AppendUint32(frame, uint32(len(hdr)))
	frame = append(frame, hdr...)
	frame = binary.BigEndian.AppendUint32(frame, uint32(len(payload)))
	frame = append(frame, payload...)
	if _, err := c.in.Write(frame); err != nil {
		c.t.Fatalf("write request: %v", err)
	}
}

func (c *conversation) recv() (map[string]any, []byte) {
	c.t.Helper()
	var lenBuf [4]byte
	if _, err := io.ReadFull(c.out, lenBuf[:]); err != nil {
		c.t.Fatalf("read header length: %v", err)
	}
	hdr := make([]byte, binary.BigEndian.Uint32(lenBuf[:]))
	if _, err := io.ReadFull(c.out, hdr); err != nil {
		c.t.Fatalf("read header: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(hdr, &parsed); err != nil {
		c.t.Fatalf("parse header %q: %v", hdr, err)
	}
	if _, err := io.ReadFull(c.out, lenBuf[:]); err != nil {
		c.t.Fatalf("read payload length: %v", err)
	}
	payload := make([]byte, binary.BigEndian.Uint32(lenBuf[:]))
	if len(payload) > 0 {
		if _, err := io.ReadFull(c.out, payload); err != nil {
			c.t.Fatalf("read payload: %v", err)
		}
	}
	return parsed, payload
}

func (c *conversation) do(req map[string]any, payload []byte) (map[string]any, []byte) {
	c.t.Helper()
	c.send(req, payload)
	return c.recv()
}

func mustOK(t *testing.T, hdr map[string]any) {
	t.Helper()
	if ok, _ := hdr["ok"].(bool); !ok {
		t.Fatalf("request failed: %v", hdr["error"])
	}
}

func TestPingIdentifiesTheAgent(t *testing.T) {
	c := start(t)
	hdr, _ := c.do(map[string]any{"id": 1, "op": "ping"}, nil)
	mustOK(t, hdr)
	if helper, _ := hdr["helper"].(bool); !helper {
		t.Error("ping response does not identify itself as the helper")
	}
	if id, _ := hdr["id"].(float64); id != 1 {
		t.Errorf("response id = %v, want 1", hdr["id"])
	}
}

func TestWriteThenReadRoundTripsBinary(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "file.bin")
	content := []byte{0x00, 0xff, 0xfe, '\r', '\n', 0x80, 'a', 0x00}

	c := start(t)
	hdr, _ := c.do(map[string]any{"id": 1, "op": "write", "path": target, "mode": 0o600}, content)
	mustOK(t, hdr)

	hdr, payload := c.do(map[string]any{"id": 2, "op": "read", "path": target}, nil)
	mustOK(t, hdr)
	if !bytes.Equal(payload, content) {
		t.Fatalf("round trip corrupted content: got % x want % x", payload, content)
	}

	fi, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	// Asserted only where it means something. Windows has no POSIX permission
	// bits, so Go reports 0666 for any writable file — and the helper never runs
	// there: it exists to be copied onto a target, and every release archive
	// carries linux and darwin helpers only, including the one a Windows
	// operator downloads.
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
}

// TestWriteIsAtomic checks that the temporary file is gone afterwards. A write
// that leaves debris behind breaks the promise that waldo's footprint on a
// target is exactly one file in one directory.
func TestWriteIsAtomic(t *testing.T) {
	dir := t.TempDir()
	c := start(t)
	hdr, _ := c.do(map[string]any{"id": 1, "op": "write",
		"path": filepath.Join(dir, "f"), "mode": 0o644}, []byte("data"))
	mustOK(t, hdr)

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "f" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("directory contains %v, want just [f]", names)
	}
}

func TestReadRangesAndOffsets(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "ranges")
	if err := os.WriteFile(target, []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := start(t)
	for i, tc := range []struct {
		off, limit int
		want       string
	}{
		{0, 4, "0123"},
		{4, 3, "456"},
		{8, 0, "89"},
		{20, 5, ""},
	} {
		hdr, payload := c.do(map[string]any{
			"id": i + 1, "op": "read", "path": target,
			"offset": tc.off, "limit": tc.limit,
		}, nil)
		mustOK(t, hdr)
		if string(payload) != tc.want {
			t.Errorf("read(off=%d limit=%d) = %q, want %q", tc.off, tc.limit, payload, tc.want)
		}
	}
}

// TestMissingFileIsNotFoundNotEmpty is the distinction an agent acts on: told a
// file is empty, it concludes the code it wanted does not exist.
func TestMissingFileIsNotFoundNotEmpty(t *testing.T) {
	c := start(t)
	hdr, payload := c.do(map[string]any{
		"id": 1, "op": "read", "path": filepath.Join(t.TempDir(), "absent"),
	}, nil)
	if ok, _ := hdr["ok"].(bool); ok {
		t.Fatalf("reading a missing file succeeded with %d bytes", len(payload))
	}
	if kind, _ := hdr["kind"].(string); kind != "notfound" {
		t.Errorf("kind = %q, want notfound", kind)
	}
}

func TestHashMatchesContent(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "h")
	content := []byte("hash me\x00\xff")
	if err := os.WriteFile(target, content, 0o644); err != nil {
		t.Fatal(err)
	}
	c := start(t)
	hdr, _ := c.do(map[string]any{"id": 1, "op": "hash", "path": target}, nil)
	mustOK(t, hdr)
	sum := sha256.Sum256(content)
	if got, _ := hdr["digest"].(string); got != hex.EncodeToString(sum[:]) {
		t.Errorf("digest = %s, want %s", got, hex.EncodeToString(sum[:]))
	}
}

func TestStatAndList(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a"), []byte("aa"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	c := start(t)

	hdr, _ := c.do(map[string]any{"id": 1, "op": "stat", "path": filepath.Join(dir, "a")}, nil)
	mustOK(t, hdr)
	info, _ := hdr["info"].(map[string]any)
	if size, _ := info["size"].(float64); size != 2 {
		t.Errorf("size = %v, want 2", info["size"])
	}
	if isDir, _ := info["is_dir"].(bool); isDir {
		t.Error("a regular file was reported as a directory")
	}

	hdr, _ = c.do(map[string]any{"id": 2, "op": "list", "path": dir}, nil)
	mustOK(t, hdr)
	entries, _ := hdr["entries"].([]any)
	if len(entries) != 2 {
		t.Fatalf("listed %d entries, want 2", len(entries))
	}
}

// TestBadRequestsDoNotKillTheAgent is the property that matters most here: the
// agent must survive every kind of bad request, because it holds the session's
// entire file-access capability and there is nothing to restart it.
func TestBadRequestsDoNotKillTheAgent(t *testing.T) {
	c := start(t)

	hdr, _ := c.do(map[string]any{"id": 1, "op": "nonsense"}, nil)
	if ok, _ := hdr["ok"].(bool); ok {
		t.Error("an unknown op was reported as successful")
	}

	hdr, _ = c.do(map[string]any{"id": 2, "op": "read", "path": "/definitely/not/here"}, nil)
	if ok, _ := hdr["ok"].(bool); ok {
		t.Error("reading a missing path was reported as successful")
	}

	hdr, _ = c.do(map[string]any{"id": 3, "op": "write", "path": "/proc/definitely/not/writable"}, []byte("x"))
	if ok, _ := hdr["ok"].(bool); ok {
		t.Error("an impossible write was reported as successful")
	}

	// Still answering afterwards.
	hdr, _ = c.do(map[string]any{"id": 4, "op": "ping"}, nil)
	mustOK(t, hdr)
}

// TestOversizedFrameIsRefused: the length prefix is attacker-controlled in the
// sense that waldo does not want a corrupt stream turning into a huge
// allocation on someone else's server.
func TestOversizedFrameIsRefused(t *testing.T) {
	c := start(t)
	var frame []byte
	frame = binary.BigEndian.AppendUint32(frame, maxFrame+1)
	if _, err := c.in.Write(frame); err != nil {
		t.Fatal(err)
	}
	_ = c.in.Close()
	if err := <-c.err; err == nil {
		t.Fatal("a frame above the limit was accepted")
	}
}

func TestCleanEOFEndsTheSession(t *testing.T) {
	c := start(t)
	hdr, _ := c.do(map[string]any{"id": 1, "op": "ping"}, nil)
	mustOK(t, hdr)
	_ = c.in.Close()
	if err := <-c.err; err != nil && err != io.EOF {
		t.Fatalf("closing the channel should end the agent cleanly, got %v", err)
	}
}
