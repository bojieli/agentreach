package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/bojieli/waldo/internal/harnessprobe"
)

// This file holds the seam guard: the check that a harness's shell tool calls
// really travel through waldo's PATH shim before waldo launches it.
//
// It exists because harnesses change how they find a shell — Codex 0.148 moved
// from execvp("bash"), which the shim intercepts, to getpwuid_r plus an
// absolute path, and Kimi Code spawns its shell by absolute path outright —
// and the failure is silent from the agent's seat: commands run on the
// operator's machine while the agent reports working on the target. A version
// string cannot tell waldo which behaviour an installed binary has; only
// observing a command's hostname can. The observation (internal/harnessprobe)
// is expensive — one scripted turn, up to two minutes — so its verdict is
// cached per harness version and this guard consults the cache first.

// guardHarnessSeam decides whether the installed harness may be launched under
// waldo. It returns 0 to proceed and 1 to refuse.
//
// The policy is fail-closed on knowledge, fail-open on ignorance: a measured
// "bypassed" verdict refuses the launch, because launching would put the
// operator's own machine in the blast radius; a probe that could not run at
// all only warns, because the probe failing says nothing about the seam and
// refusing would ground every user whose machine cannot run it.
func guardHarnessSeam(ctx context.Context, harness, sessName string) int {
	version, err := harnessprobe.HarnessVersion(ctx, harness)
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"waldo: WARNING: cannot determine the %s version (%v)\n"+
				"waldo: The shell seam is unverified. If this %s resolves its shell by\n"+
				"waldo: absolute path, its commands will run LOCALLY while appearing remote.\n",
			harness, err, harness)
		return 0
	}

	entry, cached, err := harnessprobe.LoadVerdict(harness, version)
	if err != nil {
		fmt.Fprintf(os.Stderr, "waldo: WARNING: cannot read the seam verdict cache: %v\n", err)
	}
	if cached {
		d := harnessprobe.Gate(harness, version, &entry)
		if !d.RunProbe {
			if d.Message != "" {
				fmt.Fprint(os.Stderr, d.Message)
			}
			if !d.Allow {
				return 1
			}
			return 0
		}
	}

	fmt.Fprintf(os.Stderr,
		"waldo: verifying the %s %s shell seam (once per %s version, up to 2 min)...\n",
		harness, version, harness)
	res := harnessprobe.Verify(ctx, harnessprobe.Options{
		Harness:     harness,
		SessionName: sessName,
		EnsureShim:  ensurePathShim,
	})
	if res.Conclusive() {
		if err := harnessprobe.StoreVerdict(harness, version, res); err != nil {
			fmt.Fprintf(os.Stderr, "waldo: WARNING: could not cache the seam verdict: %v\n", err)
		}
	}
	d := harnessprobe.GateFromProbe(harness, version, res)
	if d.Message != "" {
		fmt.Fprint(os.Stderr, d.Message)
	}
	if !d.Allow {
		return 1
	}
	return 0
}

