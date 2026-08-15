package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/bojieli/waldo/internal/session"
	"github.com/bojieli/waldo/internal/waldo"
)

// Build metadata injected at link time by the Makefile and by goreleaser.
//
// They are variables rather than constants because -X can only patch variables,
// and they are empty in a plain `go build`, where the compiled-in constant is
// the honest answer. A binary that reports a version it was not built as is a
// bug report waldo cannot act on: the first question about any harness-seam
// breakage is which version the operator is running.
var (
	buildVersion string
	buildCommit  string
	buildDate    string
)

func version() string {
	if buildVersion != "" {
		return buildVersion
	}
	return waldo.Version
}

// versionLine renders the full build identity for `waldo version`.
func versionLine() string {
	s := "waldo " + version()
	if buildCommit != "" {
		s += " (" + buildCommit
		if buildDate != "" {
			s += ", " + buildDate
		}
		s += ")"
	}
	return s + " " + runtime.GOOS + "/" + runtime.GOARCH
}

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
	if *tierName != "" && *tierName != "auto" {
		t, err := waldo.ParseTier(*tierName)
		if err != nil {
			return err
		}
		if t == waldo.TierAgent && *untrusted {
			return fmt.Errorf("refusing --fileops=agent on an --untrusted target: that tier installs a binary on the target")
		}
		s.Tier = t
		s.Pinned = true
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
	fmt.Printf("  fileops  %s%s\n", s.Tier, tierNote(s))
	fmt.Printf("  search   %s\n", searchEngine(s))
	if s.Untrusted {
		fmt.Printf("  policy   untrusted: no installs, no agent forwarding\n")
	}
	fmt.Printf("\nNext:\n  waldo claude          # launch Claude Code against this target\n  waldo exec -- ls -la  # or run something directly\n")
	return nil
}

// tierNote annotates the selected tier with how it was chosen, and with what
// it costs the target. An operator reading `waldo up` output should be able to
// tell at a glance whether anything was written to the machine they pointed at.
func tierNote(s *session.Session) string {
	switch {
	case s.TierReason != "":
		return " (" + s.TierReason + ")"
	case s.Tier == waldo.TierAgent:
		return " (installed a helper binary on the target; remove it with `waldo agent uninstall`)"
	case s.Pinned:
		return " (pinned)"
	case s.Tier == waldo.TierPOSIX:
		return " (nothing installed, nothing written)"
	default:
		return " (negotiated; nothing written to the target)"
	}
}

func searchEngine(s *session.Session) string {
	if s.Caps != nil && s.Caps.Ripgrep != "" {
		return "ripgrep (fast, structured)"
	}
	return "grep (no ripgrep on target)"
}

func cmdDown(_ context.Context, args []string) error {
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
	// Remove everything waldo created for this session. Leftover mirrored
	// files are the dangerous kind of debris: they are real files at
	// plausible-looking paths, and a later session of the same name would
	// find them and treat stale content as current.
	removed := cleanupSessionArtifacts(name)
	fmt.Printf("session %q closed\n", name)
	for _, r := range removed {
		fmt.Printf("  removed %s\n", r)
	}
	return nil
}

func cmdStatus(_ context.Context, _ []string) error {
	sessions, err := session.List()
	if err != nil {
		return err
	}
	if len(sessions) == 0 {
		fmt.Println("no waldo sessions.\n\nStart one with:\n  waldo up ssh://host/srv/app")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	// Writes to a tabwriter are buffered; the only error that matters surfaces
	// from Flush below.
	_, _ = fmt.Fprintln(w, "NAME\tTARGET\tMODE\tFILEOPS\tCWD\tPOLICY")
	for _, s := range sessions {
		policy := "-"
		if s.Untrusted {
			policy = "untrusted"
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			s.Name, s.Target.Describe(), s.Mode, s.Tier, s.Cwd(), policy)
	}
	return w.Flush()
}

func cmdEnv(_ context.Context, args []string) error {
	fs := flag.NewFlagSet("env", flag.ContinueOnError)
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	name := sessionNameFromEnv(first(pos))
	if _, err := session.Load(name); err != nil {
		return err
	}
	shim, shimErr := ensureShim()
	if shimErr != nil {
		return shimErr
	}
	fmt.Printf("export WALDO_SESSION=%s\n", name)
	fmt.Printf("export CLAUDE_CODE_SHELL_PREFIX=%s\n", shellQuote(shim))
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

// cleanupSessionArtifacts deletes the generated settings and mirrored files
// belonging to a session, returning what it removed.
func cleanupSessionArtifacts(name string) []string {
	var removed []string
	base := os.Getenv("WALDO_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil
		}
		base = filepath.Join(home, ".waldo")
	}
	candidates := []string{
		filepath.Join(base, "conf", name+".claude-settings.json"),
		filepath.Join(base, "conf", name+".claude-mirror-settings.json"),
		// Older layouts kept generated settings beside session state.
		filepath.Join(base, "sessions", name+".claude-settings.json"),
		filepath.Join(base, "sessions", name+".claude-mirror-settings.json"),
		filepath.Join(base, "mirror", name),
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err != nil {
			continue
		}
		if err := os.RemoveAll(c); err == nil {
			removed = append(removed, c)
		}
	}
	return removed
}
