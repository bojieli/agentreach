package main

import (
	"bufio"
	"net"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// This file answers one question: is this bare word the name of a machine?
//
// It exists because `reach build-box claude` has to decide whether "build-box"
// is a target or a misspelled command, and the two mistakes deserve different
// answers. Guessing "target" for every unrecognised word would turn `reach
// stauts` into a connection attempt against a host that does not exist, with a
// network timeout where an unknown-command message belonged.
//
// The evidence is deliberately local: the operator's ssh configuration, the
// hosts file, and the shape of the word itself. DNS is not consulted, and that
// is on purpose — resolvers that answer for every name are common enough
// (captive portals, ISP typo pages, search domains) that a lookup would make
// every typo a hostname on exactly the networks where the mistake is hardest
// to see. Anything reach cannot recognise this way can still be named
// outright: `reach ssh://whatever/srv/app claude` never consults this file.

// hostPattern is the shape of a hostname or an ssh_config alias. Underscores
// are not legal in DNS but are common in aliases, so they are allowed.
var hostPattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]*[A-Za-z0-9])?$`)

// looksLikeHost reports whether a word could be an ssh destination at all,
// before any lookup is attempted. It accepts an optional user@ prefix.
func looksLikeHost(spec string) bool {
	if spec == "" || strings.ContainsAny(spec, " \t/\\") {
		return false
	}
	return hostPattern.MatchString(hostPart(spec))
}

// hostPart strips a user@ prefix.
func hostPart(spec string) string {
	if _, h, ok := strings.Cut(spec, "@"); ok {
		return h
	}
	return spec
}

// findHost reports whether this word names a machine, and when it does not,
// what reach looked at — so the operator sees which sources were consulted
// rather than a bare refusal.
func findHost(spec string) (found bool, why string) {
	host := hostPart(spec)
	if sshConfigNames(host) {
		return true, ""
	}
	if net.ParseIP(host) != nil {
		return true, ""
	}
	// A dotted name is not a reach command and never will be, so there is
	// nothing to protect it from: hand it to ssh and let ssh say whether the
	// name resolves. Its error names the host; reach's would only guess.
	if strings.Contains(host, ".") {
		return true, ""
	}
	if hostsFileNames(host) {
		return true, ""
	}
	return false, "no Host entry in your ssh configuration names it, and it is not in " +
		hostsFilePath() + ", an address, or a dotted name"
}

// hostsFileNames reports whether the system hosts file names this host.
//
// It is worth reading because a machine on a local network often has an entry
// there and nothing in ssh_config, and reaching one of those is exactly what
// this form is for.
func hostsFileNames(host string) bool {
	f, err := os.Open(hostsFilePath())
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()

	host = strings.ToLower(host)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		// The first field is the address; every field after it is a name for
		// that address.
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		for _, name := range fields[1:] {
			if strings.ToLower(name) == host {
				return true
			}
		}
	}
	return false
}

// sshConfigNames reports whether any Host pattern in the operator's ssh
// configuration matches this name.
//
// A bare `Host *` is ignored on purpose. It matches everything, so it would
// make every typo a host, which is exactly the failure this check exists to
// avoid.
func sshConfigNames(host string) bool {
	seen := map[string]bool{}
	for _, f := range sshConfigFiles() {
		if configNames(f, strings.ToLower(host), seen, 0) {
			return true
		}
	}
	return false
}

// sshConfigFiles are the configuration files reach reads to recognise aliases.
//
// REACH_SSH_CONFIG is honoured because it is already the file reach's own
// connections use; recognising aliases from a different file than the one it
// would connect with would be a lie in the operator's favour.
func sshConfigFiles() []string {
	if p := os.Getenv("REACH_SSH_CONFIG"); p != "" {
		return []string{p}
	}
	var out []string
	if home, err := os.UserHomeDir(); err == nil {
		out = append(out, filepath.Join(home, ".ssh", "config"))
	}
	return append(out, "/etc/ssh/ssh_config")
}

// maxIncludeDepth bounds Include recursion. ssh itself allows nesting; a
// configuration that includes itself must not make reach spin.
const maxIncludeDepth = 8

func configNames(file, host string, seen map[string]bool, depth int) bool {
	if depth > maxIncludeDepth || seen[file] {
		return false
	}
	seen[file] = true
	f, err := os.Open(file)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		keyword, args, ok := configLine(sc.Text())
		if !ok {
			continue
		}
		switch keyword {
		case "host":
			for _, pattern := range args {
				if pattern == "*" || strings.HasPrefix(pattern, "!") {
					continue
				}
				if matched, err := path.Match(strings.ToLower(pattern), host); err == nil && matched {
					return true
				}
			}
		case "include":
			for _, arg := range args {
				for _, inc := range expandInclude(file, arg) {
					if configNames(inc, host, seen, depth+1) {
						return true
					}
				}
			}
		}
	}
	return false
}

// configLine splits one ssh_config line into its keyword and arguments.
// ssh accepts `Key value`, `Key=value` and any amount of whitespace.
func configLine(line string) (keyword string, args []string, ok bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", nil, false
	}
	line = strings.ReplaceAll(line, "=", " ")
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return "", nil, false
	}
	return strings.ToLower(fields[0]), fields[1:], true
}

// expandInclude resolves an Include argument the way ssh does: absolute paths
// as written, ~ against the home directory, and anything else relative to the
// directory of the file doing the including.
func expandInclude(from, arg string) []string {
	switch {
	case strings.HasPrefix(arg, "~"):
		home, err := os.UserHomeDir()
		if err != nil {
			return nil
		}
		arg = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(arg, "~"), "/"))
	case !filepath.IsAbs(arg):
		arg = filepath.Join(filepath.Dir(from), arg)
	}
	matches, err := filepath.Glob(arg)
	if err != nil {
		return nil
	}
	return matches
}
