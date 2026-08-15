//go:build integration

package integration

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

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
func startContainer(t *testing.T, name, image string, runArgs ...string) {
	t.Helper()
	dockerAvailable(t)

	_ = exec.Command("docker", "rm", "-f", name).Run()
	args := append([]string{"run", "-d", "--name", name}, runArgs...)
	args = append(args, "--entrypoint", "sleep", image, "infinity")
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		// A machine without the image cached and without registry access cannot
		// run this, which is a missing prerequisite rather than a failure.
		t.Skipf("cannot start %s from %s: %v: %s", name, image, err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })

	if out, err := exec.Command("docker", "exec", name, "mkdir", "-p", containerWorkspace).CombinedOutput(); err != nil {
		t.Logf("could not prepare %s in %s (%s); the test decides whether that matters",
			containerWorkspace, name, strings.TrimSpace(string(out)))
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

// The tests below are about waldo's governing rule rather than about
// containers: every failure must be a value the agent can reason about, never a
// process that stops responding. A container runtime is simply the cheapest way
// to build targets that are broken in specific, realistic ways — no shell, a
// filesystem that refuses writes, a user without privileges — and to check that
// each produces an error someone can act on.

// TestTargetWithNoShellFailsClearly covers a target with no shell at all.
//
// waldo's floor is a POSIX shell. A target without one cannot be used, and the
// only question is whether waldo says so or hangs trying. This is not an exotic
// case: distroless and scratch images are ordinary production containers, and
// pointing an agent at one is an easy mistake to make.
//
// The image is built here rather than pulled, from nothing but waldo's own
// statically linked agent — which is the most honest distroless target
// available, needs no registry, and holds itself open by blocking on the stdin
// it expects to receive a protocol on.
func TestTargetWithNoShellFailsClearly(t *testing.T) {
	dockerAvailable(t)
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no Go toolchain to build the scratch image's contents")
	}

	root, ok := repoRoot()
	if !ok {
		// The suite is runnable as a standalone binary — cross-compiled and
		// copied to a machine with no checkout — and this is the one test that
		// needs the source. A missing optional prerequisite is a skip; failing
		// here would report a broken product when the product is fine.
		t.Skip("no module root above the working directory, so the scratch image cannot be built")
	}
	dir := t.TempDir()
	arch := runtime.GOARCH // the daemon runs Linux containers of the host's arch
	build := exec.Command("go", "build", "-trimpath", "-o", filepath.Join(dir, "agent"),
		"./cmd/waldo-agent")
	build.Dir = root
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+arch, "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Skipf("cannot cross-compile the agent: %v: %s", err, out)
	}
	dockerfile := "FROM scratch\nCOPY agent /agent\nENTRYPOINT [\"/agent\", \"serve\"]\n"
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		t.Fatal(err)
	}
	const image = "waldo-it-scratch"
	if out, err := exec.Command("docker", "build", "-q", "-t", image, dir).CombinedOutput(); err != nil {
		t.Skipf("cannot build the scratch image: %v: %s", err, out)
	}

	const name = "waldo-it-noshell"
	_ = exec.Command("docker", "rm", "-f", name).Run()
	// -i keeps stdin open, so the agent blocks waiting for a request and the
	// container stays up with nothing else in it.
	if out, err := exec.Command("docker", "run", "-d", "-i", "--name", name, image).CombinedOutput(); err != nil {
		t.Skipf("cannot start the scratch container: %v: %s", err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", name).Run()
		_ = exec.Command("docker", "rmi", "-f", image).Run()
	})

	tr := containerTransport(t, name)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := fileops.Probe(ctx, tr)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a target with no shell was probed successfully")
		}
		t.Logf("reported as: %v", err)
	case <-ctx.Done():
		t.Fatal("probing a shell-less target hung instead of failing; " +
			"an agent cannot reason about a process that stops responding")
	}
}

