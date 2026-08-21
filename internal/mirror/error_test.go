package mirror

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"testing"
)

// errOps is a FileOps that returns a configurable error on Read and Write.
// Other methods delegate to fakeOps (which always succeeds).
type errOps struct {
	fakeOps
	readErr  error // returned by every Read call, or nil for fakeOps behaviour
	writeErr error // returned by every Write call, or nil for fakeOps behaviour
}

func (e errOps) Read(ctx context.Context, p string, off, lim int64) ([]byte, error) {
	if e.readErr != nil {
		return nil, e.readErr
	}
	return e.fakeOps.Read(ctx, p, off, lim)
}

func (e errOps) Write(ctx context.Context, p string, data []byte, mode fs.FileMode) error {
	if e.writeErr != nil {
		return e.writeErr
	}
	return e.fakeOps.Write(ctx, p, data, mode)
}

// TestFetchPropagatesReadError verifies that a transport-level read failure
// surfaces from Fetch rather than being silently swallowed.
func TestFetchPropagatesReadError(t *testing.T) {
	sentinel := errors.New("connection refused")
	m := New(t.TempDir(), errOps{readErr: sentinel})

	_, err := m.Fetch(context.Background(), "/srv/app/file.go")
	if err == nil {
		t.Fatal("Fetch succeeded despite read error")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("Fetch error = %v; want to wrap %v", err, sentinel)
	}
}

// TestPushWithoutPriorFetchWritesThrough verifies the documented "no record →
// skip verification" behaviour. If a file is written to the local mirror
// without a prior Fetch or Prepare, Push must still write it to the target
// rather than failing. This is the correct policy: we have no baseline to
// compare, so we cannot guard, but we must not silently discard the edit.
//
// This test makes the behaviour explicit so a future refactor cannot regress
// it silently.
func TestPushWithoutPriorFetchWritesThrough(t *testing.T) {
	m, target := newTestMirror(t)
	ctx := context.Background()
	remote := fmt.Sprintf("%s/fresh.txt", target)

	local := m.Local(remote)
	if err := os.MkdirAll(local[:len(local)-len("/fresh.txt")], 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(local, []byte("from scratch\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// No Fetch or Prepare — no digest record exists.
	if err := m.Push(ctx, remote); err != nil {
		t.Fatalf("Push without prior Fetch should succeed (no record = no guard): %v", err)
	}
	got, _ := os.ReadFile(remote)
	if string(got) != "from scratch\n" {
		t.Errorf("target content = %q; want %q", got, "from scratch\n")
	}
}

// TestPushVerificationReadErrorIsRefused covers the path where fo.Read fails
// with a non-NotFoundError during digest verification. Push must propagate the
// error rather than write blindly: if the transport is broken, we cannot tell
// whether the target file changed since our last read, and overwriting it with
// a potentially stale copy would be silent data loss.
func TestPushVerificationReadErrorIsRefused(t *testing.T) {
	// Phase 1: Fetch with fakeOps to create a digest record.
	root := t.TempDir()
	good := New(root, fakeOps{})
	ctx := context.Background()
	path := "/srv/app/verified.go"

	if _, err := good.Fetch(ctx, path); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if _, known := good.expectedDigest(path); !known {
		t.Fatal("Fetch should record a digest")
	}

	// Phase 2: Push with errOps that fails on Read (simulating a network error
	// during the verification step). The same root gives access to the digest
	// record written in Phase 1.
	networkErr := errors.New("i/o timeout")
	bad := New(root, errOps{readErr: networkErr})

	// Write a local file so Push has something to send.
	local := bad.Local(path)
	if err := os.MkdirAll(local[:strings.LastIndex(local, "/")], 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(local, []byte("edit\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := bad.Push(ctx, path)
	if err == nil {
		t.Fatal("Push should have refused when verification Read failed")
	}
	if !strings.Contains(err.Error(), "verify") {
		t.Errorf("Push error should mention verification context: %v", err)
	}
	if !errors.Is(err, networkErr) {
		t.Errorf("Push error should wrap the network error; got %v", err)
	}
}
