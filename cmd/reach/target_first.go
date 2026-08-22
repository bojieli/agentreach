package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"

	"github.com/bojieli/agentreach/internal/reach"
	"github.com/bojieli/agentreach/internal/session"
)

// runTargetFirst handles `reach <target> <command> [args...]`.
//
// This is the form most sessions want. A session that exists only for the
// length of one agent should not need three commands to live and die, and the
// two-step form had a worse failure than verbosity: `up` defaults the session
// name to "default", so a second `reach up` silently replaced the first
// session and it looked as though reach could only hold one target at a time.
// It could always hold many — they had to be named by hand. Here the target
// names them, so two terminals on two boxes need nothing but the two commands
// the operator was going to type anyway.
func runTargetFirst(ctx context.Context, args []string) int {
	spec := args[0]
	target, err := resolveTargetSpec(spec)
	if err != nil {
		if spelledAsTarget(spec) {
			fmt.Fprintln(os.Stderr, "reach:", err)
			return 2
		}
		// A bare word that is neither a command nor a machine is far more
		// often a mistyped command than a mistyped hostname, so the message
		// leads with that reading and offers the other.
		fmt.Fprintf(os.Stderr, "reach: unknown command %q, and not a target either:\n  %v\n\n", spec, err)
		fmt.Fprintf(os.Stderr, "  If you meant a command, `reach help` lists them.\n"+
			"  If you meant a machine, spell it out:  reach ssh://%s/srv/app claude\n", spec)
		return 2
	}

	o, rest, err := splitTargetArgs(args[1:])
	if err != nil {
		// The flag package reports its own errors, with a usage dump, before
		// returning them. A removed flag is reach's own message and has not
		// been printed by anyone.
		var removed *removedFlagError
		if errors.As(err, &removed) {
			fmt.Fprintln(os.Stderr, "reach:", err)
		}
		return 2
	}

	// Checked before the target is probed. Being told about a typo is worth
	// more than a session the operator did not get to use.
	if len(rest) > 0 && !knownCommands[rest[0]] {
		fmt.Fprintf(os.Stderr, "reach: %q is not a reach command.\n"+
			"  `reach <target> <command>` takes one target and then a command:\n"+
			"    reach %s claude\n", rest[0], spec)
		return 2
	}

	// Session bookkeeping goes to stderr whenever a command follows, because
	// stdout belongs to that command: `reach build-box exec -- cat config`
	// must pipe the file and nothing else.
	out := io.Writer(os.Stderr)
	if len(rest) == 0 {
		out = os.Stdout
	}
	s, err := ensureSession(ctx, target, o, out, commandNeedsTarget(rest))
	if err != nil {
		fmt.Fprintln(os.Stderr, "reach:", err)
		return 1
	}

	// Every command reach dispatches from here — and every tool call the
	// harness makes afterwards, since the launchers pass it on — resolves the
	// session through this variable.
	if err := os.Setenv("REACH_SESSION", s.Name); err != nil {
		fmt.Fprintln(os.Stderr, "reach:", err)
		return 1
	}

	if len(rest) == 0 {
		printNextSteps(os.Stdout, s)
		return 0
	}
	return dispatch(ctx, rest)
}

// splitTargetArgs separates the session flags that may follow a target from
// the command that follows them.
//
// flag.Parse stops at the first non-flag argument, which is exactly the split
// this form needs: `reach box --mode mirror claude --resume` configures the
// session with --mode and hands --resume to claude. It is the opposite of
// parseFlags, which the rest of reach uses because their flags may follow
// their positional arguments — here a flag after the command is the command's.
func splitTargetArgs(args []string) (*sessionOptions, []string, error) {
	// Stopping at the first non-flag argument matters here as well as below:
	// in `reach box claude --untrusted` the flag is claude's to refuse, not
	// reach's to explain.
	if err := removedFlag(args, true); err != nil {
		return nil, nil, err
	}
	fs := newFlagSet("target")
	o := registerSessionFlags(fs, "")
	fs.BoolVar(&o.fresh, "fresh", false,
		"probe the target again even if a session for it already exists")
	if err := fs.Parse(args); err != nil {
		return nil, nil, err
	}
	o.markSet(fs)
	return o, fs.Args(), nil
}