// guardKimiSeam is the fail-closed launch guard for kimi, specialised to use
// the resolved binary path and shim directory that cmdKimi already chose.
// It is separate from guardHarnessSeam because kimi's binary is not always
// the one exec.LookPath("kimi") returns — the patched npm bundle lives under
// ~/.waldo/ and has to be found by resolveKimiBinary before the guard runs.
func guardKimiSeam(ctx context.Context, sessName, binPath, shimDir string) int {
	version, err := harnessprobe.HarnessVersionFromBinary(ctx, binPath)
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"waldo: WARNING: cannot determine the kimi version (%v)\n"+
				"waldo: The shell seam is unverified. If the kimi binary was not patched for\n"+
				"waldo: KIMI_SHELL_PATH, its commands will run LOCALLY while appearing remote.\n",
			err)
		return 0
	}

	entry, cached, err := harnessprobe.LoadVerdict(harnessprobe.HarnessKimi, version)
	if err != nil {
		fmt.Fprintf(os.Stderr, "waldo: WARNING: cannot read the seam verdict cache: %v\n", err)
	}
	if cached {
		d := harnessprobe.Gate(harnessprobe.HarnessKimi, version, &entry)
		if !d.RunProbe {
			if d.Message != "" {
				fmt.Fprint(os.Stderr, d.Message)
			}
			if !d.Allow {
				return 1
			}
			return 0
		}
	}

	fmt.Fprintf(os.Stderr,
		"waldo: verifying the kimi %s shell seam (once per kimi version, up to 2 min)...\n",
		version)
	res := harnessprobe.Verify(ctx, harnessprobe.Options{
		Harness:     harnessprobe.HarnessKimi,
		SessionName: sessName,
		EnsureShim:  ensurePathShim,
		BinaryPath:  binPath,
	})
	if res.Conclusive() {
		if err := harnessprobe.StoreVerdict(harnessprobe.HarnessKimi, version, res); err != nil {
			fmt.Fprintf(os.Stderr, "waldo: WARNING: could not cache the seam verdict: %v\n", err)
		}
	}
	d := harnessprobe.GateFromProbe(harnessprobe.HarnessKimi, version, res)
	if d.Message != "" {
		fmt.Fprint(os.Stderr, d.Message)
	}
	if !d.Allow {
		return 1
	}
	return 0
}

// verifiableHarnesses are the harnesses `waldo harness verify` can probe.
var verifiableHarnesses = map[string]bool{
	harnessprobe.HarnessClaudeCode: true,
	harnessprobe.HarnessCodex:      true,
	harnessprobe.HarnessKimi:       true,
	harnessprobe.HarnessGoose:      true,
	harnessprobe.HarnessGemini:     true,
}

// cmdHarness dispatches `waldo harness <op>`.
func cmdHarness(ctx context.Context, args []string) error {
	if len(args) < 1 || args[0] != "verify" {
		return errors.New("usage: waldo harness verify claude|codex|kimi|goose|gemini [--session NAME]")
	}
	if len(args) < 2 {
		return errors.New("usage: waldo harness verify claude|codex|kimi|goose|gemini [--session NAME]")
	}
	if !verifiableHarnesses[args[1]] {
		return fmt.Errorf("unknown harness %q: claude, codex, kimi, goose, and gemini are verifiable", args[1])
	}
	return cmdHarnessVerify(ctx, args[1], args[2:])
}