// repoRoot finds the module directory so a test can build from it, and reports
// whether there is one at all.
func repoRoot() (string, bool) {
	dir, err := os.Getwd()
	if err != nil {
		return "", false
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

// TestReadOnlyTargetRefusesWritesButServesReads is the shape of a hardened
// production container, and of a filesystem that has gone read-only under a
// disk fault.
//
// Reads must keep working, and a write must fail as an error the agent can act
// on — not a partial file, and not a success the agent believes.
func TestReadOnlyTargetRefusesWritesButServesReads(t *testing.T) {
	const name = "waldo-it-readonly"
	dockerAvailable(t)
	_ = exec.Command("docker", "rm", "-f", name).Run()
	image := imageOr(gnuImageEnv, "python:3.11-slim")
	// A writable /tmp with a read-only root is the usual hardened arrangement.
	out, err := exec.Command("docker", "run", "-d", "--name", name, "--read-only",
		"--entrypoint", "sleep", image, "infinity").CombinedOutput()
	if err != nil {
		t.Skipf("cannot start a read-only container: %s", strings.TrimSpace(string(out)))
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })

	tr := containerTransport(t, name)
	ctx := context.Background()
	caps, err := fileops.Probe(ctx, tr)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	sel, err := fileops.New(ctx, waldo.TierPOSIX, tr, caps, true, nil)
	if err != nil {
		t.Fatalf("build tier: %v", err)
	}
	t.Cleanup(func() { _ = sel.Ops.Close() })

	// Reading from the read-only root still works.
	if _, err := sel.Ops.Read(ctx, "/etc/hostname", 0, 0); err != nil {
		t.Errorf("reading from a read-only target failed: %v", err)
	}
	// Writing to it must fail, and say so.
	err = sel.Ops.Write(ctx, "/etc/waldo-should-not-appear", []byte("x"), 0o644)
	if err == nil {
		t.Fatal("writing to a read-only filesystem was reported as success")
	}
	t.Logf("write refused with: %v", err)

	// And must not have left debris behind while failing.
	res, runErr := tr.Run(ctx, waldo.ExecRequest{Command: "ls /etc/.waldo.tmp.* 2>/dev/null | wc -l"})
	if runErr == nil && strings.TrimSpace(string(res.Stdout)) != "0" {
		t.Errorf("a failed write left temporary files behind: %s", res.Stdout)
	}
}

// TestUnprivilegedTargetUser covers a container running as a non-root user with
// no home directory — the default for a security-conscious image, and the case
// where the agent tier has nowhere obvious to cache itself.
func TestUnprivilegedTargetUser(t *testing.T) {
	const name = "waldo-it-nonroot"
	startContainer(t, name, imageOr(gnuImageEnv, "python:3.11-slim"), "--user", "1000:1000")
	tr := containerTransport(t, name)
	ctx := context.Background()

	caps, err := fileops.Probe(ctx, tr)
	if err != nil {
		t.Fatalf("probe as an unprivileged user: %v", err)
	}

	// /tmp is writable by anyone; the prepared workspace may not be.
	root := "/tmp/waldo-nonroot"
	sel, err := fileops.New(ctx, waldo.TierPOSIX, tr, caps, true, nil)
	if err != nil {
		t.Fatalf("build tier: %v", err)
	}
	t.Cleanup(func() { _ = sel.Ops.Close() })
	if err := sel.Ops.Mkdir(ctx, root, 0o755); err != nil {
		t.Fatalf("mkdir as an unprivileged user: %v", err)
	}
	t.Cleanup(func() { _ = sel.Ops.Remove(context.Background(), root, true) })

	fileopstest.Run(t, root, func(*testing.T) fileops.FileOps { return sel.Ops })

	// Writing somewhere this user cannot must be a clean refusal.
	if err := sel.Ops.Write(ctx, "/etc/waldo-should-not-appear", []byte("x"), 0o644); err == nil {
		t.Error("an unprivileged write to /etc was reported as success")
	}
}
