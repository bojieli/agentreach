package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/bojieli/waldo/internal/fileops"
	"github.com/bojieli/waldo/internal/session"
	"github.com/bojieli/waldo/internal/waldo"
)

// cmdDoctor reports what a target supports and, more importantly, what it does
// not.
//
// waldo degrades rather than fails when a host lacks something, which is the
// right behaviour but makes the degradation invisible. doctor is where the
// invisible becomes visible: an operator should never discover that search
// silently fell back to plain grep by noticing their agent got worse.
func cmdDoctor(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}

	var s *session.Session
	if len(pos) > 0 && strings.Contains(pos[0], "/") {
		target, err := session.ParseTarget(pos[0])
		if err != nil {
			return err
		}
		s = &session.Session{Name: "doctor", Target: target, Tier: waldo.TierPOSIX}
	} else {
		var loadErr error
		if s, loadErr = session.Load(sessionNameFromEnv(first(pos))); loadErr != nil {
			return loadErr
		}
	}

	fmt.Printf("target   %s\n", s.Target.Describe())

	t, err := s.Transport()
	if err != nil {
		return err
	}
	defer t.Close()

	caps, err := fileops.Probe(ctx, t)
	if err != nil {
		fmt.Printf("status   UNREACHABLE\n\n%v\n", err)
		return nil
	}
	fmt.Printf("status   reachable\n")
	fmt.Printf("userland %s\n\n", caps.Uname)

	ok := func(b bool) string {
		if b {
			return "yes"
		}
		return "NO"
	}
	fmt.Println("CAPABILITIES")
	fmt.Printf("  stat flavour     %s\n", orNone(caps.StatFlavor))
	fmt.Printf("  base64           %s\n", orNone(caps.Base64Decode))
	fmt.Printf("  sha256           %s\n", orNone(caps.SHA256))
	fmt.Printf("  find             %s\n", ok(caps.HasFind))
	fmt.Printf("  find -printf     %s\n", ok(caps.FindPrintf))
	fmt.Printf("  ripgrep          %s\n", orNone(caps.Ripgrep))
	fmt.Printf("  python3          %s\n", ok(caps.Python3))

	fmt.Println("\nWHAT THIS MEANS")
	if caps.Ripgrep == "" {
		fmt.Println("  ! No ripgrep on the target. Search falls back to grep, which is slower")
		fmt.Println("    and cannot disambiguate paths containing a colon.")
	}
	if !caps.FindPrintf {
		fmt.Println("  ! No GNU find -printf. Directory listings cannot represent filenames")
		fmt.Println("    containing a newline; such entries are skipped rather than mangled.")
	}
	if caps.SHA256 == "" {
		fmt.Println("  ! No sha256 utility. Mirror mode cannot verify content and is unavailable.")
	}
	if caps.StatFlavor == "" {
		fmt.Println("  ! No usable stat. File metadata is unavailable; this target cannot be used.")
	}
	fmt.Printf("  Selected file-operation tier: %s\n", waldo.TierPOSIX)
	fmt.Println("  Nothing is installed on this target and nothing is written to its disk.")

	fmt.Println("\nLOCAL HARNESSES")
	reportHarness("claude", "Claude Code", "CLAUDE_CODE_SHELL_PREFIX")
	reportHarness("codex", "Codex", "PATH shim")
	reportHarness("kimi", "Kimi Code", "PATH shim")
	reportHarness("opencode", "opencode", "tool shadowing plugin")
	return nil
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func reportHarness(bin, label, seam string) {
	if p, err := lookPath(bin); err == nil {
		fmt.Printf("  %-12s found (%s) — seam: %s\n", label, filepath.Base(p), seam)
	} else {
		fmt.Printf("  %-12s not installed\n", label)
	}
}

func lookPath(bin string) (string, error) {
	return execLookPath(bin)
}

var execLookPath = func(bin string) (string, error) {
	return exeLook(bin)
}

func exeLook(bin string) (string, error) {
	path := os.Getenv("PATH")
	for _, dir := range filepath.SplitList(path) {
		p := filepath.Join(dir, bin)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s not found in PATH", bin)
}