// spelledAsTarget reports whether a word could only have been meant as a
// target, which decides whether a failure to parse it is reported as a bad
// target or as an unknown command.
func spelledAsTarget(spec string) bool {
	return strings.Contains(spec, "://") || strings.Contains(spec, ":")
}

// resolveTargetSpec turns a command-line word into a target.
//
// A bare word is accepted only when this machine can already name the host: an
// ssh_config entry, a hosts file entry, an address, or a dotted name. reach
// connects with the operator's own ssh client, so that client's configuration
// is the right authority on what counts as a destination — and see sshhosts.go
// for why DNS is not asked.
func resolveTargetSpec(spec string) (*session.Target, error) {
	if spelledAsTarget(spec) {
		return session.ParseTarget(spec)
	}
	if !looksLikeHost(spec) {
		return nil, fmt.Errorf("it is not scheme://..., [user@]host:/path, or a hostname")
	}
	if found, why := findHost(spec); !found {
		return nil, errors.New(why)
	}
	// No path: the session works wherever a login on that host lands, which
	// Probe resolves against the host itself.
	return session.ParseTarget("ssh://" + spec)
}

// sessionOptions are the settings that describe a session rather than a
// command. `reach up` and the flags allowed between a target and its command
// take the same set, so that `reach up ssh://box/srv --mode mirror` and
// `reach ssh://box/srv --mode mirror claude` mean the same thing.
type sessionOptions struct {
	name    string
	mode    string
	tier    string
	timeout time.Duration
	fresh   bool
	// set records which of these the operator actually typed. A flag that was
	// merely defaulted must not count as disagreeing with an existing session,
	// or every command would re-probe a session it could have reused.
	set map[string]bool
}

func registerSessionFlags(fs *flag.FlagSet, defaultName string) *sessionOptions {
	o := &sessionOptions{}
	fs.StringVar(&o.name, "name", defaultName, "session name")
	fs.StringVar(&o.mode, "mode", string(session.ModeExec), "exec or mirror")
	fs.StringVar(&o.tier, "fileops", "", "pin a file-operation tier ("+reach.TierList()+")")
	fs.DurationVar(&o.timeout, "timeout", 2*time.Minute, "default per-command timeout")
	return o
}

// removedFlags are flags reach used to have, and what to say instead of
// letting the flag package answer with "flag provided but not defined" and a
// usage dump. A flag that quietly stopped doing anything would be worse than
// either: the operator would go on believing they had asked for something.
var removedFlags = map[string]string{
	"untrusted": "--untrusted has been removed.\n" +
		"It named two guarantees that now hold for every target, with no flag to ask for them:\n" +
		"  no credential is sent to a target, and no SSH agent is ever forwarded to one.\n" +
		"The third was that nothing would be installed there. Nothing is, unless you name\n" +
		"the helper tier yourself with --fileops=helper; autonegotiation never picks it.\n" +
		"Drop the flag. `reach doctor` reports what is on the target, and `reach helper\n" +
		"uninstall` removes it.",
}

// removedFlagError marks a flag reach used to have, so a caller can tell it
// apart from a flag-package error that has already been printed.
type removedFlagError struct{ msg string }

func (e *removedFlagError) Error() string { return e.msg }

// removedFlag reports a removed flag in the arguments reach itself owns.
//
// stopAtPositional draws the line for `reach <target> <command>`: the flags
// before the command are reach's, and everything from the command onwards
// belongs to another program, which must be left to answer for its own.
func removedFlag(args []string, stopAtPositional bool) error {
	for _, a := range args {
		if a == "--" {
			return nil
		}
		if !strings.HasPrefix(a, "-") {
			if stopAtPositional {
				return nil
			}
			continue
		}
		name, _, _ := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if why, ok := removedFlags[name]; ok {
			return &removedFlagError{why}
		}
	}
	return nil
}

func (o *sessionOptions) markSet(fs *flag.FlagSet) {
	o.set = map[string]bool{}
	fs.Visit(func(f *flag.Flag) { o.set[f.Name] = true })
}

