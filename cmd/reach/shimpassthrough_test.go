package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestShimPassthroughLeavesTheSeamArmed is the regression test for a bypass
// that reach caused itself.
//
// Harnesses are routinely installed behind a wrapper script — npm, asdf, pyenv
// and nvm all write one — and those wrappers start `#!/usr/bin/env bash`. With
// reach's shim first on PATH, `env` resolves that `bash` to the shim, so reach
// is asked to run the wrapper. That is not a `-c` invocation, so reach hands it
// to the real shell, which is right; what was wrong was how. reach stripped its
// shim directory from PATH and set REACH_IN_SHELL_SHIM in the environment it
// handed over, and the real shell passed both to the harness it then launched.
// Every command the agent ran afterwards executed on the operator's own machine
// while being reported as remote — the exact failure reach exists to prevent.
//
// The test drives the real binary because the mechanism is exec and argv[0];
// there is nothing to observe in-process.
func TestShimPassthroughLeavesTheSeamArmed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shebang dispatch is a POSIX mechanism")
	}
	dir := t.TempDir()

	reachBin := filepath.Join(dir, "reach")
	build := exec.Command("go", "build", "-o", reachBin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build reach: %v\n%s", err, out)
	}

	// The shim as ensurePathShim installs it: the binary under a shell's name.
	shimDir := filepath.Join(dir, "shim")
	if err := os.MkdirAll(shimDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(reachBin, filepath.Join(shimDir, "bash")); err != nil {
		t.Fatal(err)
	}

	// A harness launcher of the shape every version manager writes.
	wrapper := filepath.Join(dir, "harness")
	script := "#!/usr/bin/env bash\n" +
		"echo \"guard=${REACH_IN_SHELL_SHIM:-unset}\"\n" +
		"echo \"path=$PATH\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(wrapper)
	// No REACH_SESSION: this is the pass-through path, which must behave the
	// same whether or not reach is engaged.
	cmd.Env = append(os.Environ(), "PATH="+shimDir+string(filepath.ListSeparator)+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("run the wrapper through the shim: %v\n%s", err, out)
	}

	got := string(out)
	if !strings.Contains(got, "guard=unset") {
		t.Errorf("reach set %s for everything the wrapper starts, which disables the shim:\n%s",
			shimGuardEnv, got)
	}
	var pathLine string
	for _, line := range strings.Split(got, "\n") {
		if rest, ok := strings.CutPrefix(line, "path="); ok {
			pathLine = rest
		}
	}
	if pathLine == "" {
		t.Fatalf("the wrapper reported no PATH at all:\n%s", got)
	}
	var found bool
	for _, d := range filepath.SplitList(pathLine) {
		if sameDir(d, shimDir) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("reach removed its shim directory from the PATH the wrapper passes on,\n"+
			"so a harness launched from here would run the agent's commands locally.\ngot %q", pathLine)
	}
}

// TestFindRealShellNeverReturnsReach covers the recursion that removing the
// environment guard leaves as the only remaining risk: a shim reached by some
// route other than reach's own shim directory. Exec'ing one would put the
// process straight back where it started with nothing to break the loop.
func TestFindRealShellNeverReturnsReach(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the alias is a symlink on POSIX; Windows copies it into the shim dir, which is skipped by name")
	}
	dir := t.TempDir()
	t.Setenv("REACH_HOME", filepath.Join(dir, "home"))

	// A shim somewhere reach did not put it — a second PATH entry for the same
	// directory, a version manager's bin, someone's own symlink.
	stray := filepath.Join(dir, "stray")
	if err := os.MkdirAll(stray, 0o700); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(self, filepath.Join(stray, "bash")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", stray+string(filepath.ListSeparator)+os.Getenv("PATH"))

	shell, err := findRealShell()
	if err != nil {
		t.Skipf("no real shell on this machine's PATH: %v", err)
	}
	if sameFile(shell, self) {
		t.Fatalf("findRealShell returned this executable (%s); exec of it would not terminate", shell)
	}
}
