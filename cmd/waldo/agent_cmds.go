package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/bojieli/waldo/internal/fileops"
	"github.com/bojieli/waldo/internal/session"
	"github.com/bojieli/waldo/internal/waldo"
)

const agentUsage = `waldo agent — the optional helper binary (tier 3)

  waldo agent status [session]      what waldo has installed on the target
  waldo agent uninstall [session]   remove everything waldo installed there

Tier 3 is the only tier that writes to a target. It is never selected
automatically, is refused on a session marked --untrusted, and everything it
installs lives in one directory that this command removes.
`

func cmdAgent(ctx context.Context, args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, agentUsage)
		return fmt.Errorf("expected a subcommand")
	}
	sub, rest := args[0], args[1:]

	fs := newFlagSet("agent " + sub)
	sessName := fs.String("session", "", "session name (default $WALDO_SESSION)")
	pos, err := parseFlags(fs, rest)
	if err != nil {
		return err
	}

	name := sessionNameFromEnv(firstNonEmpty(*sessName, first(pos)))
	s, err := session.Load(name)
	if err != nil {
		return err
	}
	t, err := s.Transport()
	if err != nil {
		return err
	}
	defer func() { _ = t.Close() }()

	dir, err := fileops.AgentCacheDir(ctx, t)
	if err != nil {
		return err
	}

	switch sub {
	case "status":
		res, err := t.Run(ctx, waldo.ExecRequest{
			Command:   fmt.Sprintf("ls -la %s 2>/dev/null || true", shellQuote(dir)),
			MaxOutput: 32 << 10,
		})
		if err != nil {
			return err
		}
		out := strings.TrimSpace(string(res.Stdout))
		if out == "" {
			fmt.Printf("%s: waldo has installed nothing.\n", s.Target.Describe())
			return nil
		}
		fmt.Printf("%s: %s\n\n%s\n", s.Target.Describe(), dir, out)
		return nil

	case "uninstall":
		// Only the directory waldo created is removed, and it is named
		// explicitly rather than derived from anything the target said, so a
		// compromised host cannot talk waldo into deleting something else.
		res, err := t.Run(ctx, waldo.ExecRequest{
			Command:   fmt.Sprintf("rm -rf %s && echo removed", shellQuote(dir)),
			MaxOutput: 8 << 10,
		})
		if err != nil {
			return err
		}
		if res.Code != 0 {
			return fmt.Errorf("could not remove %s from %s: %s",
				dir, s.Target.Describe(), strings.TrimSpace(string(res.Stderr)))
		}
		fmt.Printf("removed %s from %s\n", dir, s.Target.Describe())
		if s.Tier == waldo.TierAgent {
			fmt.Println("note: this session still pins --fileops=agent, so the next command reinstalls it.")
			fmt.Printf("      run `waldo up %s` without --fileops to stop using it.\n", s.Target.Raw)
		}
		return nil
	}

	fmt.Fprint(os.Stderr, agentUsage)
	return fmt.Errorf("unknown agent subcommand %q", sub)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
