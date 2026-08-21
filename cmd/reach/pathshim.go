package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/bojieli/agentreach/internal/session"
)

// bashShimName is the logical name reach answers to when installed on PATH as a
// harness's shell.
//
// Harnesses that resolve their shell by name — Codex does, through execvp —
// are intercepted by placing an executable called `bash` earlier on PATH. This
// requires no fork, no chsh, and no configuration file the harness has to
// support.
//
// It is coarser than a dedicated hook: every `bash -c` the harness runs is
// redirected, including the ones it runs for its own internal purposes. That is
// usually what is wanted — the harness's own file reads go through the same
// path — but it is why a shim invocation with no session bound falls back to a
// local shell rather than failing.
const bashShimName = "bash"

// shimGuardEnv stops a shim from invoking itself.
//
// The shim directory is prepended to PATH for the harness, and that PATH is
// inherited by everything the harness spawns — including reach's own ssh
// subprocess. Without a guard, any `bash` invoked underneath reach would route
// back into reach and recurse until the machine ran out of processes.
const shimGuardEnv = "REACH_IN_SHELL_SHIM"

// shimmedShellNames are the shell names reach installs on PATH and answers to.
// zsh is the default login shell on macOS. Codex resolves the user's login
// shell rather than hard-coding bash there, so omitting this alias lets its
// tool calls bypass reach entirely on a stock macOS install.
var shimmedShellNames = []string{bashShimName, "sh", "zsh"}

// isBashShimInvocation reports whether reach was started as a harness's shell.
func isBashShimInvocation() bool {
	base := programBase(os.Args[0])
	return base == bashShimName || base == "sh" || base == "zsh"
}

// runBashShim implements the `bash -c "<command>"` contract.
//
// Anything that is not a `-c` invocation — an interactive shell, a script file,
// a version query — is handed to the real shell locally. reach redirects the
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
	sess, sessErr := loadSessionQuiet()
	if sessErr != nil {
		// If reach was never engaged for this process, a shell invocation is
		// somebody else's and belongs on the local machine.
		if os.Getenv("REACH_SESSION") == "" {
			return execRealShell(args)
		}
		// But if reach *was* engaged and its session is missing, running the
		// command locally would be the worst possible outcome: the agent
		// believes it is operating on the target and would silently act on the
		// operator's own machine instead. Fail visibly.
		fmt.Fprintf(os.Stderr,
			"reach: session %q is not available, refusing to run this command locally.\n"+
				"       The agent expects it to run on the target. Start the session with:\n"+
				"         reach up <target> --name %s\n"+
				"       Reason: %v\n",
			os.Getenv("REACH_SESSION"), os.Getenv("REACH_SESSION"), sessErr)
		return exitTransportFailure
	}
	return runOnTarget(shimContext(), sessionNameFromEnv(""), mapEmbeddedCwd(sess, command), "")
}

// mapEmbeddedCwd rewrites the `cd <dir> && <command>` prefix some harnesses
// wrap every shell call in (Kimi does this unconditionally) so the directory
// is the target's, not the operator's.
//
// The harness computes <dir> on the local machine — its own working directory
// — so forwarded verbatim the prefix either fails outright (the path does not
// exist on the target) or, worse, lands the command in an unrelated directory
// that happens to exist on both machines. reach maps the harness's workspace,
// passed down as REACH_EXEC_WORKSPACE by the launcher, onto the session's
// target root; a cd anywhere else is left alone, because that is the agent
// thinking in the target's own paths.
//
// Anything that does not match the exact prefix shape is returned untouched:
// rewriting arbitrary commands is worse than the leak it would prevent.
func mapEmbeddedCwd(sess *session.Session, command string) string {
	workspace := os.Getenv("REACH_EXEC_WORKSPACE")
	if workspace == "" || sess == nil || sess.Target == nil || sess.Target.Workspace == "" {
		return command
	}
	dir, rest, ok := splitCdPrefix(command)
	if !ok {
		return command
	}
	// macOS canonicalises /tmp to /private/tmp and the harness may report
	// either spelling, so compare both forms of the workspace.
	//
	// Everything below is compared with forward slashes. The workspace is a
	// path on the operator's own filesystem, so on Windows filepath.Clean
	// spells it with backslashes, while the directory the harness embedded in
	// the command is spelled however the harness spells it. Comparing the two
	// raw made a Windows operator's `cd /etc && …` come out as
	// `cd /srv/app/../../../etc`, because filepath.Rel returned `..\..\..\etc`
	// and the guard below only recognised `../`.
	candidates := []string{filepath.ToSlash(filepath.Clean(workspace))}
	if resolved, err := filepath.EvalSymlinks(workspace); err == nil {
		candidates = append(candidates, filepath.ToSlash(filepath.Clean(resolved)))
	}
	from := filepath.ToSlash(filepath.Clean(dir))
	for _, ws := range candidates {
		if from == ws {
			return "cd " + shellQuote(sess.Target.Workspace) + " && " + rest
		}
		// A plain prefix test rather than filepath.Rel: the only thing worth
		// answering is whether the directory is *inside* the workspace, and a
		// relative path that has to be inspected for `..` afterwards is a way
		// to get that wrong on one platform and not the other.
		if ws != "" && strings.HasPrefix(from, ws+"/") {
			mapped := sess.Target.Workspace + "/" + from[len(ws)+1:]
			return "cd " + shellQuote(mapped) + " && " + rest
		}
	}
	return command
}

// splitCdPrefix parses a leading `cd <dir> && ` wrapper, returning the
// directory and the remaining command. The directory may be single-quoted,
// double-quoted or bare; anything more exotic is not a prefix reach
// recognises.
func splitCdPrefix(command string) (dir, rest string, ok bool) {
	if !strings.HasPrefix(command, "cd ") {
		return "", "", false
	}
	body := strings.TrimLeft(command[len("cd "):], " \t")
	switch {
	case strings.HasPrefix(body, "'"):
		end := strings.Index(body[1:], "'")
		if end < 0 {
			return "", "", false
		}
		dir, rest = body[1:1+end], body[1+end+1:]
	case strings.HasPrefix(body, `"`):
		end := strings.Index(body[1:], `"`)
		if end < 0 {
			return "", "", false
		}
		dir, rest = body[1:1+end], body[1+end+1:]
	default:
		end := strings.IndexAny(body, " \t")
		if end < 0 {
			return "", "", false
		}
		dir, rest = body[:end], body[end:]
	}
	rest = strings.TrimLeft(rest, " \t")
	if !strings.HasPrefix(rest, "&& ") {
		return "", "", false
	}
	rest = strings.TrimSpace(rest[len("&& "):])
	if dir == "" || rest == "" {
		return "", "", false
	}
	return dir, rest, true
}

// execRealShell replaces this process with the genuine shell, with reach's shim
// directory removed from PATH so the real binary is found.
func execRealShell(args []string) int {
	shell, err := findRealShell()
	if err != nil {
		fmt.Fprintln(os.Stderr, "reach: cannot locate a real shell:", err)
		return 127
	}
	env := append(sanitisedEnv(), shimGuardEnv+"=1")
	argv := append([]string{shell}, args...)
	return replaceProcess(context.Background(), shell, argv, env)
}

// findRealShell locates a shell that is not one of reach's shims.
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
// byte comparison would fail to recognise reach's own shim directory and send
// the shim straight back into itself.
func sameDir(a, b string) bool {
	if b == "" {
		return false
	}
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

// sanitisedEnv returns the environment with reach's shim directory removed
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
// exposes only the shell, never reach itself.
func shimBinDir() (string, error) {
	return reachSubdir("shim")
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
