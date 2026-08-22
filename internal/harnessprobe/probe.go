package harnessprobe

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/bojieli/agentreach/internal/execserver"
	"github.com/bojieli/agentreach/internal/reach"
	"github.com/bojieli/agentreach/internal/session"
)

// DefaultTimeout bounds one probe run. A healthy turn against the mock takes
// seconds; the bound exists for the harness that starts and then hangs, which
// must not hang `reach <harness>` with it.
const DefaultTimeout = 120 * time.Second

// Harnesses the probe knows how to drive.
const (
	HarnessClaudeCode = "claude"
	HarnessCodex      = "codex"
	HarnessKimi       = "kimi"
	HarnessGoose      = "goose"
	HarnessGemini     = "gemini"
)

// Options configures one Verify run.
type Options struct {
	// Harness selects which agent binary to probe: HarnessCodex or
	// HarnessKimi. It selects the mock's wire dialect, the spawn argv, the
	// process environment and the version command.
	Harness string
	// SessionName is the reach session whose target is the ground truth for
	// where the scripted command should run.
	SessionName string
	// EnsureShim installs the PATH shell shims and returns the directory to
	// prepend to PATH. It is injected because shim installation lives in the
	// main package beside the shim's own dispatch.
	EnsureShim func() (string, error)
	// Timeout bounds the whole probe. Zero means DefaultTimeout.
	Timeout time.Duration
	// BinaryPath is an absolute path to the harness binary to probe. When
	// empty Verify falls back to exec.LookPath(Harness). Use this when the
	// harness binary that will actually be launched (e.g. a patched npm
	// bundle under ~/.reach/) is not the one that appears first on PATH.
	BinaryPath string
	// EnsureShellPrefix creates the reach-shell-prefix alias and returns its
	// absolute path. Required for HarnessClaudeCode; ignored by all other
	// harnesses. Injected from the main package where alias installation lives.
	EnsureShellPrefix func() (string, error)
	// CommandPrefix, when set, is prepended to the default probe command
	// ("echo <marker>; hostname") with " && " as separator. Use it to verify
	// that a specific type of operation — file read, file write, or program
	// execution — also routes through the seam to the target. The marker and
	// hostname are always appended so the verdict logic is unchanged.
	// Example: "cat /srv/app/README.md" tests that file reads on the target work.
	CommandPrefix string
}

// harnessSpec is the per-harness slice of the probe: which dialect the mock
// speaks, how the harness is launched against it, and how its environment is
// built. Everything else about the probe — the canary, the hostname
// comparison, the verdicts — is harness-agnostic, because the question it
// answers is always the same: whose machine ran the command.
type harnessSpec struct {
	dialect Dialect
	args    func(baseURL string) []string
	env     func(sessName, shimDir, home, baseURL string) []string
	// prepare runs after the harness's throwaway home directory exists and
	// before the harness is spawned. Nil means the home needs nothing beyond
	// what env arranges.
	prepare func(home, sessName string) error
	// workingDir, if set, overrides the probe process's working directory.
	// Use this when the harness embeds a cd to its own cwd in every Bash
	// call (kimi does this) and the local cwd may not exist on the target.
	// A well-known path like /tmp exists on both sides without mapping.
	workingDir string
}

var harnessSpecs = map[string]harnessSpec{
	HarnessClaudeCode: {dialect: DialectAnthropic, args: claudeCodeArgs, env: claudeCodeEnv, prepare: claudeCodePrepare},
	HarnessCodex:      {dialect: DialectResponses, args: codexArgs, env: codexEnv, prepare: codexPrepare},
	HarnessKimi:       {dialect: DialectChat, args: kimiArgs, env: kimiEnv, workingDir: "/tmp"},
	HarnessGoose:      {dialect: DialectChat, args: gooseArgs, env: gooseEnv},
	HarnessGemini:     {dialect: DialectGemini, args: geminiArgs, env: geminiEnv, prepare: geminiPrepare},
}

