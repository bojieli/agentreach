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

// shimGuardEnv makes the shim pass everything through to the local shell.
//
// reach used to set this itself whenever it handed an invocation to the real
// shell, which disarmed the seam for every process started after that one —
// see execRealShell for what that cost. Recursion is now prevented where it
// can actually happen, in findRealShell, and nothing in reach sets this.
//
// It remains as an operator escape hatch: exporting it makes a shim on PATH
// behave as an ordinary shell, which is the quickest way to take reach out of
// the picture while diagnosing something without unpicking PATH.
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
	// Grok Build snapshots the login environment with `$SHELL -lc` before
	// any tool call. Those scripts read the operator's own bashrc and must
	// stay local. Its actual tool commands are wrapped in an envelope that
	// names __grok_user_cmd; the payload after `--` is what should run on
	// the target.
	if isGrokLocalSnapshot(args) {
		return execRealShell(args)
	}
	command, ok := unwrapGrokEnvelope(args)
	if !ok {
		command = shellCommandArg(args)
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

// unwrapGrokEnvelope extracts the user command from Grok Build's shell
// invocation.
//
// Observed on grok 1.0.5: the run_terminal_command tool spawns
//
//	$SHELL -O extglob -c '<envelope using __grok_user_cmd="$1">' -- <command>
//
// The envelope reads a snapshot from fd 3, evals it, then evals $1. Forwarding
// the envelope to the target fails (no fd 3, no $1). The payload after `--` is
// the command the model asked to run.
func unwrapGrokEnvelope(args []string) (string, bool) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" || a == "-" || !strings.HasPrefix(a, "-") {
			return "", false
		}
		if strings.HasPrefix(a, "--") {
			continue
		}
		if !strings.Contains(a, "c") {
			// -O extglob sits in front of -c. Skip the option's argument.
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
			}
			continue
		}
		tail := args[i+1:]
		if len(tail) == 0 {
			return "", false
		}
		if !strings.Contains(tail[0], "__grok_user_cmd") {
			return "", false
		}
		rest := tail[1:]
		if len(rest) > 0 && rest[0] == "--" {
			rest = rest[1:]
		}
		if len(rest) == 0 {
			return "", false
		}
		return strings.Join(rest, " "), true
	}
	return "", false
}

// isGrokLocalSnapshot reports Grok Build's pre-command environment snapshot
// (`$SHELL -lc 'source "$HOME/.bashrc"; … alias … env'`). That must run on
// the operator's machine; sending it to the target would source the wrong
// home directory.
func isGrokLocalSnapshot(args []string) bool {
	cmd := shellCommandArg(args)
	if cmd == "" {
		return false
	}
	if strings.Contains(cmd, "__grok_user_cmd") {
		return false
	}
	return strings.Contains(cmd, `source "$HOME/.bashrc"`) ||
		strings.Contains(cmd, "builtin alias -p") ||
		strings.Contains(cmd, "command env -0")
}

// shellCommandArg returns the command string of a `-c` invocation, or "" when
// this is not one.
//
// Options stop at the first argument that is not one, exactly as a shell's own
// parsing does: in `bash [options] script args...` everything from the script
// path onwards belongs to the script, not to bash. Scanning the whole argv for
// anything containing a "c" ignored that, and the result was not a missed
// interception but a wrong one. A harness installed behind a wrapper is started
// as `bash /path/to/wrapper <the harness's own flags>` — and codex's flags
// begin `-c model_providers.reach.name="reach"`. reach took that for a command
// and ran the config override on the target, where it arrived as
// `bash: line 1: model_providers.reach.name=reach: command not found`, while
// the wrapper it was actually asked to run never started at all.
func shellCommandArg(args []string) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" || a == "-" || !strings.HasPrefix(a, "-") {
			// End of options: a script path, or an explicit terminator.
			return ""
		}
		// Accept -c, -lc, -ic and similar clusters, which harnesses use
		// interchangeably. Long options are not shorthand clusters.
		if !strings.HasPrefix(a, "--") && strings.Contains(a, "c") {
			if i+1 < len(args) {
				return args[i+1]
			}
			return ""
		}
	}
	return ""
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

// execRealShell replaces this process with the genuine shell, passing the
// environment through untouched.
//
// Untouched is the whole point, and it was not always so. reach used to strip
// its shim directory from PATH here and set shimGuardEnv, and the real shell
// then handed both to everything it started. That is harmless for the case it
// was written for — unrelated tooling running `bash somescript.sh` — and
// catastrophic for the case that is indistinguishable from here: a harness
// launched through a `#!/usr/bin/env bash` wrapper, which is how npm, asdf,
// pyenv and nvm all install their binaries. `env` resolves `bash` on PATH,
// finds this shim, and reach hands the wrapper to the real shell with the seam
// switched off — so the harness underneath ran every one of the agent's
// commands on the operator's own machine while reporting them as remote. That
// is the exact failure reach exists to prevent, arrived at by reach's own
// doing.
//
// Recursion is prevented by construction instead: findRealShell returns an
// absolute path that is not this executable, and exec of an absolute path
// cannot come back here.
func execRealShell(args []string) int {
	shell, err := findRealShell()
	if err != nil {
		fmt.Fprintln(os.Stderr, "reach: cannot locate a real shell:", err)
		return 127
	}
	argv := append([]string{shell}, args...)
	return replaceProcess(context.Background(), shell, argv, os.Environ())
}

// findRealShell locates a shell that is not one of reach's shims.
//
// Two tests, not one. The shim directory is skipped by name, which covers
// reach's own installation; every candidate is then checked for being this
// executable, which covers a shim reached by another route — a second PATH
// entry pointing at the same directory, a symlink someone else made, a copy in
// a version manager's bin. Exec'ing one of those would put this process
// straight back where it started, and with nothing in the environment to break
// the loop it would not stop.
func findRealShell() (string, error) {
	shimDir, _ := shimBinDir()
	self, selfErr := selfPath()
	isSelf := func(p string) bool {
		if selfErr != nil {
			return false
		}
		return sameFile(p, self)
	}
	for _, dir := range filepath.SplitList(pathEnvValue()) {
		if dir == "" || sameDir(dir, shimDir) {
			continue
		}
		for _, name := range shellCandidateNames() {
			p := filepath.Join(dir, name)
			if isExecutableFile(p) && !isSelf(p) {
				return p, nil
			}
		}
	}
	for _, p := range fallbackShellPaths() {
		if isExecutableFile(p) && !isSelf(p) {
			return p, nil
		}
	}
	return "", fmt.Errorf("no shell found on PATH")
}

// sameFile reports whether two paths are the same file on disk, following
// symlinks. A byte comparison would miss reach's shim, which is a symlink to
// the binary rather than a copy of it on every platform that has them.
func sameFile(a, b string) bool {
	ra, err := filepath.EvalSymlinks(a)
	if err != nil {
		ra = a
	}
	rb, err := filepath.EvalSymlinks(b)
	if err != nil {
		rb = b
	}
	fa, err := os.Stat(ra)
	if err != nil {
		return false
	}
	fb, err := os.Stat(rb)
	if err != nil {
		return false
	}
	return os.SameFile(fa, fb)
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
