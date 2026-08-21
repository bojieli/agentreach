package envelope

import (
	"strings"
	"testing"
)

// FuzzParseClaudeCode feeds arbitrary command envelopes to the parser.
//
// This runs once per tool call on a string a closed binary composed, whose shape
// changes between harness versions. A panic here breaks every command the agent
// issues; silently dropping the user's command would be worse still, so the
// invariant checked is that whatever comes back is either a recognised
// decomposition or the input passed through untouched.
func FuzzParseClaudeCode(f *testing.F) {
	f.Add("echo hello")
	f.Add("source /tmp/snap.sh 2>/dev/null || true && eval 'ls' < /dev/null && pwd -P >| /tmp/claude-1-cwd")
	f.Add("&& pwd -P >|")
	f.Add("source ")
	f.Add("")

	f.Fuzz(func(t *testing.T, raw string) {
		p := ParseClaudeCode(raw)
		if !p.Recognised {
			if p.Command != raw {
				t.Fatalf("an unrecognised envelope was modified rather than forwarded:\n got %q\nwant %q", p.Command, raw)
			}
			return
		}
		// A recognised envelope may only ever have had a prefix and a suffix
		// removed. Anything else means reach is editing the model's command.
		if !strings.Contains(raw, p.Command) {
			t.Fatalf("parsed command is not a substring of the envelope:\n envelope %q\n command  %q", raw, p.Command)
		}
	})
}
