package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/bojieli/waldo/internal/audit"
	"github.com/bojieli/waldo/internal/mirror"
	"github.com/bojieli/waldo/internal/session"
)

// hookEvent is the JSON a harness sends a hook on stdin.
type hookEvent struct {
	HookEventName string          `json:"hook_event_name"`
	ToolName      string          `json:"tool_name"`
	ToolInput     json.RawMessage `json:"tool_input"`
	Cwd           string          `json:"cwd"`
}

// hookReply is the JSON a hook writes on stdout.
type hookReply struct {
	HookSpecificOutput *hookSpecific `json:"hookSpecificOutput,omitempty"`
	SystemMessage      string        `json:"systemMessage,omitempty"`
}

type hookSpecific struct {
	HookEventName            string          `json:"hookEventName"`
	PermissionDecision       string          `json:"permissionDecision,omitempty"`
	PermissionDecisionReason string          `json:"permissionDecisionReason,omitempty"`
	UpdatedInput             json.RawMessage `json:"updatedInput,omitempty"`
	AdditionalContext        string          `json:"additionalContext,omitempty"`
}

// pathTools carry a file_path that mirror mode rewrites.
var pathTools = map[string]bool{
	"Read": true, "Write": true, "Edit": true, "NotebookEdit": true,
}

// scanTools search the filesystem. They are refused in mirror mode.
//
// The mirror is sparse — it holds only files a tool has already touched — so
// running a search against it would return confidently incomplete results.
// An agent told "no matches" when matches exist will conclude the code does
// not exist and act on that. Denying, with a pointer to the shell equivalent
// that runs on the target, is the only honest option.
var scanTools = map[string]bool{"Grep": true, "Glob": true}

// runHook implements the harness hook protocol for mirror mode.
//
// It always exits 0 and always emits valid JSON. A hook that crashes or writes
// garbage can wedge a harness's turn, so every failure is reported as a
// decision the agent can read instead of as a broken hook.
func runHook(_ []string) int {
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		emit(hookReply{})
		return 0
	}
	var ev hookEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		emit(hookReply{})
		return 0
	}

	s, err := session.Load(sessionNameFromEnv(""))
	if err != nil || s.Mode != session.ModeMirror {
		emit(hookReply{}) // not our business
		return 0
	}

	if scanTools[ev.ToolName] && ev.HookEventName == "PreToolUse" {
		emit(deny(ev, fmt.Sprintf(
			"%s searches the local filesystem, but this session works on %s.\n"+
				"The local mirror holds only files already opened, so a search here would\n"+
				"silently miss matches. Use the shell instead — it runs on the target:\n"+
				"  rg 'pattern' %s        (or grep -rn if ripgrep is absent)\n"+
				"  find %s -name 'glob'",
			ev.ToolName, s.Target.Describe(), s.Target.Workspace, s.Target.Workspace)))
		return 0
	}
	if !pathTools[ev.ToolName] {
		emit(hookReply{})
		return 0
	}

	var input map[string]any
	if err := json.Unmarshal(ev.ToolInput, &input); err != nil {
		emit(hookReply{})
		return 0
	}
	rawPath, _ := input["file_path"].(string)
	if rawPath == "" {
		emit(hookReply{})
		return 0
	}

	mirrorRoot, err := mirrorRootFor(s.Name)
	if err != nil {
		emit(deny(ev, "waldo: "+err.Error()))
		return 0
	}

	ctx := context.Background()

	tr, err := s.Transport()
	if err != nil {
		emit(deny(ev, "waldo: "+err.Error()))
		return 0
	}
	sel, err := s.FileOps(ctx, tr)
	if err != nil {
		emit(deny(ev, "waldo: "+err.Error()))
		return 0
	}
	defer func() { _ = sel.Ops.Close() }()

	// A hook runs inside the agent's turn, so an unresponsive target must
	// become a denial the agent can read rather than a tool call that hangs.
	ctx, cancel := s.OperationContext(ctx)
	defer cancel()

	m := mirror.New(mirrorRoot, sel.Ops)

	targetPath := resolveTargetPath(rawPath, s.Target.Workspace, m)

	// Paths outside the workspace are genuinely local — the harness's own
	// config, a scratch file — and must be left alone.
	if !underWorkspace(targetPath, s.Target.Workspace) {
		emit(hookReply{})
		return 0
	}

	switch ev.HookEventName {
	case "PreToolUse":
		var local string
		var ferr error
		if ev.ToolName == "Write" {
			local, ferr = m.Prepare(ctx, targetPath)
		} else {
			local, ferr = m.Fetch(ctx, targetPath)
		}
		recordFileAction(s, "read", targetPath, 0, ferr)
		if ferr != nil {
			emit(deny(ev, fmt.Sprintf("waldo could not fetch %s from %s: %v",
				targetPath, s.Target.Describe(), ferr)))
			return 0
		}
		input["file_path"] = local
		updated, _ := json.Marshal(input)
		emit(hookReply{HookSpecificOutput: &hookSpecific{
			HookEventName:      "PreToolUse",
			PermissionDecision: "allow",
			UpdatedInput:       updated,
		}})

	case "PostToolUse":
		if ev.ToolName == "Read" {
			emit(hookReply{})
			return 0
		}
		pushErr := m.Push(ctx, targetPath)
		recordFileAction(s, "write", targetPath, 0, pushErr)
		if err := pushErr; err != nil {
			// The edit already happened locally; the agent must be told it did
			// not reach the target, or it will believe its change landed.
			emit(hookReply{HookSpecificOutput: &hookSpecific{
				HookEventName:     "PostToolUse",
				AdditionalContext: "waldo: THE CHANGE WAS NOT SAVED TO THE TARGET. " + err.Error(),
			}})
			return 0
		}
		emit(hookReply{})

	default:
		emit(hookReply{})
	}
	return 0
}