// Verify observes where the installed harness actually runs a shell command.
//
// The method is behavioural, not static: rather than parsing the harness's
// version or source for how it resolves a shell, Verify drives one real (but
// offline and token-free) turn against an embedded mock model server, has the
// mock instruct a canary command — `echo <marker>; hostname` — and reads the
// hostname out of the tool output the harness reports back. The hostname is
// compared against the session target's own hostname, obtained through the
// same transport the shell shim uses, which is the ground truth for "ran on
// the target".
//
// The probe inherits reach's own posture rather than weakening it: the same
// session environment the shim needs (REACH_SESSION, shim directory first on
// PATH) and, for codex, an environments.toml routing its remote environment to
// the reach exec-server plus the same sandbox relaxation `reach codex`
// applies. Each harness gets an isolated home directory and a scrubbed
// credential environment so it can only ever talk to the mock on
// 127.0.0.1 — this probe must never reach a real provider.
func Verify(ctx context.Context, opts Options) Result {
	spec, ok := harnessSpecs[opts.Harness]
	if !ok {
		return Result{Verdict: VerdictError, Detail: fmt.Sprintf("unknown harness %q", opts.Harness)}
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	sess, err := session.Load(opts.SessionName)
	if err != nil {
		return Result{Verdict: VerdictError, Detail: "load session: " + err.Error()}
	}
	remoteHost, err := targetHostname(ctx, sess)
	if err != nil {
		return Result{Verdict: VerdictError, Detail: "ask the target its hostname: " + err.Error()}
	}
	localHost, err := os.Hostname()
	if err != nil {
		return Result{Verdict: VerdictError, Detail: "ask the local hostname: " + err.Error()}
	}
	if remoteHost == localHost {
		// The verdict rests on telling the two machines apart by hostname. A
		// session whose target shares this machine's hostname — local:// is
		// the usual case — makes "ran remotely" and "ran locally" identical
		// evidence, and a probe that cannot distinguish must say so rather
		// than guess.
		return Result{Verdict: VerdictError, Detail: fmt.Sprintf(
			"target and local hostnames are both %q; the probe cannot tell them apart", remoteHost)}
	}
	var shimDir string
	if opts.Harness == HarnessClaudeCode {
		if opts.EnsureShellPrefix == nil {
			return Result{Verdict: VerdictError, Detail: "no shell-prefix installer configured"}
		}
		shimDir, err = opts.EnsureShellPrefix()
		if err != nil {
			return Result{Verdict: VerdictError, Detail: "install the shell-prefix alias: " + err.Error()}
		}
	} else {
		if opts.EnsureShim == nil {
			return Result{Verdict: VerdictError, Detail: "no shim installer configured"}
		}
		shimDir, err = opts.EnsureShim()
		if err != nil {
			return Result{Verdict: VerdictError, Detail: "install the PATH shim: " + err.Error()}
		}
	}
	binPath := opts.BinaryPath
	if binPath == "" {
		binPath, err = exec.LookPath(opts.Harness)
		if err != nil {
			return Result{Verdict: VerdictError, Detail: opts.Harness + " is not installed or not in PATH"}
		}
	}

	marker := "REACH_SEAM_" + randomHex(8)
	mock := StartMock(marker, spec.dialect)
	defer mock.Close()
	if opts.CommandPrefix != "" {
		mock.SetCommand(opts.CommandPrefix + " && echo " + marker + "; hostname")
	}

	home, err := os.MkdirTemp("", "reach-"+opts.Harness+"-home-")
	if err != nil {
		return Result{Verdict: VerdictError, Detail: "create a temporary home directory: " + err.Error()}
	}
	defer func() { _ = os.RemoveAll(home) }()

	if spec.prepare != nil {
		if err := spec.prepare(home, sess.Name); err != nil {
			return Result{Verdict: VerdictError, Detail: "prepare the harness home: " + err.Error()}
		}
	}

	cmd := exec.CommandContext(ctx, binPath, spec.args(mock.BaseURL())...)
	cmd.Env = spec.env(sess.Name, shimDir, home, mock.BaseURL())
	if spec.workingDir != "" {
		cmd.Dir = spec.workingDir
	}
	// The launchers set REACH_EXEC_WORKSPACE to the directory they were run
	// in, and the shim needs it to rewrite the `cd '<local-cwd>' && …` prefix
	// Kimi wraps every command in. The probe left it unset, so the probe was
	// measuring a seam nothing ships: kimi's commands reached the target and
	// then failed on `cd /tmp`, and the probe called that inconclusive rather
	// than reporting the interception that had plainly worked.
	cmd.Env = append(cmd.Env, "REACH_EXEC_WORKSPACE="+probeWorkspace(cmd.Dir))
	// Output is captured rather than inherited: a failing probe needs the
	// tail of it as evidence, and a succeeding one has nothing worth the
	// operator's screen.
	var out cappedBuffer
	cmd.Stdout, cmd.Stderr = &out, &out
	runErr := cmd.Run()

	// By the time the harness exits, its final request has been answered — the
	// mock records the tool output before replying — so the result is already
	// settled. The short wait only covers a harness that died mid-request.
	mock.Wait(ctx.Done())
	toolOutput, observed := mock.Result()

	if ctx.Err() == context.DeadlineExceeded {
		// The evidence matters most here and used to be dropped. A probe that
		// hangs says nothing about the seam on its own, and the CI logs for one
		// contained a single line: whether the harness had reached the mock at
		// all, and what it last printed, were the two things needed to tell a
		// slow turn from a harness stuck before it ever made a request.
		reached := "the harness never made the scripted tool call"
		if observed {
			reached = "the tool call was made; the harness did not finish afterwards"
		}
		return Result{Verdict: VerdictError, Detail: fmt.Sprintf(
			"%s did not finish the scripted turn within %s (%s). Output tail: %s",
			opts.Harness, timeout, reached, out.Tail())}
	}
	if runErr != nil && !observed {
		return Result{Verdict: VerdictError, Detail: fmt.Sprintf(
			"%s exited without completing the tool call: %v. Output tail: %s",
			opts.Harness, runErr, out.Tail())}
	}
	if !observed {
		return Result{Verdict: VerdictError, Detail: fmt.Sprintf(
			"%s finished without making the scripted tool call. Output tail: %s", opts.Harness, out.Tail())}
	}

	trimmed := strings.TrimSpace(toolOutput)
	if !echoedMarker(trimmed, marker) {
		// A tool call happened, but this probe's command did not produce
		// output. Either the harness ran something of its own, or it refused
		// to run what it was told: codex declines `rm -f` by policy and reports
		// the refusal with the whole command quoted back, marker included.
		//
		// Neither says anything about the seam, and the difference from a
		// bypass matters more than it looks. A command that never ran did not
		// run locally, and calling it a bypass is not a cautious error — the
		// verdict is cached and `reach codex` refuses to launch any version
		// carrying it, so a harness policy would have permanently condemned a
		// seam that works.
		return Result{Verdict: VerdictError, Detail: "a tool call ran, but its output does not contain the probe's canary; " +
			opts.Harness + " did not run the scripted command (it may have refused it). Output: " + trimmed}
	}
	if strings.Contains(trimmed, remoteHost) {
		return Result{Verdict: VerdictOK,
			Detail:     fmt.Sprintf("tool output reports hostname %q, the target", remoteHost),
			ToolOutput: trimmed}
	}
	return Result{Verdict: VerdictBypassed,
		Detail: fmt.Sprintf("tool output does not contain the target's hostname %q; "+
			"the command ran somewhere else (locally). Observed output: %q", remoteHost, trimmed),
		ToolOutput: trimmed}
}

// echoedMarker reports whether the canary appears as output rather than as part
// of a command quoted back.
//
// The probe's command ends `echo <marker>; hostname`, so wherever the marker
// was actually printed it ends a line. Where it was merely quoted back it does
// not: a refusal carries the rest of the command after it —
// `… && echo <marker>; hostname'` — and so does the header some harnesses print
// before the output, `Command: echo <marker>; hostname`. Substring matching
// could not tell the two apart, which is how codex declining `rm -f` came to be
// recorded as reach failing to intercept it.
//
// Requiring the marker to be alone on its line is too strict in the other
// direction: Gemini frames tool output as `Stdout: <marker>`, and a real pass
// was rejected for having a label in front of it. The rule is therefore that
// the marker ends the line and was not the argument of the echo that would have
// printed it.
func echoedMarker(output, marker string) bool {
	for _, line := range strings.Split(output, "\n") {
		before, ok := strings.CutSuffix(strings.TrimRight(line, " \t\r"), marker)
		if !ok {
			continue
		}
		if strings.HasSuffix(strings.TrimRight(before, " \t"), "echo") {
			continue
		}
		return true
	}
	return false
}

// targetHostname asks the session's target for its hostname through the same
// transport path the shell shim uses, minus the shim's working-directory
// bookkeeping, which a fixed command does not need.
func targetHostname(ctx context.Context, sess *session.Session) (string, error) {
	t, err := sess.Transport()
	if err != nil {
		return "", err
	}
	res, err := t.Run(ctx, reach.ExecRequest{Command: "hostname", MaxOutput: 4 << 10})
	if err != nil {
		return "", err
	}
	if res.Code != 0 {
		return "", fmt.Errorf("hostname exited %d: %s", res.Code, strings.TrimSpace(string(res.Stderr)))
	}
	host := strings.TrimSpace(string(res.Stdout))
	if host == "" {
		return "", fmt.Errorf("the target answered `hostname` with nothing")
	}
	return host, nil
}

// codexArgs builds the argument vector for the probe turn.
//
// The provider flags point codex at the mock and at nothing else: a fresh
// provider entry (offline, no auth, no websockets) selected as model_provider,
// with the wire_api codex ≥ 0.148 still speaks. `-a never` forbids approval
// prompts the probe cannot answer, `exec --ephemeral --skip-git-repo-check`
// runs one non-interactive turn that writes no state and needs no repository.
// The sandbox flags mirror `reach codex`: a restricted network policy against
// a remote environment requires an executor-local proxy the reach exec-server
// does not provide, so the probe would be measuring the sandbox instead of the
// seam.
func codexArgs(baseURL string) []string {
	return []string{
		"-c", `model_providers.reach.name="reach"`,
		"-c", fmt.Sprintf("model_providers.reach.base_url=%q", baseURL),
		"-c", `model_providers.reach.wire_api="responses"`,
		"-c", `model_providers.reach.requires_openai_auth=false`,
		"-c", `model_providers.reach.supports_websockets=false`,
		"-c", `model_provider="reach"`,
		"-c", `model="reach-mock"`,
		"-c", `sandbox_mode="workspace-write"`,
		"-c", `sandbox_workspace_write.network_access=true`,
		"-a", "never",
		"exec", "--ephemeral", "--skip-git-repo-check",
		"Follow the tool-call instructions exactly.",
	}
}

// kimiArgs builds the argument vector for the probe turn. Print mode (`-p`)
// runs one non-interactive turn and exits; --yolo and --auto are rejected in
// combination with it, so approval behaviour is whatever the harness's print
// mode defaults to — sufficient for a tool call the mock instructs outright.
// reads its provider configuration from KIMI_MODEL_* variables, so the mock's
// URL travels through the environment instead.
func kimiArgs(_ string) []string {
	return []string{"-p", "Follow the tool-call instructions exactly."}
}

// reachSessionKey is dropped from every inherited probe environment before the
// probe's own value is appended.
//
// A duplicate key is resolved by whichever end of the block the child reads
// first, so an inherited REACH_SESSION — from the operator's shell, or from
// `reach <target> claude` setting it in this process — could win over the one
// the probe is measuring. The shim would then route the probe's tool calls
// through the operator's real session, and the seam under test would be
// reported on the strength of a run that never used it.
const reachSessionKey = "REACH_SESSION"

// baseProbeEnv builds the part of the probe environment every harness shares:
// REACH_SESSION binding the shim to the probe session exactly as `reach
// <harness>` does, and the shim directory leading PATH — the seam under test.
// The strip predicate drops inherited variables the harness must not see.
func baseProbeEnv(sessName, shimDir string, strip func(key string) bool) []string {
	var env []string
	for _, kv := range os.Environ() {
		key, value, _ := strings.Cut(kv, "=")
		if strip(key) || key == reachSessionKey {
			continue
		}
		if strings.EqualFold(key, "PATH") {
			// The key's original spelling is kept: on Windows a second PATH
			// that differs only in case would leave the child's search path to
			// chance (see cmd/reach/env.go, where this mistake was a safety
			// bug, not a style one).
			env = append(env, key+"="+shimDir+string(filepath.ListSeparator)+value)
			continue
		}
		env = append(env, kv)
	}
	return append(env, "REACH_SESSION="+sessName)
}

// probeWorkspace is the directory the probe process will treat as its project
// root — cmd.Dir when the spec pins one, and this process's own working
// directory otherwise, which is what exec.Cmd itself falls back to.
func probeWorkspace(dir string) string {
	if dir != "" {
		return dir
	}
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return cwd
}

// codexPrepare writes the environments.toml that routes the probe's codex to
// the reach exec-server — the seam under test. The throwaway CODEX_HOME gets
// exactly what `reach codex` puts in its managed one: this reach binary as the
// environment's program, bound to the probe's session. The probe therefore
// measures the exec-server seam end to end: mock model -> codex tool call ->
// process/start on the exec-server -> the session's target.
func codexPrepare(home, sessName string) error {
	reachPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate the reach binary: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(reachPath); err == nil {
		reachPath = resolved
	}
	toml := execserver.EnvironmentsTOML(reachPath, sessName)
	if err := os.WriteFile(filepath.Join(home, "environments.toml"), []byte(toml), 0o600); err != nil {
		return fmt.Errorf("write environments.toml: %w", err)
	}
	return nil
}

// codexEnv builds the environment for the probe's codex process.
//
// Two things are deliberate here beyond the shared base. CODEX_HOME is a
// throwaway directory so the operator's real codex config and credentials
// cannot interfere — and so the probe writes nothing to them. OPENAI_API_KEY
// is removed outright: the mock needs no auth, and a stray key must never let
// a probe turn reach a real provider. The mock's URL travels through argv
// (model_providers.reach.base_url), not the environment.
func codexEnv(sessName, shimDir, home, _ string) []string {
	env := baseProbeEnv(sessName, shimDir, func(key string) bool {
		return key == "OPENAI_API_KEY"
	})
	return append(env, "CODEX_HOME="+home)
}

// kimiEnv builds the environment for the probe's kimi process.
//
// Every inherited KIMI_* variable is stripped: the operator's shell may carry
// a live KIMI_API_KEY or session state (KIMI_CODE_*), and either one leaking
// into the probe would let a scripted turn touch a real provider or the
// operator's real kimi configuration. The probe then sets exactly the
// variables it needs: a throwaway KIMI_CODE_HOME, the mock's base URL, a dummy
// key that satisfies kimi's auth check without being able to authenticate
// anywhere, the openai provider type (the chat-completions dialect), and
// telemetry off — the probe is not a usage event anyone should count.
//
// KIMI_SHELL_PATH directs the patched kimi bundle to use the PATH-shim's bash
// instead of the hard-coded /bin/bash candidates the stock bundle probes.
// Without it the patched binary would still fall back to /bin/bash and the
// shim would never intercept kimi's Bash tool calls.
func kimiEnv(sessName, shimDir, home, baseURL string) []string {
	env := baseProbeEnv(sessName, shimDir, func(key string) bool {
		return strings.HasPrefix(key, "KIMI_")
	})
	return append(env,
		"KIMI_CODE_HOME="+home,
		"KIMI_MODEL_NAME=reach-mock",
		"KIMI_MODEL_API_KEY=dummy",
		"KIMI_MODEL_BASE_URL="+baseURL,
		"KIMI_MODEL_PROVIDER_TYPE=openai",
		"KIMI_DISABLE_TELEMETRY=1",
		"KIMI_SHELL_PATH="+filepath.Join(shimDir, "bash"),
	)
}

// gooseArgs builds the argument vector for the probe turn.
//
// `run` is goose's non-interactive mode. `--no-session` prevents goose from
// creating a session file in the managed home. `--no-profile` skips the user's
// profile extensions so only what the probe explicitly requests is loaded.
// `--with-builtin developer` adds the developer extension (which exposes the
// shell tool) without reading it from the managed home's (empty) config.yaml.
// `--text` provides the scripted prompt for the one probe turn.
func gooseArgs(_ string) []string {
	return []string{
		"run",
		"--no-session",
		"--no-profile",
		"--with-builtin", "developer",
		"--text", "Follow the tool-call instructions exactly.",
	}
}

// gooseEnv builds the environment for the probe's goose process.
//
// All inherited GOOSE_* vars and the standard OpenAI endpoint vars are
// stripped: the operator's shell may carry a live GOOSE_PROVIDER pointing at
// Anthropic, a real OPENAI_API_KEY, or other provider settings, any of which
// could let a scripted turn escape to a real model. The probe replaces them
// with exactly what it needs: a throwaway GOOSE_PATH_ROOT, the mock's base
// URL via OPENAI_BASE_URL, a dummy key, the openai provider (chat-completions
// dialect), GOOSE_SHELL pointing at the PATH shim, and telemetry disabled so
// the probe does not count as a usage event.
func gooseEnv(sessName, shimDir, home, baseURL string) []string {
	env := baseProbeEnv(sessName, shimDir, func(key string) bool {
		return strings.HasPrefix(key, "GOOSE_") ||
			key == "OPENAI_BASE_URL" || key == "OPENAI_HOST" || key == "OPENAI_API_KEY"
	})
	return append(env,
		"GOOSE_PATH_ROOT="+home,
		"GOOSE_PROVIDER=openai",
		"GOOSE_MODEL=reach-mock",
		"GOOSE_SHELL="+filepath.Join(shimDir, "bash"),
		"OPENAI_BASE_URL="+baseURL,
		"OPENAI_API_KEY=dummy",
		"GOOSE_TELEMETRY_DISABLED=1",
	)
}

// geminiArgs builds the argument vector for the probe turn.
//
// --yolo sets ApprovalMode.YOLO so the shell tool executes without waiting for
// user confirmation — a requirement for a headless, non-TTY probe run. -p runs
// Gemini in non-interactive (headless) mode with the given prompt; the prompt
// is read in a single turn and the process exits. HOME is managed by geminiEnv
// so the probe's settings.json (with excludeTools) takes effect.
func geminiArgs(_ string) []string {
	return []string{"--yolo", "-p", "Follow the tool-call instructions exactly."}
}

// geminiEnv builds the environment for the probe's gemini process.
//
// HOME is set to the throwaway directory so that ~/.gemini/settings.json points
// at the managed settings that exclude file tools. Every GEMINI_* and GOOGLE_*
// variable from the operator's shell is stripped: a live GEMINI_API_KEY could
// let a scripted turn reach a real model, and a GOOGLE_CLOUD_PROJECT could
// switch auth to Vertex AI which the mock does not speak. The probe replaces
// them with a dummy API key (satisfies the auth check without authenticating
// anywhere), the mock's base URL, and telemetry disabled.
func geminiEnv(sessName, shimDir, home, baseURL string) []string {
	env := baseProbeEnv(sessName, shimDir, func(key string) bool {
		return key == "HOME" ||
			strings.HasPrefix(key, "GEMINI_") ||
			strings.HasPrefix(key, "GOOGLE_")
	})
	return append(env,
		"HOME="+home,
		"GEMINI_API_KEY=dummy",
		"GOOGLE_GEMINI_BASE_URL="+baseURL,
		"GEMINI_TELEMETRY_OPT_OUT=1",
	)
}

// geminiPrepare writes a settings.json into the probe's throwaway HOME that
// mirrors what reach writes for the production managed home. The key safety
// properties are:
//   - ask_user / enter_plan_mode / exit_plan_mode are excluded so a headless
//     probe run cannot block waiting for TTY input
//   - all local-file tools are excluded so the mock turn's tool_call is
//     constrained to run_shell_command, the seam under test
//
// The exclude list must be kept in sync with writeManagedGeminiSettings in
// cmd/reach/gemini_home.go. Both use canonical TOOL_NAME constants from
// packages/core/src/tools/definitions/base-declarations.ts.
func geminiPrepare(home, _ string) error {
	geminiDir := filepath.Join(home, ".gemini")
	if err := os.MkdirAll(geminiDir, 0o700); err != nil {
		return fmt.Errorf("create .gemini dir: %w", err)
	}
	// Minimal JSON — same logical content as writeManagedGeminiSettings.
	// The "_reach" marker and indentation are omitted here; the probe's home
	// is ephemeral and doctor never inspects it.
	const settingsJSON = `{
  "excludeTools": [
    "read_file","write_file","replace","glob","grep_search","list_directory",
    "read_many_files","web_fetch","google_web_search","write_todos",
    "activate_skill","get_internal_docs",
    "ask_user","enter_plan_mode","exit_plan_mode",
    "update_topic","complete_task","invoke_agent",
    "tracker_create_task","tracker_update_task","tracker_get_task",
    "tracker_list_tasks","tracker_add_dependency","tracker_visualize",
    "read_mcp_resource","list_mcp_resources"
  ]
}
`
	p := filepath.Join(geminiDir, "settings.json")
	if err := os.WriteFile(p, []byte(settingsJSON), 0o600); err != nil {
		return fmt.Errorf("write .gemini/settings.json: %w", err)
	}
	return nil
}

// claudeCodeArgs builds the argument vector for the Claude Code probe.
//
// --dangerously-skip-permissions prevents the permission prompt that would
// otherwise block a headless (no-TTY) probe run from making tool calls. -p
// runs a single non-interactive turn with the given prompt. HOME is managed
// by claudeCodeEnv so the probe never touches the operator's real ~/.claude.
func claudeCodeArgs(_ string) []string {
	return []string{
		"--dangerously-skip-permissions",
		"-p", "Follow the tool-call instructions exactly.",
	}
}

// claudeCodeEnv builds the environment for the probe's claude process.
//
// HOME is redirected to a throwaway directory so ~/.claude (settings, auth
// cache, history) is isolated from the operator's real home. All ANTHROPIC_*
// and CLAUDE_* variables are stripped: a live ANTHROPIC_API_KEY or OAuth
// session could reach the real API and consume quota; a CLAUDE_* config
// could interfere with the probe's headless run. The probe replaces them with
// a dummy key (accepted by the mock without authenticating), the mock's base
// URL, and telemetry disabled.
//
// The second parameter (shimDir in the shared signature) carries the
// reach-shell-prefix alias path for Claude Code — it is the seam under test:
// every Bash tool call the harness makes will invoke that alias, and reach
// routes it to the session's target.
func claudeCodeEnv(sessName, shellPrefixPath, home, baseURL string) []string {
	var env []string
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		if key == "HOME" || key == reachSessionKey ||
			strings.HasPrefix(key, "ANTHROPIC_") ||
			strings.HasPrefix(key, "CLAUDE_") {
			continue
		}
		env = append(env, kv)
	}
	return append(env,
		"HOME="+home,
		"ANTHROPIC_API_KEY=dummy",
		"ANTHROPIC_BASE_URL="+baseURL,
		"CLAUDE_CODE_SHELL_PREFIX="+shellPrefixPath,
		"CLAUDE_TELEMETRY_OPT_OUT=1",
		"DO_NOT_TRACK=1",
		"REACH_SESSION="+sessName,
	)
}

