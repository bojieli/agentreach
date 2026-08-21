//go:build integration

// Package integration exercises reach against a real sshd rather than a mock.
//
// Mocks cannot catch the failures that actually happen here: shell quoting that
// works locally but not through ssh's own re-parsing, exit statuses lost to
// ssh's use of 255, or userland differences between the machine running the
// tests and the target. Everything in this file talks to a real OpenSSH server;
// see harness_test.go for how one is started.
package integration

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/bojieli/agentreach/internal/fileops"
	"github.com/bojieli/agentreach/internal/transport"
	"github.com/bojieli/agentreach/internal/reach"
)

func TestSSHExecOverRealServer(t *testing.T) {
	tr := newTransport(t)
	res, err := tr.Run(context.Background(), reach.ExecRequest{Command: "echo hello; echo err >&2; exit 3"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Code != 3 {
		t.Errorf("exit code = %d want 3", res.Code)
	}
	if strings.TrimSpace(string(res.Stdout)) != "hello" {
		t.Errorf("stdout = %q", res.Stdout)
	}
	if !strings.Contains(string(res.Stderr), "err") {
		t.Errorf("stderr = %q", res.Stderr)
	}
}

// TestSSHDoesNotConfuseItsOwn255 is the reason exit status travels in-band.
// ssh reports its own failures as 255, so a remote command exiting 255 must
// still be reported as the command's status, not as a broken connection.
func TestSSHDoesNotConfuseItsOwn255(t *testing.T) {
	tr := newTransport(t)
	res, err := tr.Run(context.Background(), reach.ExecRequest{Command: "exit 255"})
	if err != nil {
		t.Fatalf("remote exit 255 misreported as transport failure: %v", err)
	}
	if res.Code != 255 {
		t.Errorf("exit code = %d want 255", res.Code)
	}
}

func TestSSHQuotingSurvivesRemoteReparse(t *testing.T) {
	tr := newTransport(t)
	// ssh joins its trailing arguments and hands the result to a remote shell,
	// so anything shell-significant gets a second round of interpretation.
	for _, s := range []string{
		"plain", "with space", "it's", `"double"`, "$(hostname)", "`id`",
		"semi;colon", "pipe|char", "{brace}", "*glob*", "new\nline", "tab\there",
	} {
		res, err := tr.Run(context.Background(), reach.ExecRequest{
			Command: "printf '%s' " + transport.ShellQuote(s),
		})
		if err != nil {
			t.Fatalf("%q: %v", s, err)
		}
		if string(res.Stdout) != s {
			t.Errorf("round trip: got %q want %q", res.Stdout, s)
		}
	}
}

func TestFileOpsOverRealSSH(t *testing.T) {
	tr := newTransport(t)
	ctx := context.Background()
	caps, err := fileops.Probe(ctx, tr)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	t.Logf("target userland: %s stat=%s rg=%q", caps.Uname, caps.StatFlavor, caps.Ripgrep)

	fo := fileops.NewPOSIX(tr, caps)
	dir := workspace + "/itest"
	if err := fo.Mkdir(ctx, dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Binary safety over a real ssh connection, which adds its own layer of
	// escaping on top of the shell's.
	payload := []byte{0x00, 0x01, 0xff, 0xfe, '\n', '\'', '"', '$', '`', 0x80}
	fp := dir + "/binary.dat"
	if err := fo.Write(ctx, fp, payload, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := fo.Read(ctx, fp, 0, 0)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("binary round trip failed: got % x want % x", got, payload)
	}

	// A payload larger than one read chunk, to exercise chunked reassembly.
	big := bytes.Repeat([]byte("reach"), 400_000) // 2 MB
	bp := dir + "/big.dat"
	if err := fo.Write(ctx, bp, big, 0o644); err != nil {
		t.Fatalf("write big: %v", err)
	}
	gotBig, err := fo.Read(ctx, bp, 0, 0)
	if err != nil {
		t.Fatalf("read big: %v", err)
	}
	if !bytes.Equal(gotBig, big) {
		t.Fatalf("large round trip failed: got %d bytes want %d", len(gotBig), len(big))
	}

	if fi, err := fo.Stat(ctx, fp); err != nil || fi.Size != int64(len(payload)) {
		t.Errorf("stat: %+v err=%v", fi, err)
	}
	entries, err := fo.List(ctx, dir)
	if err != nil || len(entries) != 2 {
		t.Errorf("list returned %d entries (err=%v)", len(entries), err)
	}
	if err := fo.Remove(ctx, dir, true); err != nil {
		t.Errorf("remove: %v", err)
	}
}

func TestSearchRunsOnTarget(t *testing.T) {
	tr := newTransport(t)
	ctx := context.Background()
	caps, err := fileops.Probe(ctx, tr)
	if err != nil {
		t.Fatal(err)
	}
	fo := fileops.NewPOSIX(tr, caps)
	dir := workspace + "/searchtest"
	if err := fo.Mkdir(ctx, dir, 0o755); err != nil {
		t.Fatal(err)
	}
	defer fo.Remove(ctx, dir, true)

	if err := fo.Write(ctx, dir+"/a.txt", []byte("alpha\nNEEDLE here\ngamma\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := fo.Write(ctx, dir+"/b.txt", []byte("nothing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := fo.Search(ctx, reach.SearchRequest{Pattern: "NEEDLE", Root: dir})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(m) != 1 || m[0].Line != 2 {
		t.Fatalf("search result wrong: %+v", m)
	}
}

// TestAgentForwardingIsRefused is a security guarantee under test rather than
// under documentation.
//
// SECURITY.md promises that reach never forwards an SSH agent, *including* when
// the operator's own ssh config enables it for that host — because on a machine
// with a hostile root, a forwarded agent socket lets that host authenticate as
// the operator against every other system they can reach. It converts one
// compromised server into all of them.
//
// The claim rested on OpenSSH giving command-line options precedence over the
// config file. That is true, and reading it in a manual page is not the same as
// watching the socket fail to appear.
func TestAgentForwardingIsRefused(t *testing.T) {
	if os.Getenv("SSH_AUTH_SOCK") == "" {
		t.Skip("no ssh-agent running locally, so there is nothing that could be forwarded")
	}
	// Ask for forwarding as loudly as possible: the operator's config saying
	// yes is exactly the case reach has to override.
	tr, err := transport.NewSSH(transport.SSHConfig{
		Host:         sshHostAlias,
		BatchMode:    true,
		ExtraOptions: []string{"ForwardAgent=yes"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tr.Close() })

	res, err := tr.Run(context.Background(), reach.ExecRequest{
		Command: `printf '%s' "${SSH_AUTH_SOCK:-}"`,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if sock := strings.TrimSpace(string(res.Stdout)); sock != "" {
		t.Fatalf("an agent socket reached the target at %q.\n"+
			"reach must refuse agent forwarding even when asked for it: a target with a "+
			"hostile root can use a forwarded socket to authenticate as the operator "+
			"everywhere else they can reach.", sock)
	}
}