// ensureSession returns a session bound to this target, reusing the one that
// is already bound to it when there is one.
//
// Reuse is what makes the one-shot form cheap enough to type every time: the
// probe costs a round trip and an authentication, and asking a host the same
// questions again because the operator ran a second command against it would
// be work with no answer attached.
func ensureSession(ctx context.Context, target *session.Target, o *sessionOptions, out io.Writer, connect bool) (*session.Session, error) {
	name := o.name
	if name == "" {
		name = pickSessionName(deriveSessionName(target), target)
	}

	if !o.fresh {
		existing, err := session.Load(name)
		if err == nil && sameTarget(existing.Target, target) && optionsAgree(existing, o) {
			if err := warmIfNeeded(ctx, existing, connect); err == nil {
				_, _ = fmt.Fprintf(out, "session %q -> %s (already up; `reach status %s` for what it can do)\n",
					existing.Name, existing.Target.Describe(), existing.Name)
				return existing, nil
			} else {
				// The session's state is fine; the connection is not. Probing
				// again is the repair, and it happens while the operator is
				// still present to answer for a credential.
				fmt.Fprintf(os.Stderr, "reach: %s did not answer (%v); probing it again\n",
					existing.Target.Describe(), err)
			}
		}
	}

	s, err := newSession(name, target, o)
	if err != nil {
		return nil, err
	}
	if err := bringUp(ctx, s); err != nil {
		return nil, err
	}
	printSessionSummary(out, s)
	return s, nil
}

// commandNeedsTarget reports whether the command about to run will actually
// talk to the target.
//
// `reach build-box log` reads a file in ~/.reach and nothing else. Opening a
// connection for it would be a wasted round trip on a good day and a hardware
// token touch on a bad one, for a command that never leaves the machine.
func commandNeedsTarget(rest []string) bool {
	if len(rest) == 0 {
		// No command: this is `reach up` by another spelling, and the
		// connection is the thing being established.
		return true
	}
	switch rest[0] {
	case "log", "status", "env", "help", "--help", "-h", "version", "--version", "-v":
		return false
	}
	return true
}

// warmIfNeeded warms a reused session's connection unless the command that
// follows will never use it.
func warmIfNeeded(ctx context.Context, s *session.Session, connect bool) error {
	if !connect {
		return nil
	}
	return warmConnection(ctx, s)
}

// warmConnection re-authenticates a reused session while the operator is still
// in front of the terminal.
//
// Every connection after this one runs in batch mode and cannot prompt, so a
// host that wants a passphrase, a password or a hardware token has exactly one
// moment to ask for it. That moment used to be `reach up`; in the one-shot
// form it is here.
func warmConnection(ctx context.Context, s *session.Session) error {
	t, err := s.InteractiveTransport()
	if err != nil {
		return err
	}
	// The transport is deliberately not closed. Closing tears down the
	// multiplexed master, which would throw away the authentication this call
	// exists to perform and hand the agent's first tool call a reconnect it
	// cannot complete. `reach down` is what ends a connection.
	ctx, cancel := s.OperationContext(ctx)
	defer cancel()
	_, err = t.Run(ctx, reach.ExecRequest{Command: "true", MaxOutput: 4 << 10})
	return err
}

// newSession builds an unprobed session from the operator's options.
func newSession(name string, target *session.Target, o *sessionOptions) (*session.Session, error) {
	s := &session.Session{
		Name:    name,
		Target:  target,
		Mode:    session.Mode(o.mode),
		Created: time.Now(),
		Timeout: o.timeout,
		Tier:    reach.TierPOSIX,
	}
	// A misspelled mode used to be accepted and behave as exec, because only
	// "mirror" is ever compared against. An operator who asked for mirror and
	// typed it wrong would have got the other mode silently.
	switch s.Mode {
	case session.ModeExec, session.ModeMirror:
	default:
		return nil, fmt.Errorf("unknown --mode %q: use exec or mirror", o.mode)
	}
	if o.tier != "" && o.tier != "auto" {
		t, err := reach.ParseTier(o.tier)
		if err != nil {
			return nil, err
		}
		s.Tier = t
		s.Pinned = true
	}
	return s, nil
}

