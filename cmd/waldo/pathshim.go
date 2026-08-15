package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// bashShimName is the logical name waldo answers to when installed on PATH as a
// harness's shell.
//
// Harnesses that resolve their shell by name — Codex does, through execvp —
// are intercepted by placing an executable called `bash` earlier on PATH. This
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

// shimmedShellNames are the shell names waldo installs on PATH and answers to.
var shimmedShellNames = []string{bashShimName, "sh"}

// isBashShimInvocation reports whether waldo was started as a harness's shell.
func isBashShimInvocation() bool {
	base := programBase(os.Args[0])
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
	shell, err := findRealShell()
	if err != nil {
		fmt.Fprintln(os.Stderr, "waldo: cannot locate a real shell:", err)
		return 127
	}
	env := append(sanitisedEnv(), shimGuardEnv+"=1")
	argv := append([]string{shell}, args...)
	return replaceProcess(context.Background(), shell, argv, env)
}

// findRealShell locates a shell that is not one of waldo's shims.
func findRealShell() (string, error) {
	shimDir, _ := shimBinDir()
	for _, dir := range filepath.SplitList(pathEnvValue()) {
		if dir == "" || sameDir(dir, shimDir) {
			continue
		}
		for _, name := range shellCandidateNames() {
			p := filepath.Join(dir, name)
			if isExecutableFile(p) {
				return p, nil
			}
		}
	}
	for _, p := range fallbackShellPaths() {
		if isExecutableFile(p) {
			return p, nil
		}
	}
	return "", fmt.Errorf("no shell found on PATH")
}

// sameDir compares two directory paths for identity.
//
// Windows paths differ in case and separator without differing in meaning, so a
// byte comparison would fail to recognise waldo's own shim directory and send
// the shim straight back into itself.
func sameDir(a, b string) bool {
	if b == "" {
		return false
	}
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

// sanitisedEnv returns the environment with waldo's shim directory removed
// from PATH, so subprocesses find real binaries.
func sanitisedEnv() []string {
	shimDir, err := shimBinDir()
	if err != nil {
		return os.Environ()
	}
	var kept []string
	for _, d := range filepath.SplitList(pathEnvValue()) {
		if !sameDir(d, shimDir) {
			kept = append(kept, d)
		}
	}
	return setPathEnv(os.Environ(), strings.Join(kept, string(filepath.ListSeparator)))
}

// shimBinDir is the directory holding the PATH-based shell shims. It is kept
// separate from the general bin directory so that prepending it to PATH
// exposes only the shell, never waldo itself.
func shimBinDir() (string, error) {
	return waldoSubdir("shim")
}

// ensurePathShim installs shell shims and returns the directory to prepend to
// PATH.
func ensurePathShim() (string, error) {
	self, err := selfPath()
	if err != nil {
		return "", err
	}
	dir, err := shimBinDir()
	if err != nil {
		return "", err
	}
	for _, name := range shimmedShellNames {
		alias := filepath.Join(dir, programName(name))
		if programAliasIsCurrent(alias, self) {
			continue
		}
		if err := installProgramAlias(self, alias); err != nil {
			return "", fmt.Errorf("install the %s shim at %s: %w", name, alias, err)
		}
	}
	return dir, nil
}
