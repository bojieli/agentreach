package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// TestReplaceProcessFallsBackToAChild covers the path taken when execve is
// unavailable — which is every invocation on Windows, and an occasional one on
// Unix. It was previously untested because two of the three call sites simply
// printed the error and exited, so there was nothing to test.
func TestReplaceProcessFallsBackToAChild(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the helper script needs a POSIX shell")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-harness")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s' \"$REACH_TEST_MARKER\" > \""+dir+"/out\"\nexit 43\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Force the fallback, as an execve-less platform would.
	original := execve
	execve = func(string, []string, []string) error { return errors.New("execve unavailable in this test") }
	t.Cleanup(func() { execve = original })

	code := replaceProcess(context.Background(), script,
		[]string{script, "ignored"}, []string{"REACH_TEST_MARKER=handed-over"})

	if code != 43 {
		t.Errorf("exit status = %d, want 43 — a harness's status must reach the caller unchanged", code)
	}
	got, err := os.ReadFile(filepath.Join(dir, "out"))
	if err != nil {
		t.Fatalf("the child did not run: %v", err)
	}
	if string(got) != "handed-over" {
		t.Errorf("environment reached the child as %q, want %q", got, "handed-over")
	}
}

// TestReplaceProcessReportsAnUnrunnableBinary: if neither execve nor a child
// works, the caller must get a non-zero status rather than a cheerful zero.
func TestReplaceProcessReportsAnUnrunnableBinary(t *testing.T) {
	original := execve
	execve = func(string, []string, []string) error { return errors.New("no execve") }
	t.Cleanup(func() { execve = original })

	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if code := replaceProcess(context.Background(), missing, []string{missing}, nil); code == 0 {
		t.Error("a binary that cannot be run reported success")
	}
	var ee *exec.ExitError
	_ = ee // documents that a missing binary is not an ExitError, which is why the check above is on the status
}