// recordFileAction appends one file operation to the session's audit log.
func recordFileAction(s *session.Session, action, target string, bytes int, err error) {
	dir, dirErr := session.Dir()
	if dirErr != nil {
		return
	}
	entry := audit.Entry{
		Target: s.Target.Describe(),
		Action: action,
		Path:   target,
		Bytes:  bytes,
	}
	if err != nil {
		entry.Error = err.Error()
	}
	audit.Append(dir, s.Name, entry)
}

// resolveTargetPath turns whatever the harness put in `file_path` into a path
// on the target.
//
// The `path` package, not `filepath`. These are the *target's* paths and the
// target is always POSIX, while `filepath` follows the rules of whichever
// machine waldo happens to be running on. On Windows it produced
// `\srv\app\main.go` and sent that to a Linux host — a mirror mode that does
// not work, and does not work quietly, since a backslash is a legal character
// in a POSIX filename and the target would cheerfully create one file with a
// very strange name.
func resolveTargetPath(rawPath, workspace string, m *mirror.Mirror) string {
	// A path already inside the mirror is the rewritten form coming back to us
	// on PostToolUse; recover the target path it stands for.
	if tp, ok := m.Target(rawPath); ok {
		return tp
	}
	if !strings.HasPrefix(rawPath, "/") {
		return path.Join(workspace, rawPath)
	}
	return path.Clean(rawPath)
}

// underWorkspace reports whether a target path lies inside the session's
// workspace.
//
// The escape check compares path *components*, not a string prefix: a file
// legitimately named "..config" starts with ".." without being outside
// anything, and rejecting it would send an ordinary dotfile down the
// "leave it alone, it is local" path, where a Read would silently return the
// operator's own file instead of the target's.
//
// POSIX semantics throughout, for the same reason as resolveTargetPath.
func underWorkspace(target, workspace string) bool {
	base := path.Clean("/" + strings.TrimPrefix(workspace, "/"))
	p := path.Clean("/" + strings.TrimPrefix(target, "/"))
	if p == base {
		return true
	}
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	return strings.HasPrefix(p, base)
}

func deny(ev hookEvent, reason string) hookReply {
	return hookReply{HookSpecificOutput: &hookSpecific{
		HookEventName:            ev.HookEventName,
		PermissionDecision:       "deny",
		PermissionDecisionReason: reason,
	}}
}

func emit(r hookReply) {
	data, err := json.Marshal(r)
	if err != nil {
		fmt.Println("{}")
		return
	}
	fmt.Println(string(data))
}

func mirrorRootFor(sessionName string) (string, error) {
	base := os.Getenv("WALDO_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".waldo")
	}
	dir := filepath.Join(base, "mirror", sessionName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}
