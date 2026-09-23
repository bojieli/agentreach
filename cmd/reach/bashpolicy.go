package main

import (
	"path"
	"regexp"
	"strings"
)

// The Bash tool is the seam that always works: in every reach mode the command
// runs on the target. Claude Code does not know that. Before running a command
// it resolves the paths inside it against the *local* filesystem and the
// session's local working directories, and refuses what falls outside them. A
// target path is always outside them, so a session that is working exactly as
// designed gets
//
//	cat in '/srv/app/main.go' was blocked. For security, Claude Code may only
//	concatenate files from the allowed working directories for this session
//
// for a read that would have succeeded on the target, and
//
//	Output redirection to '/srv/app/x' was blocked.
//
// for a write — with no prompt to approve, because the check is a refusal
// rather than a question. A command that begins with `cd` into a directory
// this machine does not have is the third shape: the harness cannot resolve
// what the rest of the command reads, so it asks, every time.
//
// reach knows the thing that would settle all three: the command is not going
// to run here at all, so hookBash allows it. This file matters only in plan
// mode, where the operator has asked for nothing to change: there reach still
// vouches for commands that read files and change nothing, and leaves the rest
// to Claude Code.

// readOnlyShellCommands are the commands reach will vouch for.
//
// Membership is not "this command is harmless" but "this command reads and
// cannot write". Anything that can create, modify or delete a file — or run
// another command that could — stays out, however ordinary it is. The override
// exists to correct a local path check, not to shorten the operator's
// permission list: every name added here is a confirmation prompt they no
// longer get.
var readOnlyShellCommands = map[string]bool{
	// Reading a file.
	"cat": true, "head": true, "tail": true, "sed": true, "nl": true,
	"od": true, "xxd": true, "strings": true,
	// Searching files.
	"grep": true, "egrep": true, "fgrep": true, "rg": true,
	// Listing and describing them.
	"ls": true, "find": true, "stat": true, "file": true, "wc": true,
	"du": true, "tree": true, "readlink": true, "realpath": true,
	"basename": true, "dirname": true, "pwd": true, "cd": true,
	// Comparing and fingerprinting them.
	"diff": true, "cmp": true, "md5sum": true, "sha1sum": true,
	"sha256sum": true, "shasum": true, "cksum": true,
}

// findActions run or delete instead of describing, which is the whole of the
// difference between `find` as a search and `find` as a command.
var findActions = map[string]bool{
	"-exec": true, "-execdir": true, "-ok": true, "-okdir": true,
	"-delete": true, "-fprint": true, "-fprintf": true, "-fls": true,
}

// sedPrintScript is the range-printing form reach's own exec-mode guidance
// recommends: 1,50p — $p — 40p — p.
var sedPrintScript = regexp.MustCompile(`^((\d+|\$)(,(\d+|\$))?)?p$`)

// readOnlyBashCommand reports whether every part of a shell command only reads.
//
// It answers false for anything it cannot read with certainty. That is the
// right way to be wrong here: a false negative costs a permission prompt, and
// a false positive approves a command reach did not actually understand.
func readOnlyBashCommand(cmd string) bool {
	segments, ok := splitShellSegments(cmd)
	if !ok {
		return false
	}
	for _, segment := range segments {
		words, ok := shellWords(segment)
		if !ok || len(words) == 0 {
			return false
		}
		if !readOnlySegment(words) {
			return false
		}
	}
	return true
}

