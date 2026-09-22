package envelope

import "testing"

// realEnvelope is a verbatim capture from Claude Code 2.1.233 via
// CLAUDE_CODE_SHELL_PREFIX. Keeping the real string here means a harness
// upgrade that changes the shape fails this test loudly instead of silently
// degrading behaviour in the field.
const realEnvelope = `source /home/ubuntu/.claude/shell-snapshots/snapshot-bash-1786757311642-4kz5ln.sh 2>/dev/null || true && { shopt -u extglob || setopt NO_EXTENDED_GLOB NO_BARE_GLOB_QUAL; } >/dev/null 2>&1 || true && { \builtin unalias -- 'unsetenv'; \builtin unset -f -- 'unsetenv'; } >/dev/null 2>&1 || true && eval 'echo PREFIX_MARKER_991' < /dev/null && pwd -P >| /tmp/claude-1003-cwd`

func TestParsesRealClaudeEnvelope(t *testing.T) {
	p := ParseClaudeCode(realEnvelope)

	if !p.Recognised {
		t.Fatal("real envelope not recognised")
	}
	if !p.StrippedSnapshot {
		t.Error("local snapshot source was not stripped")
	}
	if p.CwdFile != "/tmp/claude-1003-cwd" {
		t.Errorf("cwd file = %q want /tmp/claude-1003-cwd", p.CwdFile)
	}
	if want := "shell-snapshots"; contains(p.Command, want) {
		t.Errorf("command still references the local snapshot: %q", p.Command)
	}
	if contains(p.Command, "pwd -P") {
		t.Errorf("cwd bookkeeping still present in command: %q", p.Command)
	}
	if !contains(p.Command, "eval 'echo PREFIX_MARKER_991'") {
		t.Errorf("user command lost: %q", p.Command)
	}
	// The portable prelude must survive: it disables extglob, which changes
	// how the user's command is parsed.
	if !contains(p.Command, "shopt -u extglob") {
		t.Errorf("portable prelude lost: %q", p.Command)
	}
}

func TestLocalHomeIsNotLeakedToTarget(t *testing.T) {
	p := ParseClaudeCode(realEnvelope)
	if contains(p.Command, "/home/ubuntu/.claude") {
		t.Errorf("local home path would be sent to the target: %q", p.Command)
	}
}

func TestUnrecognisedEnvelopeIsForwardedUnchanged(t *testing.T) {
	raw := "echo just a plain command"
	p := ParseClaudeCode(raw)
	if p.Recognised {
		t.Error("plain command should not be reported as a recognised envelope")
	}
	if p.Command != raw {
		t.Errorf("plain command altered: %q", p.Command)
	}
	if p.CwdFile != "" {
		t.Errorf("invented a cwd file: %q", p.CwdFile)
	}
}

func TestCwdOnlyEnvelope(t *testing.T) {
	p := ParseClaudeCode(`eval 'ls' < /dev/null && pwd -P >| /tmp/claude-abcd-cwd`)
	if p.CwdFile != "/tmp/claude-abcd-cwd" {
		t.Errorf("cwd file = %q", p.CwdFile)
	}
	if p.Command != `eval 'ls' < /dev/null` {
		t.Errorf("command = %q", p.Command)
	}
}

func TestCommandContainingPwdIsNotMangled(t *testing.T) {
	// A user command that merely mentions pwd must not be mistaken for the
	// harness's trailing bookkeeping.
	raw := `eval 'echo $(pwd -P)' < /dev/null`
	p := ParseClaudeCode(raw)
	if p.CwdFile != "" {
		t.Errorf("mistook user command for cwd bookkeeping: %q", p.CwdFile)
	}
	if p.Command != raw {
		t.Errorf("user command altered: %q", p.Command)
	}
}

func TestDotFormOfSnapshotSource(t *testing.T) {
	p := ParseClaudeCode(`. /tmp/snap.sh 2>/dev/null || true && echo hi`)
	if !p.StrippedSnapshot {
		t.Error("dot-form source not stripped")
	}
	if p.Command != "echo hi" {
		t.Errorf("command = %q", p.Command)
	}
}

func contains(hay, needle string) bool {
	return len(hay) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(hay); i++ {
			if hay[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}

// A Claude Code hook command arrives at the shell prefix bare, with none of
// the envelope around it, and the caller runs an unrecognised string on the
// local machine. So the inverse has to hold too: a tool call must stay
// recognised when the parts that are conditional are missing. The snapshot
// source is absent whenever the harness has not written one, and without this
// the prelude alone was not enough to claim the string.
func TestPreludeAloneMarksAToolCall(t *testing.T) {
	raw := `{ shopt -u extglob || setopt NO_EXTENDED_GLOB NO_BARE_GLOB_QUAL; } >/dev/null 2>&1 || true && eval 'make build' < /dev/null`
	p := ParseClaudeCode(raw)
	if !p.Recognised {
		t.Fatalf("an envelope with no snapshot and no cwd redirect went unrecognised, "+
			"so the agent's command would run on the operator's machine: %q", raw)
	}
	if p.Command != raw {
		t.Errorf("command altered: %q", p.Command)
	}
}

// The unalias fragment is the other half of the prelude, and it is what
// survives when the shell is bash rather than zsh.
func TestUnaliasFragmentMarksAToolCall(t *testing.T) {
	raw := `{ \builtin unalias -- 'unsetenv'; } >/dev/null 2>&1 || true && eval 'ls' < /dev/null`
	if !ParseClaudeCode(raw).Recognised {
		t.Errorf("envelope with only the unalias fragment went unrecognised: %q", raw)
	}
}

// What a hook looks like. Nothing in it may be mistaken for an envelope, or
// reach would keep sending the operator's hooks to the target.
func TestHookCommandsAreNotEnvelopes(t *testing.T) {
	for _, raw := range []string{
		"/Users/someone/.local/bin/reach hook",
		"/Users/someone/.config/iterm2/cc-status",
		`jq -r '.tool_input.command' | tee -a /tmp/audit`,
		"npx prettier --write .",
	} {
		if ParseClaudeCode(raw).Recognised {
			t.Errorf("hook command taken for a tool-call envelope: %q", raw)
		}
	}
}
