// Command reach runs a coding agent's tools against a remote target while the
// agent — and the credentials it holds — stay on the local machine.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

const usage = `reach — teleoperation for coding agents

Point your agent at any box you can SSH into. The box never gets your agent.

USAGE
  reach <target> <command> [arguments]   bind a session to the target and run
  reach <command> [arguments]            act on a session that already exists

SESSIONS
  up <target>       bind a session to a target and probe it
  down [name]       end a session and close its connection
  status [name]     show sessions
  doctor [name]     diagnose a target: what works, what degrades, and why
  log [name]        what reach has run and changed on the target
  helper <op>       inspect or remove the optional helper binary

RUNNING
  exec [cmd...]     run a command on the target
  fs <op> ...       read, write, list, search files on the target
  shell-prefix      internal: entrypoint for CLAUDE_CODE_SHELL_PREFIX
  hook              internal: harness hook entrypoint (mirror mode)
  exec-server       internal: codex remote-environment endpoint (JSON-RPC over stdio)

HARNESSES
  claude [args...]  launch Claude Code wired to the session
  codex [args...]   launch Codex wired to the session
  kimi [args...]    launch Kimi Code wired to the session
  goose [args...]   launch Goose wired to the session
  gemini [args...]  launch Gemini CLI wired to the session
  crush [args...]   launch Crush wired to the session (server mode)
  opencode install  install tools that shadow opencode's built-ins
  env               print the environment a harness needs
  harness verify codex|kimi|goose|gemini   probe whether this harness's shell routes through reach

TARGETS
  build-box                            an ssh_config alias; work where a login lands
  ssh://[user@]host[:port]/abs/path    a remote host over SSH
  docker://container/abs/path          a container
  local:///abs/path                    this machine (for testing)
  user@host:/abs/path                  scp-style shorthand

  Session flags may follow the target, before the command:
  --name --mode --untrusted --fileops --timeout --fresh

EXAMPLES
  reach build-box claude                        Claude Code, working on build-box
  reach ssh://build-box/srv/app claude          ... in a directory you name
  reach client-box:/srv/app --untrusted codex   somebody else's box
  reach build-box exec -- go test ./...
  reach status                                  every session, and where each points

Each target gets its own session, so several boxes can be open at once, one
per terminal. Run 'reach <command> --help' for details.
`

func main() {
	// The platform check comes before everything, including the shim dispatch.
	// A shim that half-works on an unsupported platform is the worst outcome
	// this program has: the harness cannot tell its shell was not redirected, so
	// the model's commands run on the operator's own machine while the agent
	// believes they are running on the target.
	if err := platformCheck(); err != nil {
		fmt.Fprintln(os.Stderr, "reach:", err)
		os.Exit(2)
	}

	// The shim path is latency-sensitive and runs once per tool call, so it is
	// dispatched before anything else and does no extra work. Harnesses invoke
	// it through a symlink, which arrives as argv[0].
	if isShimInvocation() {
		os.Exit(runShellPrefix(os.Args[1:]))
	}
	if isBashShimInvocation() {
		os.Exit(runBashShim(os.Args[1:]))
	}
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	// The internal entrypoints are answered before the signal context is set
	// up, for the same reason as the shim: they are on the per-tool-call path.
	switch os.Args[1] {
	case "shell-prefix":
		os.Exit(runShellPrefix(os.Args[2:]))
	case "hook":
		os.Exit(runHook(os.Args[2:]))
	case "exec-server":
		os.Exit(cmdExecServer(context.Background(), os.Args[2:]))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := dispatch(ctx, os.Args[1:])
	stop()
	os.Exit(code)
}

// knownCommands is every word reach answers as a command.
//
// dispatch consults it before anything else, because a first argument that is
// not in here is read as a *target*: `reach build-box claude`. A command
// missing from this map would be mistaken for a hostname, and one listed here
// that dispatch does not handle would exit zero having done nothing, so
// TestDispatchAndKnownCommandsAgree reads both out of this file and fails if
// they ever drift apart.
var knownCommands = map[string]bool{
	"up": true, "down": true, "status": true, "doctor": true, "log": true,
	"exec": true, "fs": true, "env": true, "helper": true, "harness": true,
	"claude": true, "codex": true, "kimi": true, "goose": true,
	"gemini": true, "crush": true, "opencode": true,
	"shell-prefix": true, "hook": true, "exec-server": true,
	"agent":   true,
	"version": true, "--version": true, "-v": true,
	"help": true, "--help": true, "-h": true,
}

// dispatch runs one reach command line and returns the process exit code.
//
// A first argument that is not a command is not a mistake to report: it is how
// the one-shot form starts. `reach build-box claude` binds a session to
// build-box and runs Claude Code against it, which is the shape most sessions
// actually want. `up` and `down` remain for the sessions that outlive a single
// agent, and for the ones several commands share.
func dispatch(ctx context.Context, args []string) int {
	if !knownCommands[args[0]] {
		return runTargetFirst(ctx, args)
	}

	var err error
	switch args[0] {
	case "up":
		err = cmdUp(ctx, args[1:])
	case "down":
		err = cmdDown(ctx, args[1:])
	case "status":
		err = cmdStatus(ctx, args[1:])
	case "doctor":
		err = cmdDoctor(ctx, args[1:])
	case "exec":
		return cmdExec(ctx, args[1:])
	case "fs":
		err = cmdFS(ctx, args[1:])
	case "env":
		err = cmdEnv(ctx, args[1:])
	case "claude":
		return cmdClaude(ctx, args[1:])
	case "codex":
		return cmdCodex(ctx, args[1:])
	case "kimi":
		return cmdKimi(ctx, args[1:])
	case "goose":
		return cmdGoose(ctx, args[1:])
	case "gemini":
		return cmdGemini(ctx, args[1:])
	case "crush":
		return cmdCrush(ctx, args[1:])
	case "opencode":
		err = cmdOpencode(ctx, args[1:])
	case "harness":
		err = cmdHarness(ctx, args[1:])
	case "helper":
		err = cmdHelper(ctx, args[1:])
	case "shell-prefix":
		return runShellPrefix(args[1:])
	case "hook":
		return runHook(args[1:])
	case "exec-server":
		return cmdExecServer(ctx, args[1:])
	case "agent":
		// Renamed. Anyone with this in a script or in muscle memory deserves
		// the new name, not "unknown command".
		err = fmt.Errorf("`reach agent` is now `reach helper`.\n" +
			"It was renamed because \"agent\" already means the coding agent this tool " +
			"drives, which made `reach agent uninstall` read like it removed Claude Code")
	case "log":
		err = cmdLog(ctx, args[1:])
	case "version", "--version", "-v":
		fmt.Println(versionLine())
	case "help", "--help", "-h":
		fmt.Print(usage)
	default:
		// Only reachable if knownCommands lists something this switch does not
		// handle, which the drift test exists to prevent.
		fmt.Fprintf(os.Stderr, "reach: %q is listed as a command but not handled\n", args[0])
		return 2
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "reach:", err)
		return 1
	}
	return 0
}
