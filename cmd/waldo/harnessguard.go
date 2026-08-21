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

// verifiableHarnesses are the harnesses `waldo harness verify` can probe.
var verifiableHarnesses = map[string]bool{
	harnessprobe.HarnessCodex: true,
	harnessprobe.HarnessKimi:  true,
}

// cmdHarness dispatches `waldo harness <op>`.
func cmdHarness(ctx context.Context, args []string) error {
	if len(args) < 1 || args[0] != "verify" {
		return errors.New("usage: waldo harness verify codex|kimi [--session NAME]")
	}
	if len(args) < 2 {
		return errors.New("usage: waldo harness verify codex|kimi [--session NAME]")
	}
	if !verifiableHarnesses[args[1]] {
		return fmt.Errorf("unknown harness %q: only codex and kimi are verifiable", args[1])
	}
	return cmdHarnessVerify(ctx, args[1], args[2:])
}

// cmdHarnessVerify runs the seam probe on demand and caches the verdict.
//
// This is the operator-facing twin of the launch guard: the guard probes
// inline only when it has no cached verdict, so this command is how an
// operator re-checks after upgrading a harness, or checks before trusting a
// new version at all.
func cmdHarnessVerify(ctx context.Context, harness string, args []string) error {
	fs := newFlagSet("harness verify " + harness)
	sessName := sessionFlag(fs)
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return fmt.Errorf("unexpected argument %q", pos[0])
	}

	version, err := harnessprobe.HarnessVersion(ctx, harness)
	if err != nil {
		return err
	}
	fmt.Printf("%s %s — probing the shell seam against the session target...\n", harness, version)
	res := harnessprobe.Verify(ctx, harnessprobe.Options{
		Harness:     harness,
		SessionName: sessName(pos),
		EnsureShim:  ensurePathShim,
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

	if err := harnessprobe.StoreVerdict(harness, version, res); err != nil {
		fmt.Fprintf(os.Stderr, "waldo: WARNING: could not cache the verdict: %v\n", err)
	} else {
		fmt.Printf("cached — the guard will use this verdict until %s's version changes.\n", harness)
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
	case harnessprobe.HarnessCodex:
		fmt.Println("codex resolves its shell by absolute path; waldo cannot redirect its")
		fmt.Println("commands, and `waldo codex` will refuse to launch this version.")
		fmt.Println("Remediation: use Codex <= 0.147, or run codex on the target itself.")
	case harnessprobe.HarnessKimi:
		fmt.Println("kimi spawns its shell by absolute path; waldo cannot redirect its")
		fmt.Println("commands, and `waldo kimi` will refuse to launch this version.")
		fmt.Println("Note: Kimi's native read_file, write_file and multi_edit tools act on the")
		fmt.Println("LOCAL filesystem even when the shell seam works — use shell commands for")
		fmt.Println("file access.")
		fmt.Println("Remediation: run kimi on the target itself, or use --force if you accept")
		fmt.Println("local execution.")
	}
}

// codexSeamNote describes the codex shell seam for doctor.
func codexSeamNote() string { return harnessSeamNote(harnessprobe.HarnessCodex) }

// kimiSeamNote describes the kimi shell seam for doctor.
func kimiSeamNote() string { return harnessSeamNote(harnessprobe.HarnessKimi) }

// harnessSeamNote describes a harness's shell seam for doctor, including the
// cached verdict when one exists.
//
// Doctor's job is making invisible degradation visible, and a bypassed shell
// seam is the most dangerous invisible state waldo can be in: everything works,
// on the wrong machine.
func harnessSeamNote(harness string) string {
	const seam = "PATH shim"
	unverified := fmt.Sprintf("%s (unverified — run `waldo harness verify %s`)", seam, harness)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	version, err := harnessprobe.HarnessVersion(ctx, harness)
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
