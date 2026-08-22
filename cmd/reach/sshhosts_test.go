package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSSHConfigNames(t *testing.T) {
	dir := t.TempDir()
	included := filepath.Join(dir, "work.conf")
	if err := os.WriteFile(included, []byte("Host client-vm\n  User deploy\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, "config")
	body := "" +
		"# a comment\n" +
		"Include work.conf\n" +
		"Host build-box docs-box\n" +
		"  HostName 10.0.0.9\n" +
		"Host prod-*\n" +
		"  User root\n" +
		"Host=equals-form\n" +
		"Host *\n" +
		"  ServerAliveInterval 60\n"
	if err := os.WriteFile(config, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REACH_SSH_CONFIG", config)

	for _, tc := range []struct {
		host string
		want bool
	}{
		{"build-box", true},
		{"docs-box", true},    // second name on one Host line
		{"BUILD-BOX", true},   // ssh host matching is case-insensitive
		{"prod-eu-1", true},   // pattern
		{"equals-form", true}, // Host=name is legal ssh_config
		{"client-vm", true},   // reached through Include, relative to the file
		// `Host *` is in nearly every configuration. Honouring it would make
		// every mistyped command a hostname, which is the mistake this whole
		// check exists to avoid.
		{"stauts", false},
		{"prod", false},
	} {
		if got := sshConfigNames(tc.host); got != tc.want {
			t.Errorf("sshConfigNames(%q) = %v, want %v", tc.host, got, tc.want)
		}
	}
}

// A configuration that includes itself must not make reach spin.
func TestSSHConfigIncludeCycleTerminates(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "config")
	if err := os.WriteFile(config, []byte("Include config\nHost box\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REACH_SSH_CONFIG", config)
	if !sshConfigNames("box") {
		t.Error("a self-including config lost the entry it did contain")
	}
	if sshConfigNames("elsewhere") {
		t.Error("a self-including config matched something it does not contain")
	}
}

func TestLooksLikeHost(t *testing.T) {
	for _, tc := range []struct {
		spec string
		want bool
	}{
		{"build-box", true},
		{"root@build-box", true},
		{"build.example.com", true},
		{"10.0.0.9", true},
		{"", false},
		{"has space", false},
		{"has/slash", false},
		{"-leading-dash", false},
	} {
		if got := looksLikeHost(tc.spec); got != tc.want {
			t.Errorf("looksLikeHost(%q) = %v, want %v", tc.spec, got, tc.want)
		}
	}
}

func TestHostsFileNames(t *testing.T) {
	// hostsFilePath is not configurable — it is a system file — so this reads
	// the real one and only asserts what every system agrees on.
	if !hostsFileNames("localhost") {
		t.Skip("this system's hosts file does not name localhost")
	}
	if hostsFileNames("stauts-not-in-any-hosts-file") {
		t.Error("the hosts file matched a name it cannot contain")
	}
}