// cmdHarnessVerify runs the seam probe on demand and caches the verdict.
//
// This is the operator-facing twin of the launch guard: the guard probes
// inline only when it has no cached verdict, so this command is how an
// operator re-checks after upgrading a harness, or checks before trusting a
// new version at all.
//
// --task-prefix runs the probe with a custom command prepended to the default
// "echo <marker>; hostname" canary. Use it to verify that a specific type of
// operation (file read, file write) also reaches the target. Probes with a
// task prefix are not cached — they are ad-hoc checks, not the canonical
// seam verdict.
//
// --binary points the probe at a specific binary path instead of resolving
// the harness by name on PATH. Useful when multiple versions are installed.
func cmdHarnessVerify(ctx context.Context, harness string, args []string) error {
	fs := newFlagSet("harness verify " + harness)
	sessName := sessionFlag(fs)
	taskPrefix := fs.String("task-prefix", "", "prepend this shell command to the probe's canary")
	binaryPath := fs.String("binary", "", "absolute path to the harness binary (overrides PATH lookup)")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return fmt.Errorf("unexpected argument %q", pos[0])
	}

	// For kimi, use the managed patched binary if available — the same one
	// cmdKimi will launch — so the verify result reflects the actual seam.
	binPath := *binaryPath
	if binPath == "" && harness == harnessprobe.HarnessKimi {
		if p, err := resolveKimiBinary(); err == nil {
			binPath = p
		}
	}

	var version string
	if binPath != "" {
		version, err = harnessprobe.HarnessVersionFromBinary(ctx, binPath)
	} else {
		version, err = harnessprobe.HarnessVersion(ctx, harness)
	}
	if err != nil {
		return err
	}
	if *taskPrefix != "" {
		fmt.Printf("%s %s — probing task [%s] against the session target...\n", harness, version, *taskPrefix)
	} else {
		fmt.Printf("%s %s — probing the shell seam against the session target...\n", harness, version)
	}

	// Claude Code uses the shell-prefix alias rather than a PATH shim.
	var ensureShellPrefixFn func() (string, error)
	if harness == harnessprobe.HarnessClaudeCode {
		ensureShellPrefixFn = ensureShim
	}

	res := harnessprobe.Verify(ctx, harnessprobe.Options{
		Harness:           harness,
		SessionName:       sessName(pos),
		EnsureShim:        ensurePathShim,
		EnsureShellPrefix: ensureShellPrefixFn,
		BinaryPath:        binPath,
		CommandPrefix:     *taskPrefix,
	})

	switch res.Verdict {
	case harnessprobe.VerdictOK:
		fmt.Printf("verdict: ok — %s\n", res.Detail)
		fmt.Printf("%s's shell is intercepted by waldo; its commands run on the target.\n", harness)
	case harnessprobe.VerdictBypassed:
		fmt.Printf("verdict: BYPASSED — %s\n", res.Detail)
		printBypassedRemediation(harness)
	default:
		fmt.Printf("verdict: error — %s\n", res.Detail)
		fmt.Println("the probe could not reach a conclusion; nothing was cached.")
		return fmt.Errorf("seam probe failed: %s", res.Detail)
	}

	// Task-prefix probes are not cached: they are ad-hoc checks, not the
	// canonical seam verdict. Only the unadorned "echo marker; hostname" probe
	// represents the question the launch guard asks.
	if *taskPrefix == "" {
		if err := harnessprobe.StoreVerdict(harness, version, res); err != nil {
			fmt.Fprintf(os.Stderr, "waldo: WARNING: could not cache the verdict: %v\n", err)
		} else {
			fmt.Printf("cached — the guard will use this verdict until %s's version changes.\n", harness)
		}
	}
	if res.Verdict == harnessprobe.VerdictBypassed {
		// A measured bypass is the answer the operator needed and a result CI
		// can act on, so it gets a non-zero exit without an extra message.
		return fmt.Errorf("%s bypasses waldo's shell shim", harness)
	}
	return nil
}

// printBypassedRemediation explains a bypassed verdict from `waldo harness
// verify`, per harness. The launch-time refusal carries the same substance;
// this is the calmer, operator-initiated version.
func printBypassedRemediation(harness string) {
	switch harness {
	case harnessprobe.HarnessClaudeCode:
		fmt.Println("claude's CLAUDE_CODE_SHELL_PREFIX seam failed: the scripted command ran")
		fmt.Println("on the local machine rather than the session's target.")
		fmt.Println("Remediation: check that CLAUDE_CODE_SHELL_PREFIX is being honoured by this")
		fmt.Println("version of Claude Code (`claude --version`). Run `waldo doctor` to check")
		fmt.Println("that the waldo-shell-prefix alias is current, then re-probe with:")
		fmt.Println("  waldo harness verify claude")
	case harnessprobe.HarnessCodex:
		fmt.Println("codex's exec-server seam failed: the scripted command did not run on the")
		fmt.Println("session's target, and `waldo codex` will refuse to launch this version.")
		fmt.Println("Remediation: run `waldo doctor` to check the session target, or report the")
		fmt.Println("failure — the seam is measured, so this verdict means the routing broke.")
	case harnessprobe.HarnessKimi:
		fmt.Println("kimi's shell seam failed. Either the kimi binary is not the patched")
		fmt.Println("version or KIMI_SHELL_PATH is not being honoured by this version.")
		fmt.Println("Remediation: run contrib/kimi-shell-path-patch.mjs against the kimi")
		fmt.Println("npm bundle under ~/.waldo/kimi-*/node_modules/@moonshot-ai/kimi-code/")
		fmt.Println("and re-run `waldo harness verify kimi`. Use --force only if you accept")
		fmt.Println("that commands will run on the LOCAL machine.")
	case harnessprobe.HarnessGoose:
		fmt.Println("goose's GOOSE_SHELL seam failed: the scripted command ran on the local")
		fmt.Println("machine rather than the session's target.")
		fmt.Println("Remediation: check that this version of goose reads GOOSE_SHELL")
		fmt.Println("(`goose --version`; the env var is documented in goose's developer")
		fmt.Println("extension). Use --force only if you accept that shell commands will")
		fmt.Println("run on the LOCAL machine.")
	case harnessprobe.HarnessGemini:
		fmt.Println("gemini's PATH shim seam failed: the scripted command ran on the local")
		fmt.Println("machine rather than the session's target.")
		fmt.Println("Remediation: check that this version of Gemini CLI resolves its shell")
		fmt.Println("by walking PATH (run `waldo doctor` to check the shim). Use --force")
		fmt.Println("only if you accept that shell commands will run on the LOCAL machine.")
	}
}

