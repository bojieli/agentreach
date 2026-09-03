package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// The settings file is the whole of reach's grip on Claude Code: one JSON
// document decides whether the native file tools can reach the operator's own
// disk, and whether a shell command that names the target is allowed to run.
// It had no test.

type claudeSettings struct {
	Permissions struct {
		Deny  []string `json:"deny"`
		Allow []string `json:"allow"`
	} `json:"permissions"`
	Hooks map[string][]claudeMatcher `json:"hooks"`
}

func readSettings(t *testing.T, p string) claudeSettings {
	t.Helper()
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	var doc claudeSettings
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("settings file is not JSON Claude Code can read: %v\n%s", err, data)
	}
	return doc
}

func TestWriteDenySettingsDeniesTheFileToolsAndWiresTheBashHook(t *testing.T) {
	t.Setenv("REACH_HOME", t.TempDir())

	p, err := writeDenySettings("s")
	if err != nil {
		t.Fatalf("writeDenySettings: %v", err)
	}
	doc := readSettings(t, p)

	// The deny list is the safety property. A tool missing from it is a tool
	// that reads or writes the operator's own machine while the agent believes
	// it is working on the target.
	for _, tool := range deniedFileTools {
		if !contains(doc.Permissions.Deny, tool) {
			t.Errorf("%s is not denied; it would act on the local filesystem", tool)
		}
	}
	// Nothing may be handed an allowance here. The hook decides Bash, and it
	// decides it per command.
	if len(doc.Permissions.Allow) != 0 {
		t.Errorf("settings grant a blanket allowance: %v", doc.Permissions.Allow)
	}

	pre := doc.Hooks["PreToolUse"]
	if len(pre) != 1 || pre[0].Matcher != "Bash" {
		t.Fatalf("PreToolUse hooks are %+v, want exactly the Bash matcher", pre)
	}
	if len(pre[0].Hooks) != 1 || pre[0].Hooks[0].Type != "command" ||
		!strings.HasSuffix(pre[0].Hooks[0].Command, " hook") {
		t.Errorf("Bash hook does not call reach back: %+v", pre[0].Hooks)
	}
	// A PostToolUse hook on Bash would fire after the command already ran on
	// the target, which is too late to decide anything.
	if _, ok := doc.Hooks["PostToolUse"]; ok {
		t.Errorf("exec mode wires a PostToolUse hook; there is nothing left to decide by then")
	}
}

// Mirror mode's wiring is unrelated to the Bash decision and must stay that
// way: its file tools are rewritten to a local mirror, and a Bash matcher here
// would be a second, different answer to the same question.
func TestWriteMirrorSettingsWiresOnlyTheFileTools(t *testing.T) {
	t.Setenv("REACH_HOME", t.TempDir())

	p, err := writeMirrorSettings("s")
	if err != nil {
		t.Fatalf("writeMirrorSettings: %v", err)
	}
	doc := readSettings(t, p)

	for event, want := range map[string]string{
		"PreToolUse":  "Read|Write|Edit|NotebookEdit|Grep|Glob",
		"PostToolUse": "Write|Edit|NotebookEdit",
	} {
		got := doc.Hooks[event]
		if len(got) != 1 || got[0].Matcher != want {
			t.Errorf("%s hooks are %+v, want the %q matcher", event, got, want)
		}
	}
	// Mirror mode leaves the native file tools enabled on purpose.
	if len(doc.Permissions.Deny) != 0 {
		t.Errorf("mirror settings deny %v; the mirror exists so they do not have to", doc.Permissions.Deny)
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
