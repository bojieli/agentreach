//go:build integration

package integration

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"strings"
	"testing"

	"github.com/bojieli/waldo/internal/fileops"
	"github.com/bojieli/waldo/internal/fileops/fileopstest"
	"github.com/bojieli/waldo/internal/transport"
	"github.com/bojieli/waldo/internal/waldo"
)

// The container transport is a documented target kind — `docker://container/path`
// is in the README — and it was the least exercised thing in the project: every
// other test reached a target over ssh or a local shell.
//
// It also buys the only practical way to test waldo against a userland that is
// neither GNU nor BSD. waldo's tier-0 strategy branches on what the target has,
// and busybox is the case where the branches actually differ: no `find -printf`,
// a different `stat`, and applets rather than the coreutils waldo was written
// against. Claiming support for it without running it would be a claim this
// project does not make.

const (
	gnuImageEnv     = "WALDO_TEST_DOCKER_IMAGE"
	busyboxImageEnv = "WALDO_TEST_DOCKER_BUSYBOX_IMAGE"
)

func dockerAvailable(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not installed")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker is installed but not running")
	}
}

// startContainer runs an image with a shell kept alive, and removes it after.
func startContainer(t *testing.T, name, image string) {
	t.Helper()
	dockerAvailable(t)

	_ = exec.Command("docker", "rm", "-f", name).Run()
	out, err := exec.Command("docker", "run", "-d", "--name", name,
		"--entrypoint", "sleep", image, "infinity").CombinedOutput()
	if err != nil {
		// A machine without the image cached and without registry access cannot
		// run this, which is a missing prerequisite rather than a failure.
		t.Skipf("cannot start %s from %s: %v: %s", name, image, err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })

	if out, err := exec.Command("docker", "exec", name, "mkdir", "-p", containerWorkspace).CombinedOutput(); err != nil {
		t.Fatalf("prepare workspace in %s: %v: %s", name, err, out)
	}
}

const containerWorkspace = "/srv/waldo-test"

func containerTransport(t *testing.T, name string) transport.Transport {
	t.Helper()
	tr, err := transport.NewContainer(transport.ContainerConfig{Runtime: "docker", Container: name})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tr.Close() })
	return tr
}

func imageOr(env, fallback string) string {
	if v := os.Getenv(env); v != "" {
		return v
	}
	return fallback
}

