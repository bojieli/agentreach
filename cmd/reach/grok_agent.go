package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// grokLocalFileTools are the Grok Build built-in tools that call the local
// filesystem. Every one of them would act on the operator's own machine while
// the session is pointed at a target, so none of them may reach the model.
//
// The names are Grok's internal tool IDs — the ones `--tools` and
// `disallowedTools` match on — not the permission-rule prefixes. `write` is
// here because grok 1.0.5 advertises it in a live session; it appears in no
// version of the documentation, and a list built from the docs alone would
// leave the agent a local file writer.
var grokLocalFileTools = []string{
	"read_file",
	"search_replace",
	"list_dir",
	"grep",
	"write",
}

// managedGrokAgentProfile writes the agent definition reach launches grok with
// and returns its path.
//
// A profile rather than permission rules, and the difference is the whole
// adapter. `--deny Read` does deny read_file, but grok classifies a shell
// command that reads a file under the same prefix, so it denies `cat` as well
// — and `--deny Write` and `--deny Edit` likewise deny `cat > file` and
// `sed -i`. Those are the commands reach's own guidance tells the model to use
// once the native tools are gone, so a deny-rule adapter can run `hostname`
// and nothing else: no reading, writing or editing a file on the target by any
// route. A profile's disallowedTools removes the tools from the model's view
// without teaching the permission layer anything about shell commands, so the
// shell stays whole.
//
// The file has to exist before grok starts. `--agent` pointing at a path that
// is not there is not an error to grok: it falls back to the default agent,
// which has every local file tool enabled, and reports nothing. That failure
// looks exactly like a working session, so a write error here must stop the
// launch rather than be tolerated.
func managedGrokAgentProfile(sessName, guidance string) (string, error) {
	base := os.Getenv("REACH_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("locate home directory: %w", err)
		}
		base = filepath.Join(home, ".reach")
	}
	dir := filepath.Join(base, "grok-agents", sessName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}

	path := filepath.Join(dir, "reach-exec.md")
	if err := os.WriteFile(path, []byte(grokAgentProfile(guidance)), 0o600); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	return path, nil
}

// grokAgentProfile renders the agent definition.
//
// The body carries the same guidance `--rules` does. Grok's documentation does
// not say whether a profile's body replaces the built-in system prompt or is
// added to it, and the two readings want opposite things: if it replaces,
// dropping the guidance here would lose it entirely; if it adds, saying it
// twice costs a few tokens. The cheap failure is the right one to take.
func grokAgentProfile(guidance string) string {
	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("name: reach-exec\n")
	b.WriteString("description: Coding agent whose shell runs on a remote target through reach\n")
	b.WriteString("disallowedTools:\n")
	for _, t := range grokLocalFileTools {
		b.WriteString("  - " + t + "\n")
	}
	// Subagents are disabled twice, by flag and here. A child session that
	// inherited the default toolset would hold the local file tools this
	// profile exists to remove.
	b.WriteString("  - Agent\n")
	b.WriteString("---\n\n")
	b.WriteString("You are a coding agent working on a software project.\n\n")
	b.WriteString(guidance)
	b.WriteString("\n")
	return b.String()
}