// claudeCodePrepare creates the minimal ~/.claude skeleton the harness
// expects. Without it Claude Code may attempt interactive first-run setup
// that blocks a headless probe.
func claudeCodePrepare(home, _ string) error {
	claudeDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(claudeDir, 0o700); err != nil {
		return fmt.Errorf("create .claude directory: %w", err)
	}
	// A settings file with an empty permissions block prevents Claude Code from
	// prompting about tool permissions even before --dangerously-skip-permissions
	// takes effect in some versions.
	settingsPath := filepath.Join(claudeDir, "settings.json")
	if _, err := os.Stat(settingsPath); os.IsNotExist(err) {
		if err := os.WriteFile(settingsPath, []byte(`{"permissions":{}}`+"\n"), 0o600); err != nil {
			return fmt.Errorf("write .claude/settings.json: %w", err)
		}
	}
	return nil
}

// versionRE extracts the semver from a version line: codex prints "codex-cli
// 0.148.0", kimi prints a bare "0.37.2".
var versionRE = regexp.MustCompile(`\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.-]+)?`)

// NormalizeVersion reduces a version line to the string verdicts are cached
// under. The cache key has to survive cosmetic changes in the banner
// ("codex-cli 0.148.0" one release, something else the next), so anything
// that is not the semver itself is dropped; a line with no semver at all is
// kept whole rather than normalised into emptiness.
func NormalizeVersion(line string) string {
	if v := versionRE.FindString(line); v != "" {
		return v
	}
	return strings.TrimSpace(line)
}