// readOnlySegment judges one simple command out of a pipeline.
func readOnlySegment(words []string) bool {
	name := words[0]
	// An assignment prefix (FOO=bar cat x) puts the command somewhere other
	// than the first word, and changes the environment it runs in.
	if strings.Contains(name, "=") {
		return false
	}
	// A path-qualified name is the same command: /bin/cat reads what cat reads.
	// `path`, not `filepath` — this string is the target's, and the target is
	// POSIX whatever reach is running on.
	name = path.Base(name)
	if !readOnlyShellCommands[name] {
		return false
	}

	args := words[1:]
	switch name {
	case "sed":
		return readOnlySed(args)
	case "find":
		for _, a := range args {
			if findActions[a] {
				return false
			}
		}
	case "tail":
		// -f never returns. Waving it through would turn the agent's turn into
		// a hang, which is worse than the prompt it saves.
		if hasAnyFlag(args, "-f", "-F", "--follow", "--retry") {
			return false
		}
	case "grep", "egrep", "fgrep", "rg":
		// ripgrep's --pre runs a preprocessor of the caller's choosing.
		for _, a := range args {
			if strings.HasPrefix(a, "--pre") {
				return false
			}
		}
	}
	return true
}

// readOnlySed accepts the range-printing form and a single substitution, and
// nothing else.
//
// sed writes files two ways: -i, and a `w` command inside the script (GNU sed
// also runs commands, with `e`). Only a script reach can read in full rules
// those out, so anything cleverer than a line range or one s/// becomes a
// question rather than a guess.
func readOnlySed(args []string) bool {
	quiet, script := false, ""
	for _, a := range args {
		if strings.HasPrefix(a, "-") && a != "-" {
			switch a {
			case "-n", "--quiet", "--silent":
				quiet = true
			case "-E", "-r", "--regexp-extended":
			default:
				return false
			}
			continue
		}
		if script == "" {
			script = a
		}
	}
	if quiet && sedPrintScript.MatchString(script) {
		return true
	}
	return sedSubstitution(script)
}

// sedSubstitution reports whether a script is exactly one s command whose
// flags only change what it prints: s/re/repl/ with g, p, i, I, m, M or a
// count. The w and e flags, and anything after the flags, are refused.
//
// The delimiter is whatever follows the s, and a backslash escapes it. A
// script that uses the delimiter somewhere this scan does not expect ends
// early and fails the flag check, which is the safe way to misread it.
func sedSubstitution(script string) bool {
	if len(script) < 4 || script[0] != 's' {
		return false
	}
	delim := script[1]
	if delim == '\\' || delim == '\n' || delim == ' ' {
		return false
	}
	i, parts := 2, 0
	for ; i < len(script) && parts < 2; i++ {
		switch script[i] {
		case '\\':
			i++
		case '\n':
			return false
		case delim:
			parts++
		}
	}
	if parts < 2 {
		return false
	}
	return sedSubstitutionFlags.MatchString(script[i:])
}

var sedSubstitutionFlags = regexp.MustCompile(`^[gpiImM0-9]*$`)

func hasAnyFlag(args []string, flags ...string) bool {
	for _, a := range args {
		for _, f := range flags {
			if a == f {
				return true
			}
		}
	}
	return false
}

// splitShellSegments splits a command into the simple commands a pipeline is
// made of, and reports whether the split can be trusted.
//
// Only the operators that chain commands survive: |, ||, &&, ;. A redirect
// goes through only when it discards output or joins two streams — >/dev/null,
// 2>/dev/null, &>/dev/null, 2>&1 — none of which can write a file. Any other
// redirect, a background &, a subshell, a substitution or a newline returns
// false: each of them either writes a file or runs something the words do not
// name.
func splitShellSegments(cmd string) ([]string, bool) {
	var segments []string
	var cur strings.Builder
	for i := 0; i < len(cmd); i++ {
		switch c := cmd[i]; c {
		case '\'':
			j := strings.IndexByte(cmd[i+1:], '\'')
			if j < 0 {
				return nil, false
			}
			cur.WriteString(cmd[i : i+j+2])
			i += j + 1
		case '"':
			j := closingQuote(cmd, i)
			if j < 0 {
				return nil, false
			}
			cur.WriteString(cmd[i : j+1])
			i = j
		case '|':
			if i+1 < len(cmd) && cmd[i+1] == '|' {
				i++
			}
			segments = append(segments, cur.String())
			cur.Reset()
		case '&':
			// &>/dev/null discards both streams.
			if i+1 < len(cmd) && cmd[i+1] == '>' {
				n, ok := discardRedirect(cmd[i+2:])
				if !ok {
					return nil, false
				}
				i += 1 + n
				cur.WriteByte(' ')
				continue
			}
			// && chains; a lone & backgrounds the command, which leaves it
			// running after the decision that approved it.
			if i+1 >= len(cmd) || cmd[i+1] != '&' {
				return nil, false
			}
			i++
			segments = append(segments, cur.String())
			cur.Reset()
		case ';':
			segments = append(segments, cur.String())
			cur.Reset()
		case '>':
			n, ok := discardRedirect(cmd[i+1:])
			if !ok {
				return nil, false
			}
			// The fd number in 2>/dev/null belongs to the operator, not to the
			// command's arguments — but only when it stands alone: in
			// file2>/dev/null the shell reads file2 as a word.
			prev := cur.String()
			fd := strings.TrimRight(prev, "0123456789")
			if fd != prev && (fd == "" || strings.HasSuffix(fd, " ") || strings.HasSuffix(fd, "\t")) {
				cur.Reset()
				cur.WriteString(fd)
			}
			i += n
			cur.WriteByte(' ')
		case '\n', '<', '(', ')', '{', '}', '`':
			return nil, false
		default:
			cur.WriteByte(c)
		}
	}
	return append(segments, cur.String()), true
}

