package main

import (
	"bytes"
	"testing"
)

func TestCwdWriterExtractsMarker(t *testing.T) {
	cases := []struct {
		name    string
		chunks  []string
		wantOut string
		wantCwd string
	}{
		{
			name:    "marker on its own line",
			chunks:  []string{"warning: something\n__waldo_cwd__/srv/app\n"},
			wantOut: "warning: something\n",
			wantCwd: "/srv/app",
		},
		{
			// The regression case: stderr with no trailing newline, so the
			// marker lands on the same line as the command's own output.
			name:    "marker appended to unterminated line",
			chunks:  []string{"no trailing newline__waldo_cwd__/srv/app\n"},
			wantOut: "no trailing newline",
			wantCwd: "/srv/app",
		},
		{
			name:    "marker split across writes",
			chunks:  []string{"partial output\n__waldo", "_cwd__/deep/path\n"},
			wantOut: "partial output\n",
			wantCwd: "/deep/path",
		},
		{
			name:    "no marker at all",
			chunks:  []string{"just stderr\n"},
			wantOut: "just stderr\n",
			wantCwd: "",
		},
		{
			name:    "marker with no trailing newline",
			chunks:  []string{"err\n__waldo_cwd__/x"},
			wantOut: "err\n",
			wantCwd: "/x",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			w := &cwdCapturingWriter{out: &buf, marker: "__waldo_cwd__"}
			for _, c := range tc.chunks {
				if _, err := w.Write([]byte(c)); err != nil {
					t.Fatal(err)
				}
			}
			got := w.Captured()
			if got != tc.wantCwd {
				t.Errorf("cwd = %q want %q", got, tc.wantCwd)
			}
			if buf.String() != tc.wantOut {
				t.Errorf("passthrough = %q want %q", buf.String(), tc.wantOut)
			}
		})
	}
}

func TestCwdWriterNeverLeaksMarker(t *testing.T) {
	var buf bytes.Buffer
	w := &cwdCapturingWriter{out: &buf, marker: "__waldo_cwd__"}
	_, _ = w.Write([]byte("compiling...__waldo_cwd__/build\n"))
	_ = w.Captured()
	if bytes.Contains(buf.Bytes(), []byte("__waldo_cwd__")) {
		t.Errorf("waldo bookkeeping leaked into the agent's stderr: %q", buf.String())
	}
}
