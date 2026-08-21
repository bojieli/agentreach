package main

import (
	"context"
	"flag"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/bojieli/waldo/internal/fileops"
	"github.com/bojieli/waldo/internal/session"
	"github.com/bojieli/waldo/internal/transport"
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
	sessName := fs.String("session", "", "session name (default $WALDO_SESSION)")
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
		if s, loadErr = session.Load(sessionNameFromEnv(firstNonEmpty(*sessName, first(pos)))); loadErr != nil {
			return loadErr
		}
	}

	fmt.Printf("target   %s\n", s.Target.Describe())

	t, err := s.Transport()
	if err != nil {
		return err
	}
	defer func() { _ = t.Close() }()

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

	fmt.Println("\nFILE-OPERATION TIERS")
	for _, tier := range []waldo.Tier{waldo.TierPOSIX, waldo.TierPipe, waldo.TierHelper} {
		qualifies, why := caps.Qualifies(tier)
		mark := "no "
		if qualifies {
			mark = "yes"
		}
		line := fmt.Sprintf("  %-6s %s", tier, mark)
		if why != "" {
			line += "  — " + why
		}
		fmt.Println(line)
	}
	fmt.Printf("\n  Negotiated tier: %s (autonegotiation stops below helper)\n", caps.BestTier())
	reportConnectionReuse(ctx, s)
	if s.Pinned {
		fmt.Printf("  This session pins %s with --fileops.\n", s.Tier)
	}
	reportHelperFootprint(ctx, t)

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
	if !caps.RawStdin || !caps.RawStdout {
		fmt.Println("  ! This link is not 8-bit clean in both directions, so file content is")
		fmt.Println("    base64-framed. That is always safe and about a third slower on the wire.")
	}

	fmt.Println("\nLOCAL HARNESSES")
	reportHarness("claude", "Claude Code", claudeSeamNote())
	reportHarness("codex", "Codex", codexSeamNote())
	reportHarness("kimi", "Kimi Code", kimiSeamNote())
	reportHarness("goose", "Goose", gooseSeamNote())
	reportHarness("gemini", "Gemini CLI", geminiSeamNote())
	reportHarness("crush", "Crush", "server mode (crush server --host)")
	reportHarness("opencode", "opencode", "tool shadowing plugin")
	return nil
}

// reportConnectionReuse says whether commands will share one authenticated
// connection or open a new one each time.
//
// This is probed rather than inferred from the platform. Win32-OpenSSH has no
// ControlMaster today, but a version string says what was compiled in, not what
// a Match block or a restricted socket directory will permit at run time — and
// if a future release gains the feature, waldo should use it without waiting
// for someone to update a table of assumptions.
func reportConnectionReuse(ctx context.Context, s *session.Session) {
	if s.Target.Kind != session.KindSSH {
		return
	}
	ok, why := transport.DetectMultiplexing(ctx, transport.SSHConfig{
		Host: s.Target.Host,
		User: s.Target.User,
		Port: s.Target.Port,
	})
	if ok {
		fmt.Println("  Connection reuse: yes — one authenticated connection, reused")
		fmt.Println("    Measured at 4-5x faster per command than reconnecting, on real links.")
		return
	}
	fmt.Println("  Connection reuse: NO — every command opens and authenticates its own")
	if why != "" {
		fmt.Printf("    %s\n", why)
	}
	fmt.Println("    Every command pays a full connect and authentication: measured at 4-5x")
	fmt.Println("    the cost of a reused connection. If your key has a passphrase, run an")
	fmt.Println("    ssh-agent — without one, every tool call needs it, and waldo runs ssh in")
	fmt.Println("    batch mode, so they will fail rather than prompt.")
}

// reportHelperFootprint prints what waldo has left on this target.
//
// waldo's central promise is that it puts nothing on a machine you do not
// control. The only tier that breaks that promise does so at the operator's
// explicit request — and a promise like this one is worth nothing unless there
// is a command that shows whether it currently holds.
func reportHelperFootprint(ctx context.Context, t transport.Transport) {
	dir, err := fileops.HelperCacheDir(ctx, t)
	if err != nil {
		return
	}
	res, err := t.Run(ctx, waldo.ExecRequest{
		Command:   fmt.Sprintf("ls -1 %s 2>/dev/null || true", shellQuote(dir)),
		MaxOutput: 8 << 10,
	})
	if err != nil {
		return
	}
	names := strings.Fields(string(res.Stdout))
	if len(names) == 0 {
		fmt.Println("\n  waldo has written nothing to this target.")
		return
	}
	fmt.Printf("\n  waldo has installed %d file(s) in %s:\n", len(names), dir)
	for _, n := range names {
		fmt.Printf("    %s\n", n)
	}
	fmt.Println("  Remove them with: waldo helper uninstall")
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func reportHarness(bin, label, seam string) {
	if p, err := exec.LookPath(bin); err == nil {
		fmt.Printf("  %-12s found (%s) — seam: %s\n", label, filepath.Base(p), seam)
	} else {
		fmt.Printf("  %-12s not installed\n", label)
	}
}