// discardRedirect reads what follows a > and reports how many bytes of it
// belong to a redirect that cannot write a file: an optional second > for
// append, then /dev/null or &N. Anything else — a real file, >&- , a target
// hidden in an expansion — is refused.
func discardRedirect(rest string) (int, bool) {
	n := 0
	if strings.HasPrefix(rest, ">") {
		n++
	}
	for n < len(rest) && (rest[n] == ' ' || rest[n] == '\t') {
		n++
	}
	switch {
	case strings.HasPrefix(rest[n:], "/dev/null"):
		n += len("/dev/null")
	case strings.HasPrefix(rest[n:], "&"):
		n++
		start := n
		for n < len(rest) && rest[n] >= '0' && rest[n] <= '9' {
			n++
		}
		if n == start {
			return 0, false
		}
	default:
		return 0, false
	}
	// The target must end at a word boundary: /dev/nullx is a file.
	if n < len(rest) && !strings.ContainsRune(" \t|&;", rune(rest[n])) {
		return 0, false
	}
	return n, true
}

// shellWords splits one segment into words the way a POSIX shell would, and
// reports whether the result is what the shell will actually run.
//
// An expansion or a substitution returns false: the word reach would inspect
// is not the word that gets executed.
func shellWords(segment string) ([]string, bool) {
	var words []string
	var cur strings.Builder
	started := false
	for i := 0; i < len(segment); i++ {
		switch c := segment[i]; c {
		case ' ', '\t':
			if started {
				words = append(words, cur.String())
				cur.Reset()
				started = false
			}
		case '\'':
			j := strings.IndexByte(segment[i+1:], '\'')
			if j < 0 {
				return nil, false
			}
			cur.WriteString(segment[i+1 : i+1+j])
			started = true
			i += j + 1
		case '"':
			j := closingQuote(segment, i)
			if j < 0 {
				return nil, false
			}
			inner := segment[i+1 : j]
			if strings.ContainsAny(inner, "$`") {
				return nil, false
			}
			cur.WriteString(strings.ReplaceAll(inner, `\"`, `"`))
			started = true
			i = j
		case '\\':
			if i+1 >= len(segment) {
				return nil, false
			}
			i++
			cur.WriteByte(segment[i])
			started = true
		case '$', '`':
			return nil, false
		default:
			cur.WriteByte(c)
			started = true
		}
	}
	if started {
		words = append(words, cur.String())
	}
	return words, true
}

// closingQuote returns the index of the double quote closing the one at open,
// or -1 when the string ends first.
func closingQuote(s string, open int) int {
	for i := open + 1; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
			continue
		}
		if s[i] == '"' {
			return i
		}
	}
	return -1
}
