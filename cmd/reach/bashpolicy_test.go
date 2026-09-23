package main

import "testing"

// What reach vouches for is what the operator stops being asked about, so the
// interesting cases here are not the ones that read a file. They are the ones
// that look like they only read a file.

func TestReadOnlyBashCommandVouchesForReads(t *testing.T) {
	for _, cmd := range []string{
		"cat /srv/app/main.go",
		"cat -- /srv/app/main.go",
		"/bin/cat /srv/app/main.go",
		"head -n 40 /srv/app/main.go",
		"sed -n '1,50p' /srv/app/main.go",
		"sed -n '$p' /srv/app/main.go",
		"grep -rn pattern /srv/app",
		`grep -rn "用户名" /srv/app/app/utils.py`,
		"rg 用户名 /srv/app",
		"ls -la /srv/app",
		"find /srv/app -name '*.go'",
		"stat /srv/app/main.go",
		"wc -l /srv/app/main.go",
		"diff /srv/app/a /srv/app/b",
		// A pipeline is read-only when every stage is.
		"cat /srv/app/a.txt | head -20",
		"rg pattern /srv/app | wc -l",
		"head -n 5 /srv/app/x && wc -l /srv/app/y",
		// cd is not itself a file operation. The harness asks about this shape
		// anyway — a compound command it cannot resolve is its own rule — but
		// that is not a reason for reach to call it a write.
		"cd /srv/app && ls -la",
		// A semicolon is sequencing, no different from && for what runs.
		"ls /srv/app; ls ~/work",
		// Discarding or joining streams writes nothing.
		"ls /srv/app 2>/dev/null",
		"grep -rn x /srv/app/config* 2>/dev/null | head",
		"ls /srv/app >/dev/null 2>&1",
		"ls /srv/app &>/dev/null",
		"ls /srv/app 2> /dev/null",
		// One substitution prints; it cannot write.
		"sed 's/a/b/' /srv/app/x",
		"sed -E 's|:[^:@]*@|:***@|g' /srv/app/x",
		`sed 's/a\/b/c/2' /srv/app/x`,
		// The command an agent actually sent, verbatim.
		`ls /srv/ustc-course; ls ~/work; grep -rn "SQLALCHEMY_DATABASE_URI\|mysql\|postgres" /srv/ustc-course/config* 2>/dev/null | sed 's/:[^:@]*@/:***@/' | head; grep -n "class User\b\|class User(" -A40 /srv/ustc-course/app/models/user.py 2>/dev/null | grep -n "Column" | head -30`,
	} {
		if !readOnlyBashCommand(cmd) {
			t.Errorf("readOnlyBashCommand(%q) = false; this only reads and the operator will be asked about it", cmd)
		}
	}
}

func TestReadOnlyBashCommandRefusesEverythingElse(t *testing.T) {
	for _, tc := range []struct {
		cmd, why string
	}{
		{"cat > /srv/app/x", "a redirect writes"},
		{"cat /srv/app/x > /tmp/y", "a redirect writes even when the command reads"},
		{"cat /srv/app/x >> /tmp/y", "an appending redirect writes"},
		{"cat > /srv/app/x <<'EOF'\nhi\nEOF", "a heredoc write spans lines"},
		{"rm -rf /srv/app/build", "rm is not a read"},
		{"cat /srv/app/x | tee /srv/app/y", "the last stage of the pipeline writes"},
		{"sed -i 's/a/b/' /srv/app/x", "sed -i edits in place"},
		{"sed -n 's/a/b/w out' /srv/app/x", "a sed script can write without -i"},
		{"sed 's/a/b/e' /srv/app/x", "the e flag runs the pattern space as a command"},
		{"sed 's/a/b/;w out' /srv/app/x", "a second command can write"},
		{"sed 's/a/b/\nw out' /srv/app/x", "a second line can write"},
		{"sed y/ab/cd/ /srv/app/x", "reach reads s and line ranges, nothing else"},
		{"ls /srv/app 2>/tmp/err", "a stderr redirect to a file writes"},
		{"ls /srv/app >/dev/nullx", "that is a file, not /dev/null"},
		{"ls /srv/app >&-", "closing a stream is not a redirect reach reads"},
		{"ls /srv/app >| /tmp/x", "a clobbering redirect writes"},
		{"ls /srv/app &> /tmp/x", "&> writes both streams to a file"},
		{"ls /srv/app 2>&1 > /tmp/x", "one harmless redirect does not vouch for the next"},
		{"tail -f /var/log/app.log", "-f never returns"},
		{"find /srv/app -name '*.go' -delete", "find deletes"},
		{"find /srv/app -exec rm {} +", "find runs a command of its own"},
		{"rg --pre ./preprocess pattern /srv/app", "--pre runs a program"},
		{"cat $(ls /srv/app)", "a substitution hides the argument"},
		{"cat `ls /srv/app`", "a backtick substitution hides the argument"},
		{"cat /srv/app/$FILE", "an expansion hides the path"},
		{`cat "/srv/app/$FILE"`, "an expansion inside quotes hides the path"},
		{"FOO=1 cat /srv/app/x", "an assignment prefix moves the command"},
		{"sh -c 'cat /srv/app/x'", "the words are not the command that runs"},
		{"cat /srv/app/x; rm -rf /tmp/y", "a semicolon chains something else"},
		{"cat /srv/app/x &", "a background command outlives the decision"},
		{"ls /srv/app && rm -rf /tmp/x", "one read does not vouch for the rest"},
		{"awk '{print > \"f\"}' /srv/app/x", "awk writes"},
		{"xargs rm", "xargs runs a command of its own"},
		{"cat 'unbalanced", "an unbalanced quote cannot be read"},
		{"make build", "not a file command at all"},
		{"go test ./...", "not a file command at all"},
		{"echo hi", "harmless, but reach vouches for reads, not for harmlessness"},
		{"", "there is nothing to vouch for"},
	} {
		if readOnlyBashCommand(tc.cmd) {
			t.Errorf("readOnlyBashCommand(%q) = true, but %s", tc.cmd, tc.why)
		}
	}
}