// HarnessVersion reports the installed harness's version, normalised. Both
// supported harnesses answer `<binary> --version` with the version on the
// first line. It resolves the binary via exec.LookPath; for a specific binary
// path use HarnessVersionFromBinary.
func HarnessVersion(ctx context.Context, harness string) (string, error) {
	p, err := exec.LookPath(harness)
	if err != nil {
		return "", fmt.Errorf("%s is not installed or not in PATH", harness)
	}
	return HarnessVersionFromBinary(ctx, p)
}

// HarnessVersionFromBinary reports the version of an arbitrary harness binary,
// normalised. Use this when the binary to version-check is not on PATH — for
// example a reach-managed patched kimi bundle under ~/.reach/.
func HarnessVersionFromBinary(ctx context.Context, binPath string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, binPath, "--version").Output()
	if err != nil {
		return "", fmt.Errorf("%s --version: %w", binPath, err)
	}
	first, _, _ := strings.Cut(string(out), "\n")
	v := NormalizeVersion(first)
	if v == "" {
		return "", fmt.Errorf("%s --version printed nothing", binPath)
	}
	return v, nil
}

// randomHex returns n random bytes, hex-encoded.
func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand failing is a broken machine, not a condition to handle;
		// fall back to a timestamp so the marker is still unique per run.
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

// cappedBuffer is a bounded io.Writer for the probe's captured harness output.
// A wedged harness printing forever must not grow memory without bound; only
// the tail is diagnostic anyway.
type cappedBuffer struct {
	buf []byte
	max int
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if c.max == 0 {
		c.max = 64 << 10
	}
	c.buf = append(c.buf, p...)
	if len(c.buf) > c.max {
		c.buf = c.buf[len(c.buf)-c.max:]
	}
	return len(p), nil
}

// Tail returns the last captured output, trimmed, for error details.
func (c *cappedBuffer) Tail() string {
	tail := strings.TrimSpace(string(c.buf))
	if len(tail) > 400 {
		tail = "…" + tail[len(tail)-400:]
	}
	if tail == "" {
		return "(no output)"
	}
	return tail
}
