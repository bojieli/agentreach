package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/bojieli/agentreach/internal/audit"
	"github.com/bojieli/agentreach/internal/session"
)

const logUsage = `reach log — what reach did on the target

  reach log [session] [--limit N] [--json] [--failed] [--path]

reach records every command it runs on a target and every file it changes
there. This prints that record.

The point of it is the situation reach is built for: you pointed an agent at a
machine you do not own, and afterwards somebody asks what it did. Nothing is
sent anywhere — the log is a local file only you can read. Set ` + audit.DisableEnv + `=1
to turn it off.
`

func cmdLog(_ context.Context, args []string) error {
	fs := newFlagSet("log")
	sessName := fs.String("session", "", "session name (default $REACH_SESSION)")
	limit := fs.Int("limit", 50, "show at most this many entries (0 for all)")
	asJSON := fs.Bool("json", false, "emit the raw records")
	onlyFailed := fs.Bool("failed", false, "only actions that failed")
	showPath := fs.Bool("path", false, "print the log file's path and exit")
	fs.Usage = func() { fmt.Fprint(os.Stderr, logUsage) }
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}

	name := sessionNameFromEnv(firstNonEmpty(*sessName, first(pos)))
	dir, err := session.Dir()
	if err != nil {
		return err
	}
	if *showPath {
		fmt.Println(audit.Path(dir, name))
		return nil
	}

	// Read everything and narrow afterwards. Applying the limit first would
	// make `--failed --limit 5` mean "failures among the last five actions"
	// rather than "the last five failures", which is never what someone
	// investigating a failure wants.
	entries, err := audit.Read(dir, name, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("no audit log for session %q yet.\n"+
				"reach writes one as soon as it runs something on the target", name)
		}
		return err
	}

	if *onlyFailed {
		kept := entries[:0]
		for _, e := range entries {
			if e.Code != 0 || e.Error != "" {
				kept = append(kept, e)
			}
		}
		entries = kept
	}
	if *limit > 0 && len(entries) > *limit {
		entries = entries[len(entries)-*limit:]
	}
	if len(entries) == 0 {
		fmt.Println("nothing recorded yet")
		return nil
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		for _, e := range entries {
			if err := enc.Encode(e); err != nil {
				return err
			}
		}
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "WHEN\tACTION\tSTATUS\tDETAIL")
	for _, e := range entries {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
			e.Time.Format("15:04:05"), e.Action, logStatus(e), logDetail(e))
	}
	return w.Flush()
}

func logStatus(e audit.Entry) string {
	switch {
	case e.Error != "":
		return "FAILED"
	case e.Code != 0:
		return fmt.Sprintf("exit %d", e.Code)
	default:
		return "ok"
	}
}

func logDetail(e audit.Entry) string {
	detail := e.Command
	if detail == "" {
		detail = e.Path
	}
	// Commands are recorded verbatim, including newlines. A record per line is
	// what makes this readable; the full text is in --json.
	detail = strings.Join(strings.Fields(detail), " ")
	const width = 90
	if len(detail) > width {
		detail = detail[:width] + "…"
	}
	if e.Error != "" {
		detail += "  (" + strings.Join(strings.Fields(e.Error), " ") + ")"
	}
	return detail
}
