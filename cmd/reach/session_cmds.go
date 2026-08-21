package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/bojieli/agentreach/internal/audit"
	"github.com/bojieli/agentreach/internal/fileops"
	"github.com/bojieli/agentreach/internal/session"
	"github.com/bojieli/agentreach/internal/transport"
	"github.com/bojieli/agentreach/internal/reach"
)

// Build metadata injected at link time by the Makefile and by goreleaser.
//
// They are variables rather than constants because -X can only patch variables,
// and they are empty in a plain `go build`, where the compiled-in constant is
// the honest answer. A binary that reports a version it was not built as is a
// bug report reach cannot act on: the first question about any harness-seam
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
	return reach.Version
}

// versionLine renders the full build identity for `reach version`.
func versionLine() string {
	s := "reach " + version()
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

// sessionFlag registers the --session flag and returns a resolver for the
// session name, combining the flag, a positional argument, $REACH_SESSION and
// the default in that order.
//
// It exists so every command that acts on a session accepts the name the same
// way. They did not: `reach env --session prod` failed with "flag provided but
// not defined" while `reach log --session prod` worked, so which spelling was
// right depended on which command you were running — and the error blamed the
// operator for a flag the tool uses everywhere else.
func sessionFlag(fs *flag.FlagSet) func([]string) string {
	name := fs.String("session", "", "session name (default $REACH_SESSION)")
	return func(pos []string) string {
		return sessionNameFromEnv(firstNonEmpty(*name, first(pos)))
	}
}

func sessionNameFromEnv(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if v := os.Getenv("REACH_SESSION"); v != "" {
		return v
	}
	return defaultSessionName
}

func cmdUp(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("up", flag.ContinueOnError)
	name := fs.String("name", defaultSessionName, "session name")
	mode := fs.String("mode", string(session.ModeExec), "exec or mirror")
	untrusted := fs.Bool("untrusted", false, "target is not yours: never install anything, never forward an agent")
	tierName := fs.String("fileops", "", "pin a file-operation tier ("+reach.TierList()+")")
	timeout := fs.Duration("timeout", 2*time.Minute, "default per-command timeout")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 1 {
		return fmt.Errorf("usage: reach up <target> [--name N] [--untrusted]\n\nExample:\n  reach up ssh://build-box/srv/app")
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
		Tier:      reach.TierPOSIX,
	}
	if *tierName != "" && *tierName != "auto" {
		t, err := reach.ParseTier(*tierName)
		if err != nil {
			return err
		}
		if t == reach.TierHelper && *untrusted {
			return fmt.Errorf("refusing --fileops=helper on an --untrusted target: that tier installs a binary on the target")
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
	if s.Target.Kind == session.KindSSH {
		fmt.Printf("  connect  %s\n", connectionNote(s))
	}
	if s.Untrusted {
		fmt.Printf("  policy   untrusted: no installs, no agent forwarding\n")
	}
	fmt.Printf("\nNext:\n  reach claude          # launch Claude Code against this target\n  reach exec -- ls -la  # or run something directly\n")
	return nil
}

// sessionList renders session names for a sentence.
func sessionList(names []string) string {
	quoted := make([]string, 0, len(names))
	for _, n := range names {
		quoted = append(quoted, strconv.Quote(n))
	}
	if len(quoted) == 1 {
		return "session " + quoted[0] + " is"
	}
	return "sessions " + strings.Join(quoted, ", ") + " are"
}

// tierNote annotates the selected tier with how it was chosen, and with what
// it costs the target. An operator reading `reach up` output should be able to
// tell at a glance whether anything was written to the machine they pointed at.
func tierNote(s *session.Session) string {
	switch {
	case s.TierReason != "":
		return " (" + s.TierReason + ")"
	case s.Tier == reach.TierHelper:
		return " (installed a helper binary on the target; remove it with `reach helper uninstall`)"
	case s.Pinned:
		return " (pinned)"
	case s.Tier == reach.TierPOSIX:
		return " (nothing installed, nothing written)"
	default:
		return " (negotiated; nothing written to the target)"
	}
}

// connectionNote describes how commands will reach the target.
//
// Multiplexing is the difference between ~7 ms and ~130 ms per command, and
// between authenticating once and authenticating on every tool call. On a host
// where it is unavailable, an operator with a passphrase-protected key and no
// agent will meet that fact once per command, so it is worth a line at `up`
// rather than a discovery later.
func connectionNote(s *session.Session) string {
	if s.Multiplex {
		return "multiplexed (one authenticated connection, reused)"
	}
	note := "one connection per command"
	if s.MultiplexNote != "" {
		note += " — " + s.MultiplexNote
	}
	return note
}

func searchEngine(s *session.Session) string {
	if s.Caps != nil && s.Caps.Ripgrep != "" {
		return "ripgrep (fast, structured)"
	}
	return "grep (no ripgrep on target)"
}

func cmdDown(ctx context.Context, args []string) error {
	fs := newFlagSet("down")
	clean := fs.Bool("clean", false, "also remove anything reach installed on the target")
	sess := sessionFlag(fs)
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	name := sess(pos)

	// A session that will not load must still be removable.
	//
	// Loading used to be a precondition, which made `down` refuse exactly the
	// sessions most in need of removing — one written by a newer reach, one
	// naming a tier that no longer exists — and every message that suggests
	// `reach down` as the way out led to a command that declined to do it. The
	// operator's only remaining move was to delete a file out of ~/.reach by
	// hand, which is not something a tool should require.
	//
	// The session is only needed for the courtesy half of the work: closing the
	// multiplexed connection and clearing reach's footprint on the target.
	// Losing that is a nuisance; being unable to remove a session is a trap.
	s, loadErr := session.Load(name)
	switch {
	case loadErr == nil:
		// Close the multiplexed connection so nothing reach opened outlives the
		// session. Leaving a live master on someone else's server would be a
		// surprising residue for a tool whose premise is leaving no trace.
		//
		// Unless another session is working over it. The control socket is keyed
		// on the destination, so two sessions on one host share a connection,
		// and closing this one's would leave the other reconnecting on its next
		// tool call — in batch mode, so on a host that needs a password or a
		// token, failing rather than reconnecting.
		if t, err := s.Transport(); err == nil {
			reportOrRemoveFootprint(ctx, s, t, *clean)
			if sharing := s.SharesConnectionWith(); len(sharing) > 0 {
				fmt.Fprintf(os.Stderr,
					"reach: leaving the connection to %s open; %s still using it\n",
					s.Target.Describe(), sessionList(sharing))
			} else {
				_ = t.Close()
			}
		}
	case errors.Is(loadErr, os.ErrNotExist):
		return loadErr
	default:
		fmt.Fprintf(os.Stderr, "reach: %s could not be read, so nothing was cleaned up on the target:\n%s",
			name, indented(loadErr.Error()))
		if *clean {
			fmt.Fprintf(os.Stderr, "  --clean needs a readable session to know which target to clean.\n"+
				"  If reach installed a helper there, it is the only thing it wrote:\n"+
				"    ssh HOST 'rm -rf \"${XDG_CACHE_HOME:-$HOME/.cache}/reach\"'\n")
		}
		fmt.Fprintf(os.Stderr, "  Removing the local session state anyway.\n\n")
	}
	if err := session.Remove(name); err != nil {
		return err
	}
	// Remove everything reach created for this session. Leftover mirrored
	// files are the dangerous kind of debris: they are real files at
	// plausible-looking paths, and a later session of the same name would
	// find them and treat stale content as current.
	removed := cleanupSessionArtifacts(name)
	fmt.Printf("session %q closed\n", name)
	for _, r := range removed {
		fmt.Printf("  removed %s\n", r)
	}
	// The audit log deliberately outlives the session it describes: a record of
	// what an agent did on someone else's machine is not something to delete
	// because the session ended.
	if dir, err := session.Dir(); err == nil {
		if _, statErr := os.Stat(audit.Path(dir, name)); statErr == nil {
			fmt.Printf("  kept %s (what reach did on the target; `reach log %s`)\n",
				audit.Path(dir, name), name)
		}
	}
	return nil
}

func cmdStatus(_ context.Context, args []string) error {
	fs := newFlagSet("status")
	named := fs.String("session", "", "show only this session")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	// `reach status NAME` shows one session, as the help has always said it
	// does. Accepting the argument and listing everything anyway would look
	// like it had printed the one that was asked for.
	//
	// Unlike the other commands, an absent name here does not fall back to
	// $REACH_SESSION: a bare `reach status` lists everything, and having it
	// silently narrow to one session inside a harness's shell would hide the
	// others at exactly the moment someone is checking what is running.
	if len(pos) > 1 {
		return fmt.Errorf("status takes at most one session name (got %q)", strings.Join(pos, " "))
	}
	if name := firstNonEmpty(*named, first(pos)); name != "" {
		return statusOne(name)
	}
	sessions, broken, err := session.List()
	if err != nil {
		return err
	}
	if len(sessions) == 0 && len(broken) == 0 {
		fmt.Println("no reach sessions.\n\nStart one with:\n  reach up ssh://host/srv/app")
		return nil
	}
	if len(sessions) > 0 {
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
		if err := w.Flush(); err != nil {
			return err
		}
	}
	// A session that will not load is still configured in a harness somewhere.
	// Listing it, with the reason, is the difference between "reach forgot my
	// session" and a sentence saying what to do about it.
	for _, b := range broken {
		fmt.Fprintf(os.Stderr, "\n%s: cannot be loaded\n%s", b.Name, indented(b.Err.Error()))
	}
	return nil
}

// statusOne describes a single session.
//
// It reports only what the session file already knows, and never touches the
// target. `reach doctor` is the command that goes and asks; keeping status
// local means it still answers when the target is unreachable, which is exactly
// when someone wants to know what reach thinks it is connected to.
func statusOne(name string) error {
	s, err := session.Load(name)
	if err != nil {
		return err
	}
	fmt.Printf("session %q -> %s\n", s.Name, s.Target.Describe())
	fmt.Printf("  mode     %s\n", s.Mode)
	fmt.Printf("  cwd      %s\n", s.Cwd())
	fmt.Printf("  fileops  %s%s\n", s.Tier, tierNote(s))
	if s.Caps != nil && s.Caps.Uname != "" {
		fmt.Printf("  target   %s\n", s.Caps.Uname)
		fmt.Printf("  search   %s\n", searchEngine(s))
	}
	if s.Target.Kind == session.KindSSH {
		fmt.Printf("  connect  %s\n", connectionNote(s))
	}
	if s.Untrusted {
		fmt.Printf("  policy   untrusted: no installs, no agent forwarding\n")
	}
	if !s.Created.IsZero() {
		fmt.Printf("  started  %s\n", s.Created.Format(time.RFC3339))
	}
	return nil
}

// indented lays a multi-line explanation under the line that introduces it.
// These messages are several sentences long by design — an operator whose
// session will not load needs the reason and the way out — and unindented
// continuations read as if a new, unrelated error had begun.
func indented(msg string) string {
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimRight(msg, "\n"), "\n") {
		b.WriteString("  " + line + "\n")
	}
	return b.String()
}

func cmdEnv(_ context.Context, args []string) error {
	fs := newFlagSet("env")
	sess := sessionFlag(fs)
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	name := sess(pos)
	if _, err := session.Load(name); err != nil {
		return err
	}
	shim, shimErr := ensureShim()
	if shimErr != nil {
		return shimErr
	}
	fmt.Printf("export REACH_SESSION=%s\n", name)
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

// reportOrRemoveFootprint accounts for anything reach installed on the target.
//
// reach's central claim is that it leaves nothing behind, and exactly one tier
// breaks that on purpose, at the operator's request. Ending a session without
// mentioning the binary still sitting on someone else's machine would make the
// claim false by omission — the operator would reasonably believe `down` undid
// everything.
//
// It is reported rather than removed by default because the install is cached
// deliberately: the path carries reach's version, so the next session reuses it
// instead of re-uploading several megabytes. --clean is for when the point was
// to leave no trace.
func reportOrRemoveFootprint(ctx context.Context, s *session.Session, t transport.Transport, clean bool) {
	if s.Tier != reach.TierHelper {
		return
	}
	dir, err := fileops.HelperCacheDir(ctx, t)
	if err != nil {
		return
	}
	if !clean {
		fmt.Printf("  note: reach's helper binary is still installed on the target, in %s\n", dir)
		fmt.Printf("        remove it with: reach down --clean, or reach helper uninstall\n")
		return
	}
	res, err := t.Run(ctx, reach.ExecRequest{
		Command:   fmt.Sprintf("rm -rf %s && echo removed", shellQuote(dir)),
		MaxOutput: 4 << 10,
	})
	if err != nil || res.Code != 0 {
		fmt.Printf("  WARNING: could not remove %s from the target; it is still there\n", dir)
		return
	}
	fmt.Printf("  removed %s from the target\n", dir)
}

// cleanupSessionArtifacts deletes the generated settings and mirrored files
// belonging to a session, returning what it removed.
func cleanupSessionArtifacts(name string) []string {
	var removed []string
	base := os.Getenv("REACH_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil
		}
		base = filepath.Join(home, ".reach")
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
