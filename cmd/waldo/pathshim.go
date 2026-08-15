package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// bashShimName is the basename waldo answers to when installed on PATH as a
// harness's shell.
//
// Harnesses that resolve their shell with execvp — Codex does — can be
// intercepted by placing an executable named `bash` earlier on PATH. This
// requires no fork, no chsh, and no configuration file the harness has to
// support.
const bashShimName = "bash"

// shimGuardEnv stops a shim from invoking itself.
//
// The shim directory is prepended to PATH for the harness, and that PATH is
// inherited by everything the harness spawns — including waldo's own ssh
// subprocess. Without a guard, any `bash` invoked underneath waldo would route
// back into waldo and recurse until the machine ran out of processes.
const shimGuardEnv = "WALDO_IN_SHELL_SHIM"

// isBashShimInvocation reports whether waldo was started as a harness's shell.
func isBashShimInvocation() bool {
	base := filepath.Base(os.Args[0])
	return base == bashShimName || base == "sh" || base == "zsh"
}

// runBashShim implements the `bash -c "<command>"` contract.
//
// Anything that is not a `-c` invocation — an interactive shell, a script file,
// a version query — is handed to the real shell locally. waldo redirects the
// harness's commands, not every incidental use of a shell by unrelated tooling.
func runBashShim(args []string) int {
	if os.Getenv(shimGuardEnv) != "" {
		return execRealShell(args)
	}
	command := ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		// Accept -c, -lc, -ic and similar clusters, which harnesses use
		// interchangeably.
		if strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.Contains(a, "c") {
			if i+1 < len(args) {
				command = args[i+1]
			}
			break
		}
	}
	if command == "" {
		return execRealShell(args)
	}
	if _, err := loadSessionQuiet(); err != nil {
		// If waldo was never engaged for this process, a shell invocation is
		// somebody else's and belongs on the local machine.
		if os.Getenv("WALDO_SESSION") == "" {
			return execRealShell(args)
		}
		// But if waldo *was* engaged and its session is missing, running the
		// command locally would be the worst possible outcome: the agent
		// believes it is operating on the target and would silently act on the
		// operator's own machine instead. Fail visibly.
		fmt.Fprintf(os.Stderr,
			"waldo: session %q is not available, refusing to run this command locally.\n"+
				"       The agent expects it to run on the target. Start the session with:\n"+
				"         waldo up <target> --name %s\n"+
				"       Reason: %v\n",
			os.Getenv("WALDO_SESSION"), os.Getenv("WALDO_SESSION"), err)
		return exitTransportFailure
	}
	return runOnTarget(shimContext(), sessionNameFromEnv(""), command, "")
}

// execRealShell replaces this process with the genuine shell, with waldo's shim
// directory removed from PATH so the real binary is found.
func execRealShell(args []string) int {
	real, err := findRealShell()
	if err != nil {
		fmt.Fprintln(os.Stderr, "waldo: cannot locate a real shell:", err)
		return 127
	}
	env := append(sanitisedEnv(), shimGuardEnv+"=1")
	argv := append([]string{real}, args...)
	if err := syscall.Exec(real, argv, env); err != nil {
		fmt.Fprintln(os.Stderr, "waldo:", err)
		return 127
	}
	return 0
}

// findRealShell locates a shell that is not one of waldo's shims.
func findRealShell() (string, error) {
	shimDir, _ := binDir()
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == shimDir || filepath.Base(dir) == "shim" {
			continue
		}
		p := filepath.Join(dir, "bash")
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	for _, p := range []string{"/bin/bash", "/usr/bin/bash", "/bin/sh"} {
		if fi, err := os.Stat(p); err == nil && fi.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("no shell found on PATH")
}

// sanitisedEnv returns the environment with waldo's shim directory removed
// from PATH, so subprocesses find real binaries.
func sanitisedEnv() []string {
	shimDir, err := shimBinDir()
	if err != nil {
		return os.Environ()
	}
	out := make([]string, 0, len(os.Environ()))
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok && k == "PATH" {
			var kept []string
			for _, d := range filepath.SplitList(v) {
				if d != shimDir {
					kept = append(kept, d)
				}
			}
			out = append(out, "PATH="+strings.Join(kept, string(filepath.ListSeparator)))
			continue
		}
		out = append(out, kv)
	}
	return out
}

// shimBinDir is the directory holding the PATH-based shell shims. It is kept
// separate from the general bin directory so that prepending it to PATH
// exposes only the shell, never waldo itself.
func shimBinDir() (string, error) {
	base := os.Getenv("WALDO_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".waldo")
	}
	dir := filepath.Join(base, "shim")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// ensurePathShim installs shell shims and returns the directory to prepend to
// PATH.
func ensurePathShim() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	if self, err = filepath.EvalSymlinks(self); err != nil {
		return "", err
	}
	dir, err := shimBinDir()
	if err != nil {
		return "", err
	}
	for _, name := range []string{"bash", "sh"} {
		link := filepath.Join(dir, name)
		if cur, err := os.Readlink(link); err == nil && cur == self {
			continue
		}
		_ = os.Remove(link)
		if err := os.Symlink(self, link); err != nil {
			return "", fmt.Errorf("install %s shim: %w", name, err)
		}
	}
	return dir, nil
}
