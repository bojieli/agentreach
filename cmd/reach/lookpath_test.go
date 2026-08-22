package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeHarnessBin writes an executable named `name` into dir.
func fakeHarnessBin(t *testing.T, dir, name string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		name += ".bat"
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// A relative PATH entry is an ordinary thing for a person to have. bash runs
// what it finds there; reach has to agree, or it reports a binary the operator
// can launch by name as "not installed".
func TestLookHarnessPathAcceptsARelativePathEntry(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	fakeHarnessBin(t, bin, "faketool")

	// Run from root so "bin" is a meaningful relative entry.
	t.Chdir(root)
	t.Setenv("PATH", "bin")

	got, err := lookHarnessPath("faketool")
	if err != nil {
		t.Fatalf("lookHarnessPath: %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("path %q is relative; it must be pinned before any chdir", got)
	}
	if !strings.HasPrefix(filepath.Base(got), "faketool") {
		t.Errorf("resolved to the wrong file: %q", got)
	}
}

// The failure that made the obvious fix useless: hitting the refusal and
// appending an absolute entry changed nothing, because the relative entry is
// earlier and LookPath stops at the first match.
func TestLookHarnessPathWithARelativeEntryAheadOfAnAbsoluteOne(t *testing.T) {
	root := t.TempDir()
	rel := filepath.Join(root, "relbin")
	abs := filepath.Join(root, "absbin")
	for _, d := range []string{rel, abs} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		fakeHarnessBin(t, d, "faketool")
	}

	t.Chdir(root)
	t.Setenv("PATH", "relbin"+string(os.PathListSeparator)+abs)

	got, err := lookHarnessPath("faketool")
	if err != nil {
		t.Fatalf("lookHarnessPath: %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("path %q is relative", got)
	}
	// bash would take the first match; so does reach. What matters is that it
	// resolves rather than refusing.
	if !strings.HasPrefix(got, rel) {
		t.Errorf("expected the first PATH entry to win, got %q", got)
	}
}

// A genuinely absent binary must still be an error — the refusal being wrong
// for relative entries does not make "not installed" wrong in general.
func TestLookHarnessPathStillFailsWhenAbsent(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if p, err := lookHarnessPath("definitely-not-a-real-harness"); err == nil {
		t.Errorf("expected an error, got %q", p)
	}
}
