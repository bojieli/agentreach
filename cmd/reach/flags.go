package main

import (
	"flag"
	"fmt"
	"strings"
)

// parseFlags parses a flag set where flags may appear before, after or between
// positional arguments, and returns the positional arguments.
//
// The standard library stops parsing at the first non-flag argument, so
// `reach up ssh://host/path --name build` would silently ignore --name and
// create a session called "default". A flag that is quietly discarded is worse
// than one that errors: the operator believes they configured something they
// did not, and only finds out when a later command cannot find the session.
//
// Everything after a literal "--" is positional, so `reach exec -- ls -la`
// passes -la to the target rather than to reach.
func parseFlags(fs *flag.FlagSet, args []string) ([]string, error) {
	var tail []string
	for i, a := range args {
		if a == "--" {
			// G602: i is an index into args from ranging over it, so both
			// args[i+1:] and args[:i] are in range by construction. gosec
			// does not follow that through the reassignment of args.
			tail = args[i+1:] //nolint:gosec // i came from range over args
			args = args[:i]   //nolint:gosec // i came from range over args
			break
		}
	}
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			break
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
	return append(positional, tail...), nil
}

// newFlagSet builds a flag set that reports errors without exiting, so callers
// can produce their own message.
func newFlagSet(name string) *flag.FlagSet {
	return flag.NewFlagSet(name, flag.ContinueOnError)
}

// parseHarnessFlags parses the flags reach itself defines for a harness
// launcher and returns everything else — unrecognised flags included — in the
// order it was typed, for the harness to answer for.
//
// `reach build-box claude --dangerously-skip-permissions` is what an operator
// actually wants to type, and demanding `-- --dangerously-skip-permissions`
// for it was a tax on every launch. The separator is easy to forget, and
// forgetting it failed with the flag package's "flag provided but not
// defined" over reach's own usage block, which reads as though reach had
// rejected a Claude Code flag rather than merely declined to forward one.
// reach owns the handful of names it defines here; every other flag belongs to
// the harness.
//
// A literal "--" still works, and is the escape hatch for the collision this
// creates: `reach claude -- --session x` gives --session to Claude Code rather
// than to reach. -h and --help stay reach's, so `reach claude --help`
// describes the launcher instead of starting the agent.
func parseHarnessFlags(fs *flag.FlagSet, args []string) ([]string, error) {
	var mine, theirs []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			theirs = append(theirs, args[i+1:]...)
			break
		}
		// "-" is stdin by convention, and a bare word is the harness's own
		// argument — a prompt, a path, a subcommand.
		if len(a) < 2 || a[0] != '-' {
			theirs = append(theirs, a)
			continue
		}
		name, _, hasValue := strings.Cut(strings.TrimLeft(a, "-"), "=")
		f := fs.Lookup(name)
		if f == nil {
			// An undefined -h or --help is the flag package's own signal to
			// print the usage it just built, which is worth more here than
			// launching the harness with a flag reach could have explained.
			if name != "h" && name != "help" {
				theirs = append(theirs, a)
				continue
			}
			mine = append(mine, a)
			continue
		}
		mine = append(mine, a)
		// A non-boolean flag written without "=" takes the next word with it.
		// Getting this wrong would hand the harness reach's flag value as an
		// argument of its own.
		if !hasValue && !isBoolFlag(f) && i+1 < len(args) {
			i++
			mine = append(mine, args[i])
		}
	}
	if err := fs.Parse(mine); err != nil {
		return nil, err
	}
	return theirs, nil
}

// isBoolFlag reports whether a flag is satisfied by its own presence, which is
// what decides whether the next word is its value or the harness's argument.
func isBoolFlag(f *flag.Flag) bool {
	b, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && b.IsBoolFlag()
}

// newHarnessFlagSet builds the flag set for a harness launcher, with a usage
// block that says what happens to the flags it does not define. Without that
// line the reach flags read as the only ones accepted, which is the belief
// this whole mechanism exists to correct.
func newHarnessFlagSet(name string) *flag.FlagSet {
	fs := newFlagSet(name)
	fs.Usage = func() {
		out := fs.Output()
		_, _ = fmt.Fprintf(out, "Usage: reach [<target>] %s [reach flags] [%s arguments]\n\n"+
			"reach's own flags:\n", name, name)
		fs.PrintDefaults()
		_, _ = fmt.Fprintf(out, "\nEvery other flag and argument is handed to %s unchanged, so\n"+
			"`reach %s --resume` starts %s with --resume. Put `--` first to pass\n"+
			"a flag whose name reach also defines: `reach %s -- --session x`.\n",
			name, name, name, name)
	}
	return fs
}
