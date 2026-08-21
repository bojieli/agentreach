package transport

import (
	"strings"
	"testing"
	"time"
)

// The connection reach authenticated at `reach up` is the only one an operator
// was present to complete, so how long it is kept is the difference between a
// slow tool call and a failed one.
func TestControlPersistFromEnv(t *testing.T) {
	for _, tc := range []struct {
		env     string
		want    time.Duration
		forever bool
		bad     bool
	}{
		{env: "", want: defaultControlPersist},
		{env: "yes", forever: true},
		{env: "YES", forever: true},
		{env: "forever", forever: true},
		{env: "30m", want: 30 * time.Minute},
		{env: "45s", want: 45 * time.Second},
		{env: "0", bad: true},
		{env: "-5m", bad: true},
		{env: "later", bad: true},
	} {
		t.Setenv(ControlPersistEnv, tc.env)
		d, forever, err := controlPersistFromEnv()
		if tc.bad {
			if err == nil {
				t.Errorf("%s=%q was accepted", ControlPersistEnv, tc.env)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s=%q: %v", ControlPersistEnv, tc.env, err)
			continue
		}
		if forever != tc.forever || (!forever && d != tc.want) {
			t.Errorf("%s=%q gave (%v, forever=%v), want (%v, forever=%v)",
				ControlPersistEnv, tc.env, d, forever, tc.want, tc.forever)
		}
	}
}

// A value reach cannot understand must not be quietly replaced with the
// default: an operator who asked for five minutes and silently got an hour
// finds out by noticing a connection outliving what they configured.
func TestABadControlPersistIsRefusedAtConstruction(t *testing.T) {
	t.Setenv(ControlPersistEnv, "sometimes")
	if _, err := NewSSH(SSHConfig{Host: "box"}); err == nil {
		t.Fatal("a transport was built with an unreadable connection lifetime")
	} else if !strings.Contains(err.Error(), ControlPersistEnv) {
		t.Errorf("the error does not name the setting to fix: %v", err)
	}
}

func TestControlPersistReachesSSH(t *testing.T) {
	t.Setenv(ControlPersistEnv, "")

	args := newTestSSH(t, SSHConfig{Multiplex: true}).baseArgs()
	if v, _ := argValue(args, "ControlPersist"); v != "3600" {
		t.Errorf("ControlPersist=%q, want 3600 (one hour, in seconds)", v)
	}

	t.Setenv(ControlPersistEnv, "yes")
	forever := newTestSSH(t, SSHConfig{Multiplex: true}).baseArgs()
	if v, _ := argValue(forever, "ControlPersist"); v != "yes" {
		t.Errorf("ControlPersist=%q, want yes", v)
	}
}

// A tool call runs in batch mode and cannot ask for a credential, so ssh's
// "Permission denied" has to be turned into something an operator can act on.
func TestBatchAuthAdvice(t *testing.T) {
	t.Setenv(ControlPersistEnv, "")

	for _, s := range []string{
		"ssh box: command did not complete (exit 255): Permission denied (publickey).",
		"box: Too many authentication failures",
		"no such identity: /home/u/.ssh/id_ed25519: No such file or directory",
	} {
		if !IsAuthFailure(s) {
			t.Errorf("not recognised as an authentication failure: %q", s)
		}
	}
	for _, s := range []string{
		"bash: line 1: rg: command not found",
		"channel 0: open failed: administratively prohibited: open failed",
	} {
		if IsAuthFailure(s) {
			t.Errorf("wrongly recognised as an authentication failure: %q", s)
		}
	}
	// A target-side EACCES — `cp: cannot create regular file: Permission denied`
	// — reads the same way, which is why this is only ever consulted about a
	// transport failure. A command that ran and failed carries its own exit
	// status through the sentinel and never reaches Advise at all.

	batch := newTestSSH(t, SSHConfig{Multiplex: true, BatchMode: true})
	advice := batch.Advise("ssh box: Permission denied (publickey).")
	if !strings.Contains(advice, "reach up") || !strings.Contains(advice, ControlPersistEnv) {
		t.Errorf("the advice does not say what to do: %q", advice)
	}

	// `reach up` can prompt, so an authentication failure there is exactly what
	// it looks like and needs no explanation about batch mode.
	interactive := newTestSSH(t, SSHConfig{Multiplex: true, BatchMode: false})
	if got := interactive.Advise("ssh box: Permission denied (publickey)."); got != "" {
		t.Errorf("an interactive connection was given batch-mode advice: %q", got)
	}
	if got := batch.Advise("bash: line 1: make: command not found"); got != "" {
		t.Errorf("a command failure was given authentication advice: %q", got)
	}
}
