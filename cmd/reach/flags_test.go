package main

import (
	"flag"
	"io"
	"slices"
	"testing"
)

// The standard library stops parsing flags at the first positional argument, so
// `reach up ssh://host/path --name build` silently ignored --name and made a
// session called "default". The operator finds out much later, when a command
// cannot find the session they thought they had named.

func TestParseFlagsAcceptsFlagsAnywhere(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     []string
		wantName string
		wantPos  []string
	}{
		{"flags first", []string{"--name", "build", "ssh://h/p"}, "build", []string{"ssh://h/p"}},
		{"flags last", []string{"ssh://h/p", "--name", "build"}, "build", []string{"ssh://h/p"}},
		{"flags interspersed", []string{"a", "--name", "build", "b"}, "build", []string{"a", "b"}},
		{"single dash", []string{"ssh://h/p", "-name", "build"}, "build", []string{"ssh://h/p"}},
		{"equals form", []string{"ssh://h/p", "--name=build"}, "build", []string{"ssh://h/p"}},
		{"no flags", []string{"ssh://h/p"}, "default", []string{"ssh://h/p"}},
		{"nothing", nil, "default", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := newFlagSet("test")
			fs.SetOutput(io.Discard)
			name := fs.String("name", "default", "")

			pos, err := parseFlags(fs, tc.args)
			if err != nil {
				t.Fatalf("parseFlags(%q): %v", tc.args, err)
			}
			if *name != tc.wantName {
				t.Errorf("--name = %q, want %q (a flag was silently dropped)", *name, tc.wantName)
			}
			if !slices.Equal(pos, tc.wantPos) {
				t.Errorf("positional = %q, want %q", pos, tc.wantPos)
			}
		})
	}
}

// Everything after `--` belongs to the target, not to reach. Without this,
// `reach exec -- ls -la` would hand -la to reach's own flag parser and fail on
// a flag the *target's* ls understands perfectly well.
func TestParseFlagsTreatsEverythingAfterDashDashAsPositional(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     []string
		wantName string
		wantPos  []string
	}{
		{"target flags are not reach's", []string{"--", "ls", "-la"}, "default", []string{"ls", "-la"}},
		{"reach flags before the separator", []string{"--name", "s", "--", "ls", "-la"}, "s", []string{"ls", "-la"}},
		{"a reach flag name after it belongs to the target",
			[]string{"--", "echo", "--name", "not-reaches"}, "default", []string{"echo", "--name", "not-reaches"}},
		{"empty tail", []string{"--name", "s", "--"}, "s", nil},
		// A second `--` is the target's argument: `git checkout -- file` is a
		// real command an agent will run.
		{"a second separator is data", []string{"--", "git", "checkout", "--", "f"}, "default",
			[]string{"git", "checkout", "--", "f"}},
		{"order is preserved", []string{"a", "--name", "s", "b", "--", "c", "d"}, "s",
			[]string{"a", "b", "c", "d"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := newFlagSet("test")
			fs.SetOutput(io.Discard)
			name := fs.String("name", "default", "")

			pos, err := parseFlags(fs, tc.args)
			if err != nil {
				t.Fatalf("parseFlags(%q): %v", tc.args, err)
			}
			if *name != tc.wantName {
				t.Errorf("--name = %q, want %q", *name, tc.wantName)
			}
			if !slices.Equal(pos, tc.wantPos) {
				t.Errorf("positional = %q, want %q", pos, tc.wantPos)
			}
		})
	}
}

// An unknown flag has to be an error. Accepting it would mean the same silent
// misconfiguration this function exists to prevent, one typo further along.
func TestParseFlagsRejectsUnknownFlags(t *testing.T) {
	for _, args := range [][]string{
		{"--nope"},
		{"ssh://h/p", "--nope"},
		{"--name", "s", "--nope", "x"},
	} {
		fs := newFlagSet("test")
		fs.SetOutput(io.Discard)
		fs.String("name", "default", "")

		if _, err := parseFlags(fs, args); err == nil {
			t.Errorf("parseFlags(%q) accepted an unknown flag", args)
		}
	}
}

// A flag given twice takes its last value, matching what every other CLI does.
func TestParseFlagsLastValueWins(t *testing.T) {
	fs := newFlagSet("test")
	fs.SetOutput(io.Discard)
	name := fs.String("name", "default", "")

	if _, err := parseFlags(fs, []string{"--name", "first", "x", "--name", "second"}); err != nil {
		t.Fatal(err)
	}
	if *name != "second" {
		t.Errorf("--name = %q, want %q", *name, "second")
	}
}

// Boolean flags are the case the interspersing loop is most likely to get
// wrong, because `--flag value` means something different for a bool.
func TestParseFlagsHandlesBooleans(t *testing.T) {
	fs := newFlagSet("test")
	fs.SetOutput(io.Discard)
	clean := fs.Bool("clean", false, "")

	pos, err := parseFlags(fs, []string{"session-name", "--clean"})
	if err != nil {
		t.Fatal(err)
	}
	if !*clean {
		t.Error("--clean after a positional argument was dropped")
	}
	if !slices.Equal(pos, []string{"session-name"}) {
		t.Errorf("positional = %q, want [session-name]", pos)
	}
}

// newFlagSet must not exit the process on a bad flag, or a mistyped flag would
// take the whole command down before its caller could explain anything.
func TestNewFlagSetDoesNotExit(t *testing.T) {
	fs := newFlagSet("test")
	if fs.ErrorHandling() != flag.ContinueOnError {
		t.Errorf("error handling is %v, want ContinueOnError", fs.ErrorHandling())
	}
}
