package main

import "flag"

// parseFlags parses a flag set where flags may appear before, after or between
// positional arguments, and returns the positional arguments.
//
// The standard library stops parsing at the first non-flag argument, so
// `waldo up ssh://host/path --name build` would silently ignore --name and
// create a session called "default". A flag that is quietly discarded is worse
// than one that errors: the operator believes they configured something they
// did not, and only finds out when a later command cannot find the session.
//
// Everything after a literal "--" is positional, so `waldo exec -- ls -la`
// passes -la to the target rather than to waldo.
func parseFlags(fs *flag.FlagSet, args []string) ([]string, error) {
	var tail []string
	for i, a := range args {
		if a == "--" {
			tail = args[i+1:]
			args = args[:i]
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