// bringUp probes the target and records the session.
func bringUp(ctx context.Context, s *session.Session) error {
	fmt.Fprintf(os.Stderr, "probing %s ...\n", s.Target.Describe())
	if err := s.Probe(ctx); err != nil {
		return fmt.Errorf("cannot use %s: %w", s.Target.Describe(), err)
	}
	if err := s.Save(); err != nil {
		return err
	}
	if err := s.SetCwd(s.Target.Workspace); err != nil {
		// The session is usable, but every command will start in the target's
		// default directory rather than the workspace that was just verified.
		fmt.Fprintf(os.Stderr, "reach: could not record the starting directory: %v\n", err)
	}
	return nil
}

// sameTarget reports whether a session already points where this target does.
//
// A requested workspace of "" — `reach build-box claude` — matches whatever
// the session resolved it to. Asking the host again for a directory it has
// already answered for would cost a round trip to learn nothing.
func sameTarget(have, want *session.Target) bool {
	if have == nil || want == nil {
		return false
	}
	if have.Kind != want.Kind || have.Host != want.Host || have.User != want.User ||
		have.Port != want.Port || have.Container != want.Container {
		return false
	}
	return want.Workspace == "" || have.Workspace == want.Workspace
}

// optionsAgree reports whether an existing session was built the way this
// invocation asks for. Only flags the operator actually typed are compared:
// the rest are defaults, and a default must never silently overrule a session
// that was deliberately created with something else.
func optionsAgree(s *session.Session, o *sessionOptions) bool {
	if o.set["mode"] && session.Mode(o.mode) != s.Mode {
		return false
	}
	if o.set["timeout"] && o.timeout != s.Timeout {
		return false
	}
	if o.set["fileops"] {
		if o.tier == "" || o.tier == "auto" {
			return !s.Pinned
		}
		t, err := reach.ParseTier(o.tier)
		if err != nil || !s.Pinned || t != s.Tier {
			return false
		}
	}
	return true
}

// deriveSessionName names a session after the target it is bound to.
//
// The name is what `reach status`, `reach log` and `reach down` address, so it
// has to mean something to the operator reading it a day later. Host plus the
// last element of the workspace is short enough to type and specific enough to
// tell two sessions on one host apart.
func deriveSessionName(t *session.Target) string {
	var base string
	switch t.Kind {
	case session.KindSSH:
		base = t.Host
	case session.KindDocker, session.KindPodman:
		base = t.Container
	default:
		base = "local"
	}
	if leaf := path.Base(t.Workspace); t.Workspace != "" && leaf != "/" && leaf != "." && leaf != "" {
		base += "-" + leaf
	}
	return sanitizeSessionName(base)
}

// sanitizeSessionName forces a derived name into the shape Save will accept,
// since a hostname may carry characters a session name may not.
func sanitizeSessionName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-._")
	if out == "" {
		out = "target"
	}
	// Short of the 64-character limit, leaving room for a collision suffix.
	if len(out) > 48 {
		out = out[:48]
	}
	return out
}

// pickSessionName finds the name this target should own.
//
// It is the derived name if that name is free or already this target's, and a
// numbered variant otherwise — two directories on one host, or the same host
// under two logins, are different sessions and must not overwrite each other.
func pickSessionName(base string, target *session.Target) string {
	for i := 1; i <= 9; i++ {
		name := base
		if i > 1 {
			name = fmt.Sprintf("%s-%d", base, i)
		}
		existing, err := session.Load(name)
		if errors.Is(err, os.ErrNotExist) {
			return name
		}
		if err == nil && sameTarget(existing.Target, target) {
			return name
		}
		// A name held by a different target, or by a file that will not load,
		// belongs to someone else. Take the next one.
	}
	// Nine variants taken. A digest keeps the name deterministic, so the same
	// target still lands on the same session tomorrow.
	sum := sha256.Sum256([]byte(target.Describe()))
	return base + "-" + hex.EncodeToString(sum[:3])
}
