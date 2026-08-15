package main

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// The subcommand is checked before the flags are parsed. Parsing first meant
// `waldo fs search --root /srv` — a plausible guess, since the operation is
// called Search throughout waldo's own code — answered "flag provided but not
// defined: -root", which sends the operator looking for the right flag when the
// problem was the subcommand.

func TestFSSubcommandsAreAccepted(t *testing.T) {
	for _, sub := range fsSubcommands {
		if err := checkFSSubcommand(sub); err != nil {
			t.Errorf("checkFSSubcommand(%q) = %v", sub, err)
		}
	}
}

func TestFSAliasesNameTheRightSubcommand(t *testing.T) {
	for alias, want := range fsAliases {
		err := checkFSSubcommand(alias)
		if err == nil {
			t.Errorf("%q was accepted as a subcommand; it is only a hint", alias)
			continue
		}
		if !strings.Contains(err.Error(), "waldo fs "+want) {
			t.Errorf("checkFSSubcommand(%q) does not point at %q: %v", alias, want, err)
		}
	}
}

// An alias that points at a subcommand which does not exist is worse than no
// alias: it sends the operator to a second command that also fails. `mv` was
// exactly that until `waldo fs mv` existed.
func TestFSAliasesPointAtRealSubcommands(t *testing.T) {
	for alias, want := range fsAliases {
		if !slices.Contains(fsSubcommands, want) {
			t.Errorf("alias %q points at %q, which is not a subcommand", alias, want)
		}
		if slices.Contains(fsSubcommands, alias) {
			t.Errorf("%q is both a subcommand and an alias", alias)
		}
	}
}

// The order is the point. This is the exact invocation that misdiagnosed
// itself: a wrong subcommand carrying a flag that only makes sense for it.
// With the check after parseFlags, waldo answered "flag provided but not
// defined: -root" and said nothing about the subcommand being wrong.
func TestFSChecksTheSubcommandBeforeTheFlags(t *testing.T) {
	tempHome(t)
	quiet(t)

	for _, args := range [][]string{
		{"search", "pattern", "--root", "/srv"},
		{"bogus", "--nonsense", "x"},
		{"find", "--depth", "3", "x"},
	} {
		err := cmdFS(context.Background(), args)
		if err == nil {
			t.Errorf("cmdFS(%q) succeeded", args)
			continue
		}
		if strings.Contains(err.Error(), "flag provided but not defined") {
			t.Errorf("cmdFS(%q) blamed a flag when the subcommand was wrong: %v", args, err)
		}
	}
}

func TestFSRejectsUnknownSubcommands(t *testing.T) {
	for _, sub := range []string{"bogus", "", "READ", "grep "} {
		err := checkFSSubcommand(sub)
		if err == nil {
			t.Errorf("checkFSSubcommand(%q) accepted it", sub)
			continue
		}
		if !strings.Contains(err.Error(), "unknown fs subcommand") {
			t.Errorf("checkFSSubcommand(%q) = %v, want an unknown-subcommand error", sub, err)
		}
	}
}

// Every subcommand the usage text advertises has to exist, and every one that
// exists has to be advertised. A subcommand nobody can discover is not shipped,
// and one in the help that is not implemented is a promise the tool breaks.
func TestFSUsageMatchesTheSubcommands(t *testing.T) {
	for _, sub := range fsSubcommands {
		if !strings.Contains(fsUsage, "\n  "+sub+" ") {
			t.Errorf("subcommand %q is not in the usage text", sub)
		}
	}
	for _, line := range strings.Split(fsUsage, "\n") {
		if !strings.HasPrefix(line, "  ") {
			continue
		}
		name, _, found := strings.Cut(strings.TrimSpace(line), " ")
		if !found || name == "" {
			continue
		}
		if !slices.Contains(fsSubcommands, name) {
			t.Errorf("the usage text advertises %q, which is not a subcommand", name)
		}
	}
}
