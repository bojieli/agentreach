package mirror

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bojieli/waldo/internal/fileops"
	"github.com/bojieli/waldo/internal/transport"
)

func newTestMirror(t *testing.T) (*Mirror, string) {
	t.Helper()
	tr := localTransport(t)
	caps, err := fileops.Probe(context.Background(), tr)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	target := t.TempDir() // stands in for the remote filesystem
	return New(t.TempDir(), fileops.NewPOSIX(tr, caps)), target
}

func TestFetchAndPushRoundTrip(t *testing.T) {
	m, target := newTestMirror(t)
	ctx := context.Background()
	remote := filepath.Join(target, "f.txt")
	if err := os.WriteFile(remote, []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	local, err := m.Fetch(ctx, remote)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got, _ := os.ReadFile(local); string(got) != "original\n" {
		t.Fatalf("mirrored content = %q", got)
	}

	if err := os.WriteFile(local, []byte("edited\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Push(ctx, remote); err != nil {
		t.Fatalf("push: %v", err)
	}
	if got, _ := os.ReadFile(remote); string(got) != "edited\n" {
		t.Fatalf("target content = %q want edited", got)
	}
}

// TestPushRefusesWhenTargetChanged is the property that prevents silent data
// loss: if the file moved under us between read and write, overwriting it with
// a stale base would destroy whatever changed it.
func TestPushRefusesWhenTargetChanged(t *testing.T) {
	m, target := newTestMirror(t)
	ctx := context.Background()
	remote := filepath.Join(target, "f.txt")
	if err := os.WriteFile(remote, []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	local, err := m.Fetch(ctx, remote)
	if err != nil {
		t.Fatal(err)
	}
	// The agent edits its copy...
	if err := os.WriteFile(local, []byte("agent edit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// ...while something else changes the target.
	if err := os.WriteFile(remote, []byte("someone else\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err = m.Push(ctx, remote)
	if err == nil {
		t.Fatal("push overwrote a file that changed on the target")
	}
	if !strings.Contains(err.Error(), "changed on the target") {
		t.Errorf("unhelpful error: %v", err)
	}
	if got, _ := os.ReadFile(remote); string(got) != "someone else\n" {
		t.Errorf("target was clobbered: %q", got)
	}
}

func TestPushRefusesWhenTargetDeleted(t *testing.T) {
	m, target := newTestMirror(t)
	ctx := context.Background()
	remote := filepath.Join(target, "f.txt")
	if err := os.WriteFile(remote, []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	local, err := m.Fetch(ctx, remote)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(local, []byte("edited\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(remote); err != nil {
		t.Fatal(err)
	}

	if err := m.Push(ctx, remote); err == nil {
		t.Fatal("push recreated a file that was deleted on the target")
	}
}

// TestPrepareAllowsCreatingNewFile covers Write on a path that does not exist
// yet, which must not be treated as an error.
func TestPrepareAllowsCreatingNewFile(t *testing.T) {
	m, target := newTestMirror(t)
	ctx := context.Background()
	remote := filepath.Join(target, "sub", "new.txt")

	local, err := m.Prepare(ctx, remote)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if err := os.WriteFile(local, []byte("brand new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The parent directory does not exist on the target either.
	if err := os.MkdirAll(filepath.Dir(remote), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := m.Push(ctx, remote); err != nil {
		t.Fatalf("push: %v", err)
	}
	if got, _ := os.ReadFile(remote); string(got) != "brand new\n" {
		t.Fatalf("target content = %q", got)
	}
}

func TestPrepareRefusesWhenFileAppearsUnderneath(t *testing.T) {
	m, target := newTestMirror(t)
	ctx := context.Background()
	remote := filepath.Join(target, "race.txt")

	local, err := m.Prepare(ctx, remote) // absent at this point
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(local, []byte("agent version\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(remote, []byte("appeared\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := m.Push(ctx, remote); err == nil {
		t.Fatal("push clobbered a file created on the target after Prepare")
	}
}

func TestPathMappingIsReversible(t *testing.T) {
	// The mirror root is a path on the operator's own machine, so it is spelled
	// in that machine's convention: on Windows filepath.Join yields
	// \mirror\root\..., which no forward-slash prefix can match. The property
	// under test is that the mapping stays inside the root and reverses exactly,
	// not how a separator is spelled — and the round trip below already passed
	// on Windows while this prefix check did not.
	root := filepath.FromSlash("/mirror/root")
	m := New(root, nil)
	for _, p := range []string{"/srv/app/main.go", "/a", "/deep/nested/path/file.txt"} {
		local := m.Local(p)
		if !strings.HasPrefix(local, root) {
			t.Errorf("Local(%q) = %q escaped the mirror root", p, local)
		}
		back, ok := m.Target(local)
		if !ok || back != p {
			t.Errorf("round trip %q -> %q -> %q (ok=%v)", p, local, back, ok)
		}
	}
	if _, ok := m.Target(filepath.FromSlash("/somewhere/else/file")); ok {
		t.Error("a path outside the mirror was accepted as mirrored")
	}
}

// TestPathTraversalIsRefused covers a real attack path: file paths can come
// from content read off an untrusted target, and a ".." segment would otherwise
// escape the mirror root once filepath.Join normalised it — letting a hostile
// target make waldo read or overwrite arbitrary files on the operator's machine.
func TestPathTraversalIsRefused(t *testing.T) {
	m, target := newTestMirror(t)
	ctx := context.Background()

	for _, evil := range []string{
		"/../../../etc/passwd",
		"/srv/app/../../../../etc/passwd",
		"/srv/../../root/.ssh/authorized_keys",
	} {
		local := m.Local(evil)
		rel, err := filepath.Rel(m.Root(), local)
		if err != nil || strings.HasPrefix(rel, "..") {
			t.Errorf("Local(%q) = %q escaped the mirror root", evil, local)
		}
	}

	// A legitimate path with interior ".." that stays inside must still work.
	realPath := filepath.Join(target, "sub", "..", "ok.txt")
	if err := os.WriteFile(filepath.Join(target, "ok.txt"), []byte("fine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Fetch(ctx, realPath); err != nil {
		t.Errorf("legitimate path with interior .. was rejected: %v", err)
	}
}

func TestCheckContainedRejectsOutsidePaths(t *testing.T) {
	m := New(t.TempDir(), nil)
	if err := m.checkContained("/etc/passwd"); err == nil {
		t.Error("a path outside the mirror root was accepted")
	}
	if err := m.checkContained(filepath.Join(m.Root(), "a", "b")); err != nil {
		t.Errorf("a path inside the mirror root was rejected: %v", err)
	}
}

// localTransport builds a local:// transport, skipping when this machine cannot
// be a target. A local target needs a POSIX shell, which Windows only has when
// Git for Windows or MSYS2 supplied one — and a Windows machine is never a
// supported target, so skipping is the honest outcome rather than a failure.
func localTransport(t *testing.T) transport.Transport {
	t.Helper()
	tr, err := transport.NewLocal()
	if err != nil {
		t.Skipf("this machine cannot host a local:// target: %v", err)
	}
	return tr
}