// claudeSeamNote describes the Claude Code shell seam for doctor.
func claudeSeamNote() string { return harnessSeamNote(harnessprobe.HarnessClaudeCode) }

// codexSeamNote describes the codex shell seam for doctor.
func codexSeamNote() string { return harnessSeamNote(harnessprobe.HarnessCodex) }

// kimiSeamNote describes the kimi shell seam for doctor.
func kimiSeamNote() string { return harnessSeamNote(harnessprobe.HarnessKimi) }

// gooseSeamNote describes the goose shell seam for doctor.
func gooseSeamNote() string { return harnessSeamNote(harnessprobe.HarnessGoose) }

// geminiSeamNote describes the gemini shell seam for doctor.
func geminiSeamNote() string { return harnessSeamNote(harnessprobe.HarnessGemini) }

// harnessSeamNote describes a harness's shell seam for doctor, including the
// cached verdict when one exists.
//
// Doctor's job is making invisible degradation visible, and a bypassed shell
// seam is the most dangerous invisible state waldo can be in: everything works,
// on the wrong machine.
func harnessSeamNote(harness string) string {
	seam := "PATH shim"
	switch harness {
	case harnessprobe.HarnessClaudeCode:
		seam = "CLAUDE_CODE_SHELL_PREFIX → waldo-shell-prefix"
	case harnessprobe.HarnessCodex:
		seam = "exec-server"
	case harnessprobe.HarnessKimi:
		seam = "KIMI_SHELL_PATH shim"
	case harnessprobe.HarnessGoose:
		seam = "GOOSE_SHELL env var"
	case harnessprobe.HarnessGemini:
		seam = "PATH shim (run_shell_command)"
	}
	unverified := fmt.Sprintf("%s (unverified — run `waldo harness verify %s`)", seam, harness)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// For kimi, version from the managed patched binary (same one waldo launches).
	var version string
	var err error
	if harness == harnessprobe.HarnessKimi {
		if binPath, berr := resolveKimiBinary(); berr == nil {
			version, err = harnessprobe.HarnessVersionFromBinary(ctx, binPath)
		} else {
			version, err = harnessprobe.HarnessVersion(ctx, harness)
		}
	} else {
		version, err = harnessprobe.HarnessVersion(ctx, harness)
	}
	if err != nil {
		return unverified
	}
	entry, ok, err := harnessprobe.LoadVerdict(harness, version)
	if err != nil || !ok {
		return unverified
	}
	switch entry.Verdict {
	case harnessprobe.VerdictOK:
		return seam + " (verified ok)"
	case harnessprobe.VerdictBypassed:
		return fmt.Sprintf("%s (BYPASSED by this version — `waldo %s` will refuse to launch)", seam, harness)
	default:
		return unverified
	}
}
