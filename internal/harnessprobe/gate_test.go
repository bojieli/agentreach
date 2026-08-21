package harnessprobe

import (
	"strings"
	"testing"
	"time"
)

// The gate is where the guard's policy lives, and the policy is asymmetric on
// purpose: only a *measured* bypass refuses the launch. These tests pin the
// whole matrix, because every cell of it is a decision about whether an
// agent's commands run on the target or on the operator's own machine.

func TestGateFromCache(t *testing.T) {
	cases := []struct {
		name      string
		cached    *Entry
		wantAllow bool
		wantProbe bool
		wantMsg   string
	}{
		{
			name:      "no verdict runs the probe",
			cached:    nil,
			wantProbe: true,
		},
		{
			name:      "verified ok launches silently",
			cached:    &Entry{Verdict: VerdictOK, When: time.Now()},
			wantAllow: true,
			wantMsg:   "",
		},
		{
			name:      "bypassed refuses",
			cached:    &Entry{Verdict: VerdictBypassed, When: time.Now(), Detail: "ran locally"},
			wantAllow: false,
			wantMsg:   "refusing to launch",
		},
		{
			name:      "an unrecognised verdict re-probes",
			cached:    &Entry{Verdict: "from-a-future-reach", When: time.Now()},
			wantProbe: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := Gate("codex", "0.148.0", tc.cached)
			if d.Allow != tc.wantAllow || d.RunProbe != tc.wantProbe {
				t.Fatalf("Gate = %+v, want allow=%v probe=%v", d, tc.wantAllow, tc.wantProbe)
			}
			if tc.wantMsg == "" && d.Message != "" {
				t.Fatalf("expected silence, got %q", d.Message)
			}
			if tc.wantMsg != "" && !strings.Contains(d.Message, tc.wantMsg) {
				t.Fatalf("message %q does not contain %q", d.Message, tc.wantMsg)
			}
		})
	}
}

func TestGateFromProbe(t *testing.T) {
	cases := []struct {
		name      string
		result    Result
		wantAllow bool
		wantMsg   string
	}{
		{
			name:      "ok launches with a one-line note",
			result:    Result{Verdict: VerdictOK, Detail: "on target"},
			wantAllow: true,
			wantMsg:   "seam verified",
		},
		{
			name:      "bypassed refuses",
			result:    Result{Verdict: VerdictBypassed, Detail: "ran locally"},
			wantAllow: false,
			wantMsg:   "refusing to launch",
		},
		{
			name:      "probe error fails open with a loud warning",
			result:    Result{Verdict: VerdictError, Detail: "timed out"},
			wantAllow: true,
			wantMsg:   "could not verify",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := GateFromProbe("codex", "0.148.0", tc.result)
			if d.Allow != tc.wantAllow {
				t.Fatalf("GateFromProbe allow = %v, want %v", d.Allow, tc.wantAllow)
			}
			if !strings.Contains(d.Message, tc.wantMsg) {
				t.Fatalf("message %q does not contain %q", d.Message, tc.wantMsg)
			}
		})
	}
}

// The refusal is the message an operator reads at the worst moment; pin the
// facts it must carry: that commands would run locally, how reach knows, and
// every documented way forward.
func TestRefusalMessageCarriesTheFacts(t *testing.T) {
	d := Gate("codex", "0.148.0", &Entry{Verdict: VerdictBypassed, When: time.Now(), Detail: "ran locally"})
	for _, want := range []string{
		"0.148.0",
		"exec-server",
		"THIS machine",
		"reach harness verify codex",
		"--force",
		"ran locally",
	} {
		if !strings.Contains(d.Message, want) {
			t.Errorf("codex refusal message does not mention %q:\n%s", want, d.Message)
		}
	}

	// Kimi's refusal carries one fact codex's does not: even a working shell
	// seam leaves its native file tools acting locally. An operator reading
	// only the refusal must not walk into the subtler version of the trap.
	d = Gate("kimi", "0.37.2", &Entry{Verdict: VerdictBypassed, When: time.Now(), Detail: "ran locally"})
	for _, want := range []string{
		"0.37.2",
		"absolute path",
		"THIS machine",
		"on the target itself",
		"reach harness verify kimi",
		"--force",
		"read_file, write_file and multi_edit",
		"LOCAL filesystem",
	} {
		if !strings.Contains(d.Message, want) {
			t.Errorf("kimi refusal message does not mention %q:\n%s", want, d.Message)
		}
	}
}
