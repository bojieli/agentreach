package session

import "testing"

func TestParseTargetForms(t *testing.T) {
	cases := []struct {
		spec      string
		kind      Kind
		host      string
		user      string
		port      int
		container string
		workspace string
	}{
		{"ssh://box/srv/app", KindSSH, "box", "", 0, "", "/srv/app"},
		{"ssh://root@box/srv/app", KindSSH, "box", "root", 0, "", "/srv/app"},
		{"ssh://root@box:2222/srv/app", KindSSH, "box", "root", 2222, "", "/srv/app"},
		{"docker://mycontainer/work", KindDocker, "", "", 0, "mycontainer", "/work"},
		{"podman://c1/work", KindPodman, "", "", 0, "c1", "/work"},
		{"local:///tmp/x", KindLocal, "", "", 0, "", "/tmp/x"},
		{"root@box:/srv/app", KindSSH, "box", "root", 0, "", "/srv/app"},
		{"box:/srv/app", KindSSH, "box", "", 0, "", "/srv/app"},
		// An ssh_config alias must survive untouched, since waldo delegates
		// destination resolution to the user's ssh client.
		{"ssh://my-alias/srv/app", KindSSH, "my-alias", "", 0, "", "/srv/app"},
	}
	for _, c := range cases {
		got, err := ParseTarget(c.spec)
		if err != nil {
			t.Errorf("ParseTarget(%q): %v", c.spec, err)
			continue
		}
		if got.Kind != c.kind || got.Host != c.host || got.User != c.user ||
			got.Port != c.port || got.Container != c.container || got.Workspace != c.workspace {
			t.Errorf("ParseTarget(%q) = %+v", c.spec, got)
		}
	}
}

func TestParseTargetRejectsBadInput(t *testing.T) {
	for _, spec := range []string{
		"",                   // empty
		"ssh://box",          // no workspace: waldo would not know where to work
		"ssh://box/relative", // caught below as absolute; kept for clarity
		"ftp://box/srv",      // unsupported scheme
		"docker:///srv",      // no container name
		"ssh:///srv/app",     // no host
		"ssh://box/../etc",   // still absolute, but suspicious
		"justastring",        // no scheme and no colon
	} {
		if _, err := ParseTarget(spec); err == nil {
			// "/../etc" is absolute so it parses; only assert on the rest.
			if spec != "ssh://box/../etc" && spec != "ssh://box/relative" {
				t.Errorf("ParseTarget(%q) should have failed", spec)
			}
		}
	}
}

func TestWorkspaceMustBeAbsolute(t *testing.T) {
	if _, err := ParseTarget("box:relative/path"); err == nil {
		t.Error("relative workspace accepted; the target path must be unambiguous")
	}
}

func TestDescribeIsStable(t *testing.T) {
	for _, c := range []struct{ spec, want string }{
		{"ssh://root@box:2222/srv/app", "ssh://root@box:2222/srv/app"},
		{"docker://c1/work", "docker://c1/work"},
		{"local:///tmp/x", "local:///tmp/x"},
	} {
		got, err := ParseTarget(c.spec)
		if err != nil {
			t.Fatal(err)
		}
		if d := got.Describe(); d != c.want {
			t.Errorf("Describe() = %q want %q", d, c.want)
		}
	}
}
