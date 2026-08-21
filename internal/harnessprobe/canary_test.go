package harnessprobe

import "testing"

// TestEchoedMarker pins the distinction the seam verdict rests on: whether the
// probe's canary was printed by the command, or merely quoted back in text
// about the command. Getting it wrong is not symmetric — a false "it ran" reads
// a refusal as a bypass and, because the verdict is cached, makes reach refuse
// to launch a harness whose seam works.
//
// Every string here is a real one, copied from a probe log.
func TestEchoedMarker(t *testing.T) {
	const marker = "REACH_SEAM_289b44a021f4d7d5"

	for _, tc := range []struct {
		name   string
		output string
		want   bool
	}{
		{
			// codex and kimi print the command's output and nothing else.
			name:   "bare output",
			output: marker + "\n8fd670bb2789\n",
			want:   true,
		},
		{
			// Gemini labels each stream. Requiring the marker to be alone on
			// its line rejected this, which is how a real pass was recorded as
			// inconclusive.
			name: "gemini labels the stream",
			output: "Command: echo " + marker + "; hostname\n" +
				"Directory: (root)\n" +
				"Stdout: " + marker + "\n8fd670bb2789\n\n" +
				"Stderr: (empty)\nExit Code: 0\n",
			want: true,
		},
		{
			// The header alone, with no Stdout — the shape of a tool call that
			// was reported but produced nothing.
			name:   "only the echoed command",
			output: "Command: echo " + marker + "; hostname\nDirectory: (root)\n",
			want:   false,
		},
		{
			// codex declining `rm -f`: the whole command comes back inside the
			// refusal, canary and all, and nothing ran anywhere.
			name: "codex refuses and quotes the command back",
			output: "exec_command failed for `/usr/bin/bash -lc 'echo reach_probe_write > /tmp/x.txt && " +
				"cat /tmp/x.txt && rm -f /tmp/x.txt && echo " + marker + "; hostname'`: " +
				"CreateProcess { message: \"Rejected(\\\"rm -f style commands are not permitted\\\")\" }",
			want: false,
		},
		{
			name:   "no canary at all",
			output: "8fd670bb2789\n",
			want:   false,
		},
		{
			name:   "trailing carriage return",
			output: "Stdout: " + marker + "\r\n",
			want:   true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := echoedMarker(tc.output, marker); got != tc.want {
				t.Errorf("echoedMarker(...) = %v, want %v\noutput:\n%s", got, tc.want, tc.output)
			}
		})
	}
}