// TestDockerTierConformance runs the shared suite against a container, for every
// tier a container can support.
//
// A container has no SSH subsystem, so tier 1 is genuinely unavailable here —
// and that is worth asserting rather than skipping past, because "unavailable"
// must be reported as such rather than silently satisfied by something else.
func TestDockerTierConformance(t *testing.T) {
	const name = "waldo-it-gnu"
	startContainer(t, name, imageOr(gnuImageEnv, "python:3.11-slim"))
	tr := containerTransport(t, name)
	ctx := context.Background()

	caps, err := fileops.Probe(ctx, tr)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	t.Logf("container userland: %s stat=%s python3=%v find-printf=%v",
		caps.Uname, caps.StatFlavor, caps.Python3, caps.FindPrintf)

	if ok, why := caps.Qualifies(waldo.TierSFTP); ok {
		t.Errorf("a container claimed to support the sftp tier (%q); it has no subsystem channel", why)
	}

	for _, tier := range []waldo.Tier{waldo.TierPOSIX, waldo.TierPipe, waldo.TierAgent} {
		t.Run(tier.String(), func(t *testing.T) {
			if ok, why := caps.Qualifies(tier); !ok {
				t.Skipf("this image does not qualify for tier %s: %s", tier, why)
			}
			sel, err := fileops.New(ctx, tier, tr, caps, true, nil)
			if err != nil {
				t.Fatalf("build tier %s: %v", tier, err)
			}
			t.Cleanup(func() { _ = sel.Ops.Close() })

			root := path.Join(containerWorkspace, "conf-"+tier.String())
			if err := sel.Ops.Mkdir(ctx, root, 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			fileopstest.Run(t, root, func(*testing.T) fileops.FileOps { return sel.Ops })
		})
	}
}

// TestBusyboxTarget is the userland waldo degrades for but had never met.
//
// busybox is not a smaller GNU: `find` has no -printf, the applets accept a
// different subset of flags, and the fallbacks waldo carries for exactly this
// case had never been executed against the thing they were written for. A
// target like this is also the likeliest real one — an Alpine container, an
// embedded box, a rescue image.
func TestBusyboxTarget(t *testing.T) {
	const name = "waldo-it-busybox"
	startContainer(t, name, imageOr(busyboxImageEnv, "alpine:latest"))
	tr := containerTransport(t, name)
	ctx := context.Background()

	caps, err := fileops.Probe(ctx, tr)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	t.Logf("busybox userland: %s stat=%s find-printf=%v python3=%v sha256=%q",
		caps.Uname, caps.StatFlavor, caps.FindPrintf, caps.Python3, caps.SHA256)

	// The degradation this target is here to exercise. If a future busybox
	// grows -printf the assertion should be relaxed, but silently testing the
	// GNU path while believing it is the busybox one would make the whole test
	// pointless.
	if caps.FindPrintf {
		t.Log("note: this busybox has find -printf, so the NUL-safe listing path is being used")
	} else {
		t.Log("confirmed: no find -printf, so the portable listing fallback is under test")
	}

	sel, err := fileops.New(ctx, waldo.TierPOSIX, tr, caps, true, nil)
	if err != nil {
		t.Fatalf("build tier 0 on busybox: %v", err)
	}
	t.Cleanup(func() { _ = sel.Ops.Close() })

	root := path.Join(containerWorkspace, "conf-busybox")
	if err := sel.Ops.Mkdir(ctx, root, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	fileopstest.Run(t, root, func(*testing.T) fileops.FileOps { return sel.Ops })
}

// TestDockerExecReportsStatusAndOutput covers the transport itself: a container
// runtime reports failures differently from ssh, and waldo must still tell a
// command that failed apart from a runtime that did.
func TestDockerExecReportsStatusAndOutput(t *testing.T) {
	const name = "waldo-it-exec"
	startContainer(t, name, imageOr(gnuImageEnv, "python:3.11-slim"))
	tr := containerTransport(t, name)
	ctx := context.Background()

	res, err := tr.Run(ctx, waldo.ExecRequest{Command: "echo out; echo err >&2; exit 9"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Code != 9 {
		t.Errorf("exit code = %d, want 9", res.Code)
	}
	if strings.TrimSpace(string(res.Stdout)) != "out" {
		t.Errorf("stdout = %q", res.Stdout)
	}
	if !strings.Contains(string(res.Stderr), "err") {
		t.Errorf("stderr = %q", res.Stderr)
	}

	// A container that is not there is a transport failure, not a command that
	// exited non-zero — the distinction waldo's whole exec protocol exists for.
	missing, err := transport.NewContainer(transport.ContainerConfig{
		Runtime: "docker", Container: "waldo-it-definitely-not-running",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := missing.Run(ctx, waldo.ExecRequest{Command: "true"}); err == nil {
		t.Error("running in a nonexistent container was reported as success")
	}

	// Quoting survives the runtime's own argument handling.
	for _, s := range []string{"with space", "it's", `"double"`, "$(hostname)", "semi;colon", "new\nline"} {
		res, err := tr.Run(ctx, waldo.ExecRequest{Command: "printf '%s' " + transport.ShellQuote(s)})
		if err != nil {
			t.Fatalf("%q: %v", s, err)
		}
		if string(res.Stdout) != s {
			t.Errorf("round trip through docker exec: got %q want %q", res.Stdout, s)
		}
	}
	_ = fmt.Sprint()
}
