package transport

import (
	"context"
	"strings"
	"testing"

	"github.com/bojieli/waldo/internal/waldo"
)

// TestExitIsNotMistakenForTransportFailure guards the subshell decision in
// wrapWithSentinel. Agents write `exit N` routinely; if that killed the
// wrapper, waldo would report a transport failure for an ordinary command.
func TestExitIsNotMistakenForTransportFailure(t *testing.T) {
	tr := NewLocal()
	for _, code := range []int{0, 1, 3, 66, 127, 255} {
		res, err := tr.Run(context.Background(), waldo.ExecRequest{
			Command: "echo before; exit " + itoa(code),
		})
		if err != nil {
			t.Fatalf("exit %d reported as transport failure: %v", code, err)
		}
		if res.Code != code {
			t.Errorf("exit %d: got code %d", code, res.Code)
		}
		if !strings.Contains(string(res.Stdout), "before") {
			t.Errorf("exit %d: lost stdout produced before exit", code)
		}
	}
}

func TestStdoutIsByteExact(t *testing.T) {
	tr := NewLocal()
	for _, tc := range []struct{ cmd, want string }{
		{"printf 'no newline'", "no newline"},
		{"printf 'with newline\\n'", "with newline\n"},
		{"printf ''", ""},
		{"printf 'two\\n\\n'", "two\n\n"},
	} {
		res, err := tr.Run(context.Background(), waldo.ExecRequest{Command: tc.cmd})
		if err != nil {
			t.Fatalf("%s: %v", tc.cmd, err)
		}
		if string(res.Stdout) != tc.want {
			t.Errorf("%s: got %q want %q", tc.cmd, res.Stdout, tc.want)
		}
	}
}

// TestOutputCapPreservesHeadTailAndExitCode is the regression test for a bug
// where capping stdout discarded the trailing status marker, making every
// high-output command look like a transport failure.
func TestOutputCapPreservesHeadTailAndExitCode(t *testing.T) {
	tr := NewLocal()
	const cap = 4096
	res, err := tr.Run(context.Background(), waldo.ExecRequest{
		Command:   "echo FIRST_LINE; yes abcdefghij | head -n 200000; echo LAST_LINE; exit 7",
		MaxOutput: cap,
	})
	if err != nil {
		t.Fatalf("flooding command reported as transport failure: %v", err)
	}
	// The cap bounds retained content; the truncation notice is metadata and
	// may push the total slightly past it.
	if len(res.Stdout) > cap+256 {
		t.Errorf("cap not enforced: %d bytes (cap %d)", len(res.Stdout), cap)
	}
	if !res.Truncated {
		t.Error("truncation not reported")
	}
	if res.Code != 7 {
		t.Errorf("exit code lost to truncation: got %d want 7", res.Code)
	}
	out := string(res.Stdout)
	if !strings.Contains(out, "FIRST_LINE") {
		t.Error("head of output not preserved")
	}
	// The end is where failures live; losing it is the expensive mistake.
	if !strings.Contains(out, "LAST_LINE") {
		t.Error("tail of output not preserved")
	}
	if !strings.Contains(out, "truncated") {
		t.Error("no truncation notice between head and tail")
	}
}

func TestShellQuoteSurvivesHostileInput(t *testing.T) {
	tr := NewLocal()
	for _, s := range []string{
		"plain", "with space", "it's", `"double"`, "$(whoami)", "`id`",
		"back\\slash", "semi;colon", "pipe|char", "new\nline", "{brace}", "*glob*",
	} {
		res, err := tr.Run(context.Background(), waldo.ExecRequest{
			Command: "printf '%s' " + ShellQuote(s),
		})
		if err != nil {
			t.Fatalf("%q: %v", s, err)
		}
		if string(res.Stdout) != s {
			t.Errorf("quote round trip: got %q want %q", res.Stdout, s)
		}
	}
}

func TestBuildCommandChainsDirectorySafely(t *testing.T) {
	got := BuildCommand(waldo.ExecRequest{Command: "ls", Dir: "/tmp/a b"})
	if !strings.HasPrefix(got, "cd '/tmp/a b' && ") {
		t.Errorf("directory not chained with &&: %q", got)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
