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
// to run here at all. This file decides how far that knowledge goes. It
// vouches for commands whose only problem is the remote path — the ones that
// read files and change nothing — and it recognises, without vouching for
// them, the commands that would be refused outright, so the operator gets a
// question instead of a wall. Everything else keeps whatever policy the
// operator configured: reach's argument covers only the mistake it can prove.

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

// fileMutatingCommands are commands reach recognises as touching files without
// vouching for them.
//
// They are here only so that a write to the target becomes a question the
// operator can answer rather than a refusal they cannot. Nothing in this map
// is ever allowed by reach.
var fileMutatingCommands = map[string]bool{
	"tee": true, "cp": true, "mv": true, "rm": true, "mkdir": true,
	"rmdir": true, "touch": true, "ln": true, "chmod": true, "chown": true,
	"truncate": true, "install": true, "patch": true, "dd": true,
	"tar": true, "unzip": true, "gzip": true, "gunzip": true,
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

// readOnlySed accepts the range-printing form and nothing else.
//
// sed writes files two ways: -i, and a `w` command inside the script. Only a
// script reach can read in full rules the second one out, so anything cleverer
// than a line range becomes a question rather than a guess.
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
	return quiet && sedPrintScript.MatchString(script)
}

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
// Only the operators that chain commands survive: |, ||, &&. A redirect, a
// background &, a subshell, a substitution or a newline all return false —
// each of them either writes a file or runs something the words do not name.
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
			// && chains; a lone & backgrounds the command, which leaves it
			// running after the decision that approved it.
			if i+1 >= len(cmd) || cmd[i+1] != '&' {
				return nil, false
			}
			i++
			segments = append(segments, cur.String())
			cur.Reset()
		case ';', '\n', '<', '>', '(', ')', '{', '}', '`':
			return nil, false
		default:
			cur.WriteByte(c)
		}
	}
	return append(segments, cur.String()), true
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

// bashPathRefusedLocally returns the first absolute path in a command that the
// harness's local check would refuse, and whether that check applies at all.
//
// It exists to tell two failures apart. `make -C /srv/app` does not read or
// write a file as far as that check is concerned, so reach leaves it alone and
// the operator's own permission rules go on deciding it — silently forcing a
// prompt there would take away an allowance they configured. `cat > /srv/app/x`
// does: locally it is refused outright, with no prompt to approve, so reach
// turns the refusal into a question.
//
// The scan is loose where readOnlyBashCommand is strict, and deliberately so.
// Everything it decides costs at most one permission prompt: a false positive
// is a question the operator can answer, and a false negative is only the
// refusal they already get today.
func bashPathRefusedLocally(cmd, cwd string) (string, bool) {
	// Without the harness's working directory there is nothing to compare
	// against, and a guess here would invent prompts.
	if cwd == "" || !touchesFiles(cmd) {
		return "", false
	}
	for _, w := range looseWords(cmd) {
		if !strings.HasPrefix(w, "/") || strings.Contains(w, "://") {
			continue
		}
		// underWorkspace is a containment test on POSIX paths; the "workspace"
		// it is given here is the harness's own working directory.
		if underWorkspace(w, cwd) {
			continue
		}
		return w, true
	}
	return "", false
}

// touchesFiles reports whether a command names a file operation the local
// check would evaluate: a redirect, or a command reach recognises as reading
// or writing files.
func touchesFiles(cmd string) bool {
	if strings.ContainsAny(cmd, "<>") {
		return true
	}
	for _, segment := range strings.FieldsFunc(cmd, func(r rune) bool {
		return r == '|' || r == '&' || r == ';' || r == '\n'
	}) {
		fields := strings.Fields(segment)
		if len(fields) == 0 {
			continue
		}
		name := path.Base(strings.Trim(fields[0], `"'`))
		if readOnlyShellCommands[name] || fileMutatingCommands[name] {
			return true
		}
	}
	return false
}

// looseWords pulls the path-shaped words out of a command without pretending
// to parse it. Quotes, operators and --flag=value are all just separators.
func looseWords(cmd string) []string {
	return strings.FieldsFunc(cmd, func(r rune) bool {
		switch r {
		case ' ', '\t', '\n', '\r', '|', '&', ';', '(', ')', '<', '>', '"', '\'', '=':
			return true
		}
		return false
	})
}
