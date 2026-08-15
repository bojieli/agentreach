package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/bojieli/waldo/internal/session"
	"github.com/bojieli/waldo/internal/waldo"
)

func version() string { return waldo.Version }

// defaultSessionName is used when the operator does not name a session, which
// is the common single-target case.
const defaultSessionName = "default"

func sessionNameFromEnv(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if v := os.Getenv("WALDO_SESSION"); v != "" {
		return v
	}
	return defaultSessionName
}

func cmdUp(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("up", flag.ContinueOnError)
	name := fs.String("name", defaultSessionName, "session name")
	mode := fs.String("mode", string(session.ModeExec), "exec or mirror")
	untrusted := fs.Bool("untrusted", false, "target is not yours: never install anything, never forward an agent")
	tierName := fs.String("fileops", "", "pin a file-operation tier (posix, sftp, pipe, agent)")
	timeout := fs.Duration("timeout", 2*time.Minute, "default per-command timeout")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 1 {
		return fmt.Errorf("usage: waldo up <target> [--name N] [--untrusted]\n\nExample:\n  waldo up ssh://build-box/srv/app")
	}

	target, err := session.ParseTarget(pos[0])
	if err != nil {
		return err
	}
	s := &session.Session{
		Name:      *name,
		Target:    target,
		Mode:      session.Mode(*mode),
		Created:   time.Now(),
		Untrusted: *untrusted,
		Timeout:   *timeout,
		Tier:      waldo.TierPOSIX,
	}
	if *tierName != "" {
		t, err := waldo.ParseTier(*tierName)
		if err != nil {
			return err
		}
		if t == waldo.TierAgent && *untrusted {
			return fmt.Errorf("refusing --fileops=agent on an --untrusted target: that tier installs a binary on the target")
		}
		s.Tier = t
	}

	fmt.Fprintf(os.Stderr, "probing %s ...\n", target.Describe())
	if err := s.Probe(ctx); err != nil {
		return fmt.Errorf("cannot use %s: %w", target.Describe(), err)
	}
	if err := s.Save(); err != nil {
		return err
	}
	_ = s.SetCwd(target.Workspace)

	fmt.Printf("session %q -> %s\n", s.Name, target.Describe())
	fmt.Printf("  target   %s\n", s.Caps.Uname)
	fmt.Printf("  fileops  %s\n", s.Tier)
	fmt.Printf("  search   %s\n", searchEngine(s))
	if s.Untrusted {
		fmt.Printf("  policy   untrusted: no installs, no agent forwarding\n")
	}
	fmt.Printf("\nNext:\n  waldo claude          # launch Claude Code against this target\n  waldo exec -- ls -la  # or run something directly\n")
	return nil
}

func searchEngine(s *session.Session) string {
	if s.Caps != nil && s.Caps.Ripgrep != "" {
		return "ripgrep (fast, structured)"
	}
	return "grep (no ripgrep on target)"
}

func cmdDown(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("down", flag.ContinueOnError)
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	name := sessionNameFromEnv(first(pos))

	s, err := session.Load(name)
	if err != nil {
		return err
	}
	// Close the multiplexed connection so nothing waldo opened outlives the
	// session. Leaving a live master on someone else's server would be a
	// surprising residue for a tool whose premise is leaving no trace.
	if t, err := s.Transport(); err == nil {
		_ = t.Close()
	}
	if err := session.Remove(name); err != nil {
		return err
	}
	fmt.Printf("session %q closed\n", name)
	return nil
}

func cmdStatus(ctx context.Context, args []string) error {
	sessions, err := session.List()
	if err != nil {
		return err
	}
	if len(sessions) == 0 {
		fmt.Println("no waldo sessions.\n\nStart one with:\n  waldo up ssh://host/srv/app")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tTARGET\tMODE\tFILEOPS\tCWD\tPOLICY")
	for _, s := range sessions {
		policy := "-"
		if s.Untrusted {
			policy = "untrusted"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			s.Name, s.Target.Describe(), s.Mode, s.Tier, s.Cwd(), policy)
	}
	return w.Flush()
}

func cmdEnv(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("env", flag.ContinueOnError)
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	name := sessionNameFromEnv(first(pos))
	if _, err := session.Load(name); err != nil {
		return err
	}
	self, exeErr := os.Executable()
	if exeErr != nil {
		return exeErr
	}
	fmt.Printf("export WALDO_SESSION=%s\n", name)
	fmt.Printf("export CLAUDE_CODE_SHELL_PREFIX=%s\n", shellQuote(self+" shell-prefix"))
	return nil
}

func shellQuote(s string) string {
	if !strings.ContainsAny(s, " \t'\"$`\\") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// first returns the first element or the empty string.
func first(v []string) string {
	if len(v) == 0 {
		return ""
	}
	return v[0]
}
