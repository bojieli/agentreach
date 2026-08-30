package main

import (
	"errors"
	"flag"
	"go/ast"
	"go/parser"
	"go/token"
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

// A harness launcher is the one place an unknown flag must NOT be an error.
// `reach build-box claude --dangerously-skip-permissions` is the command an
// operator wants to type; before this, it died on reach's own flag parser with
// "flag provided but not defined: -dangerously-skip-permissions", which reads
// as reach rejecting a Claude Code flag rather than declining to forward it.
func TestParseHarnessFlagsForwardsWhatItDoesNotOwn(t *testing.T) {
	for _, tc := range []struct {
		name        string
		args        []string
		wantSession string
		wantForce   bool
		wantTheirs  []string
	}{
		{"a flag reach never heard of", []string{"--dangerously-skip-permissions"},
			"", false, []string{"--dangerously-skip-permissions"}},
		{"reach's flag first, then the harness's",
			[]string{"--session", "build", "--resume"}, "build", false, []string{"--resume"}},
		{"the harness's flag first, then reach's",
			[]string{"--resume", "--session", "build"}, "build", false, []string{"--resume"}},
		{"a forwarded flag with its own value keeps them adjacent and in order",
			[]string{"--model", "sonnet", "-p", "fix it"}, "", false,
			[]string{"--model", "sonnet", "-p", "fix it"}},
		{"reach's string flag takes the next word, which is not the harness's",
			[]string{"--session", "build", "prompt"}, "build", false, []string{"prompt"}},
		{"reach's bool flag does not swallow the next word",
			[]string{"--force", "prompt"}, "", true, []string{"prompt"}},
		{"equals form on both sides",
			[]string{"--session=build", "--model=sonnet"}, "build", false, []string{"--model=sonnet"}},
		{"single dash is reach's spelling too",
			[]string{"-session", "build", "-resume"}, "build", false, []string{"-resume"}},
		{"a bare dash is stdin, not a flag", []string{"-"}, "", false, []string{"-"}},
		{"positionals and forwarded flags keep the order they were typed",
			[]string{"a", "--resume", "--session", "build", "b"}, "build", false,
			[]string{"a", "--resume", "b"}},
		{"nothing at all", nil, "", false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := newFlagSet("test")
			fs.SetOutput(io.Discard)
			session := fs.String("session", "", "")
			force := fs.Bool("force", false, "")

			theirs, err := parseHarnessFlags(fs, tc.args)
			if err != nil {
				t.Fatalf("parseHarnessFlags(%q): %v", tc.args, err)
			}
			if *session != tc.wantSession {
				t.Errorf("--session = %q, want %q", *session, tc.wantSession)
			}
			if *force != tc.wantForce {
				t.Errorf("--force = %v, want %v", *force, tc.wantForce)
			}
			if !slices.Equal(theirs, tc.wantTheirs) {
				t.Errorf("forwarded = %q, want %q", theirs, tc.wantTheirs)
			}
		})
	}
}

// Forwarding by default creates one collision — a harness flag that reach also
// defines — and `--` is the escape hatch for it. It has to keep working, or
// there is no way to give a harness its own --session.
func TestParseHarnessFlagsDashDashGivesEverythingToTheHarness(t *testing.T) {
	fs := newFlagSet("test")
	fs.SetOutput(io.Discard)
	session := fs.String("session", "", "")

	theirs, err := parseHarnessFlags(fs, []string{"--session", "reaches", "--", "--session", "theirs", "-h"})
	if err != nil {
		t.Fatal(err)
	}
	if *session != "reaches" {
		t.Errorf("--session = %q, want %q — reach's flag before the separator is still reach's", *session, "reaches")
	}
	if want := []string{"--session", "theirs", "-h"}; !slices.Equal(theirs, want) {
		t.Errorf("forwarded = %q, want %q", theirs, want)
	}
}

// --help describes the launcher. Forwarding it would start the agent instead,
// and the operator asking what `reach claude` accepts would never find out.
func TestParseHarnessFlagsKeepsHelp(t *testing.T) {
	for _, arg := range []string{"-h", "--help", "-help"} {
		fs := newFlagSet("test")
		fs.SetOutput(io.Discard)
		fs.String("session", "", "")

		_, err := parseHarnessFlags(fs, []string{arg})
		if !errors.Is(err, flag.ErrHelp) {
			t.Errorf("parseHarnessFlags(%q) = %v, want flag.ErrHelp", arg, err)
		}
	}
}

// Forwarding must not extend to reach's own flags: a value it needs and did
// not get is still an error, not something to hand the harness.
func TestParseHarnessFlagsStillChecksItsOwn(t *testing.T) {
	fs := newFlagSet("test")
	fs.SetOutput(io.Discard)
	fs.String("session", "", "")

	if _, err := parseHarnessFlags(fs, []string{"--resume", "--session"}); err == nil {
		t.Error("a reach flag missing its value was accepted")
	}
}

// Every harness launcher must forward, not reject. A new one added with
// parseFlags would compile, pass its own tests, and refuse the harness's flags
// — which is exactly the bug this function was written to remove.
func TestHarnessLaunchersForwardUnknownFlags(t *testing.T) {
	launchers := map[string]string{
		"cmdClaude": "claude.go", "cmdCodex": "harness.go", "cmdKimi": "harness.go",
		"cmdGoose": "harness.go", "cmdGemini": "harness.go",
		"cmdCrush": "crush.go", "cmdGrok": "grok.go",
	}
	seen := map[string]bool{}
	for _, path := range []string{"claude.go", "harness.go", "crush.go", "grok.go"} {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok || launchers[fn.Name.Name] != path {
				return true
			}
			seen[fn.Name.Name] = true
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "parseFlags" {
					t.Errorf("%s parses with parseFlags: it will reject the harness's own flags "+
						"instead of forwarding them. Use parseHarnessFlags", fn.Name.Name)
				}
				return true
			})
			return false
		})
	}
	for name := range launchers {
		if !seen[name] {
			t.Errorf("%s was not found where this test looks for it; the check no longer covers it", name)
		}
	}
}
