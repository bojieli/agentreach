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

	"github.com/bojieli/waldo/internal/session"
	"github.com/bojieli/waldo/internal/waldo"
)

// DefaultTimeout bounds one probe run. A healthy turn against the mock takes
// seconds; the bound exists for the harness that starts and then hangs, which
// must not hang `waldo <harness>` with it.
const DefaultTimeout = 120 * time.Second

// Harnesses the probe knows how to drive.
const (
	HarnessCodex = "codex"
	HarnessKimi  = "kimi"
)

// Options configures one Verify run.
type Options struct {
	// Harness selects which agent binary to probe: HarnessCodex or
	// HarnessKimi. It selects the mock's wire dialect, the spawn argv, the
	// process environment and the version command.
	Harness string
	// SessionName is the waldo session whose target is the ground truth for
	// where the scripted command should run.
	SessionName string
	// EnsureShim installs the PATH shell shims and returns the directory to
	// prepend to PATH. It is injected because shim installation lives in the
	// main package beside the shim's own dispatch.
	EnsureShim func() (string, error)
	// Timeout bounds the whole probe. Zero means DefaultTimeout.
	Timeout time.Duration
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
}

var harnessSpecs = map[string]harnessSpec{
	HarnessCodex: {dialect: DialectResponses, args: codexArgs, env: codexEnv},
	HarnessKimi:  {dialect: DialectChat, args: kimiArgs, env: kimiEnv},
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
// The probe inherits waldo's own posture rather than weakening it: the same
// session environment the shim needs (WALDO_SESSION, shim directory first on
// PATH) and, for codex, the same sandbox relaxation `waldo codex` applies
// (network access for the workspace-write sandbox, because the shim has to
// open an SSH connection). Each harness gets an isolated home directory and a
// scrubbed credential environment so it can only ever talk to the mock on
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
	if opts.EnsureShim == nil {
		return Result{Verdict: VerdictError, Detail: "no shim installer configured"}
	}
	shimDir, err := opts.EnsureShim()
	if err != nil {
		return Result{Verdict: VerdictError, Detail: "install the PATH shim: " + err.Error()}
	}
	binPath, err := exec.LookPath(opts.Harness)
	if err != nil {
		return Result{Verdict: VerdictError, Detail: opts.Harness + " is not installed or not in PATH"}
	}

	marker := "WALDO_SEAM_" + randomHex(8)
	mock := StartMock(marker, spec.dialect)
	defer mock.Close()

	home, err := os.MkdirTemp("", "waldo-"+opts.Harness+"-home-")
	if err != nil {
		return Result{Verdict: VerdictError, Detail: "create a temporary home directory: " + err.Error()}
	}
	defer func() { _ = os.RemoveAll(home) }()

	cmd := exec.CommandContext(ctx, binPath, spec.args(mock.BaseURL())...)
	cmd.Env = spec.env(sess.Name, shimDir, home, mock.BaseURL())
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
		return Result{Verdict: VerdictError, Detail: fmt.Sprintf(
			"%s did not finish the scripted turn within %s", opts.Harness, timeout)}
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
	if !strings.Contains(trimmed, marker) {
		// A tool call happened, but not this probe's command — the harness ran
		// something of its own. That says nothing about the seam.
		return Result{Verdict: VerdictError, Detail: "a tool call ran, but its output does not contain the probe's canary; " +
			opts.Harness + " ignored the scripted instruction. Output: " + trimmed}
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

// targetHostname asks the session's target for its hostname through the same
// transport path the shell shim uses, minus the shim's working-directory
// bookkeeping, which a fixed command does not need.
func targetHostname(ctx context.Context, sess *session.Session) (string, error) {
	t, err := sess.Transport()
	if err != nil {
		return "", err
	}
	res, err := t.Run(ctx, waldo.ExecRequest{Command: "hostname", MaxOutput: 4 << 10})
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
// The sandbox flags mirror `waldo codex`: without network access the shell
// shim cannot open its SSH connection, and the probe would be measuring the
// sandbox instead of the seam.
func codexArgs(baseURL string) []string {
	return []string{
		"-c", `model_providers.waldo.name="waldo"`,
		"-c", fmt.Sprintf("model_providers.waldo.base_url=%q", baseURL),
		"-c", `model_providers.waldo.wire_api="responses"`,
		"-c", `model_providers.waldo.requires_openai_auth=false`,
		"-c", `model_providers.waldo.supports_websockets=false`,
		"-c", `model_provider="waldo"`,
		"-c", `model="waldo-mock"`,
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

// baseProbeEnv builds the part of the probe environment every harness shares:
// WALDO_SESSION binding the shim to the probe session exactly as `waldo
// <harness>` does, and the shim directory leading PATH — the seam under test.
// The strip predicate drops inherited variables the harness must not see.
func baseProbeEnv(sessName, shimDir string, strip func(key string) bool) []string {
	var env []string
	for _, kv := range os.Environ() {
		key, value, _ := strings.Cut(kv, "=")
		if strip(key) {
			continue
		}
		if strings.EqualFold(key, "PATH") {
			// The key's original spelling is kept: on Windows a second PATH
			// that differs only in case would leave the child's search path to
			// chance (see cmd/waldo/env.go, where this mistake was a safety
			// bug, not a style one).
			env = append(env, key+"="+shimDir+string(filepath.ListSeparator)+value)
			continue
		}
		env = append(env, kv)
	}
	return append(env, "WALDO_SESSION="+sessName)
}

// codexEnv builds the environment for the probe's codex process.
//
// Two things are deliberate here beyond the shared base. CODEX_HOME is a
// throwaway directory so the operator's real codex config and credentials
// cannot interfere — and so the probe writes nothing to them. OPENAI_API_KEY
// cannot interfere — and so the probe writes nothing to them. OPENAI_API_KEY
// is removed outright: the mock needs no auth, and a stray key must never let
// a probe turn reach a real provider. The mock's URL travels through argv
// (model_providers.waldo.base_url), not the environment.
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
func kimiEnv(sessName, shimDir, home, baseURL string) []string {
	env := baseProbeEnv(sessName, shimDir, func(key string) bool {
		return strings.HasPrefix(key, "KIMI_")
	})
	return append(env,
		"KIMI_CODE_HOME="+home,
		"KIMI_MODEL_NAME=waldo-mock",
		"KIMI_MODEL_API_KEY=dummy",
		"KIMI_MODEL_BASE_URL="+baseURL,
		"KIMI_MODEL_PROVIDER_TYPE=openai",
		"KIMI_DISABLE_TELEMETRY=1",
	)
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
// first line.
func HarnessVersion(ctx context.Context, harness string) (string, error) {
	path, err := exec.LookPath(harness)
	if err != nil {
		return "", fmt.Errorf("%s is not installed or not in PATH", harness)
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		return "", fmt.Errorf("%s --version: %w", harness, err)
	}
	first, _, _ := strings.Cut(string(out), "\n")
	v := NormalizeVersion(first)
	if v == "" {
		return "", fmt.Errorf("%s --version printed nothing", harness)
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
