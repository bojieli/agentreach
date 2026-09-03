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
		{"sed 's/a/b/' /srv/app/x", "without -n reach cannot read the script's intent"},
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

// The second decision: which commands the local check would refuse outright,
// and therefore which ones are worth spending an "ask" on. Getting this wrong
// in the generous direction takes away a permission the operator granted.
func TestBashPathRefusedLocally(t *testing.T) {
	const cwd = "/home/me/proj"
	for _, tc := range []struct {
		cmd, cwd string
		wantPath string
		want     bool
	}{
		{"cat > /srv/app/x", cwd, "/srv/app/x", true},
		{"rm -rf /srv/app/build", cwd, "/srv/app/build", true},
		{"tee /srv/app/x", cwd, "/srv/app/x", true},
		{"cp /srv/app/a /srv/app/b", cwd, "/srv/app/a", true},
		{"grep --file=/srv/app/pat /srv/app/x", cwd, "/srv/app/pat", true},
		// A read names a foreign path too. The allow branch reaches it first;
		// this function only reports what the local check would refuse.
		{"cat /srv/app/main.go", cwd, "/srv/app/main.go", true},
		// The trap underWorkspace exists for: a prefix is not a component.
		{"cat /home/me/project-notes/x", cwd, "/home/me/project-notes/x", true},

		// Everything below must be left to the operator's own rules. Forcing a
		// prompt here would override an allowance they configured.
		{"make -C /srv/app", cwd, "", false},
		{"go test ./...", cwd, "", false},
		{"npm install", cwd, "", false},
		{"curl https://example.invalid/x", cwd, "", false},
		{"cat /home/me/proj/x", cwd, "", false},
		{"cat /home/me/proj/deep/nested/x", cwd, "", false},
		{"cat x", cwd, "", false},
		// Without the harness's working directory there is nothing to compare
		// against, and a guess would invent prompts.
		{"cat /srv/app/x", "", "", false},
	} {
		got, ok := bashPathRefusedLocally(tc.cmd, tc.cwd)
		if ok != tc.want {
			t.Errorf("bashPathRefusedLocally(%q, %q) = (%q, %v), want ok=%v", tc.cmd, tc.cwd, got, ok, tc.want)
			continue
		}
		if ok && got != tc.wantPath {
			t.Errorf("bashPathRefusedLocally(%q, %q) named %q, want %q", tc.cmd, tc.cwd, got, tc.wantPath)
		}
	}
}
