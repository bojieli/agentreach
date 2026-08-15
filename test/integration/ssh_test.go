//go:build integration

// Package integration exercises waldo against a real sshd rather than a mock.
//
// Mocks cannot catch the failures that actually happen here: shell quoting that
// works locally but not through ssh's own re-parsing, exit statuses lost to
// ssh's use of 255, or userland differences between the machine running the
// tests and the target. Everything in this file talks to a real OpenSSH server
// in a container.
package integration

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/waldo/internal/fileops"
	"github.com/bojieli/waldo/internal/transport"
	"github.com/bojieli/waldo/internal/waldo"
)

const (
	containerName = "waldo-integration-sshd"
	sshPort       = "22333"
)

var testDir string

func TestMain(m *testing.M) {
	if _, err := exec.LookPath("docker"); err != nil {
		fmt.Fprintln(os.Stderr, "docker not available; skipping integration tests")
		os.Exit(0)
	}
	dir, err := os.MkdirTemp("", "waldo-integration-")
	if err != nil {
		panic(err)
	}
	testDir = dir
	if err := startTarget(); err != nil {
		fmt.Fprintln(os.Stderr, "could not start test target:", err)
		stopTarget()
		os.Exit(1)
	}
	code := m.Run()
	stopTarget()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func startTarget() error {
	key := filepath.Join(testDir, "id")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", key).CombinedOutput(); err != nil {
		return fmt.Errorf("ssh-keygen: %v: %s", err, out)
	}
	dockerfile := `FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends openssh-server ca-certificates \
 && rm -rf /var/lib/apt/lists/* && mkdir -p /run/sshd /root/.ssh && chmod 700 /root/.ssh
COPY id.pub /root/.ssh/authorized_keys
RUN chmod 600 /root/.ssh/authorized_keys && \
    sed -i 's/^#\?PermitRootLogin.*/PermitRootLogin prohibit-password/' /etc/ssh/sshd_config
RUN mkdir -p /srv/app
EXPOSE 22
CMD ["/usr/sbin/sshd","-D","-e"]`
	if err := os.WriteFile(filepath.Join(testDir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		return err
	}
	if out, err := exec.Command("docker", "build", "-q", "-t", "waldo-integration", testDir).CombinedOutput(); err != nil {
		return fmt.Errorf("docker build: %v: %s", err, out)
	}
	_ = exec.Command("docker", "rm", "-f", containerName).Run()
	if out, err := exec.Command("docker", "run", "-d", "--name", containerName,
		"-p", sshPort+":22", "waldo-integration").CombinedOutput(); err != nil {
		return fmt.Errorf("docker run: %v: %s", err, out)
	}

	cfg := fmt.Sprintf(`Host waldo-it
  HostName 127.0.0.1
  Port %s
  User root
  IdentityFile %s
  StrictHostKeyChecking no
  UserKnownHostsFile /dev/null
  LogLevel ERROR
`, sshPort, key)
	cfgPath := filepath.Join(testDir, "ssh_config")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		return err
	}
	os.Setenv("WALDO_SSH_CONFIG", cfgPath)

	for i := 0; i < 60; i++ {
		if exec.Command("ssh", "-F", cfgPath, "waldo-it", "true").Run() == nil {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("sshd did not become reachable")
}

func stopTarget() { _ = exec.Command("docker", "rm", "-f", containerName).Run() }

func newTransport(t *testing.T) transport.Transport {
	t.Helper()
	tr, err := transport.NewSSH(transport.SSHConfig{Host: "waldo-it", BatchMode: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tr.Close() })
	return tr
}

func TestSSHExecOverRealServer(t *testing.T) {
	tr := newTransport(t)
	res, err := tr.Run(context.Background(), waldo.ExecRequest{Command: "echo hello; echo err >&2; exit 3"})
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
	res, err := tr.Run(context.Background(), waldo.ExecRequest{Command: "exit 255"})
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
		res, err := tr.Run(context.Background(), waldo.ExecRequest{
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
	dir := "/srv/app/itest"
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
	big := bytes.Repeat([]byte("waldo"), 400_000) // 2 MB
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
	dir := "/srv/app/searchtest"
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
	m, err := fo.Search(ctx, waldo.SearchRequest{Pattern: "NEEDLE", Root: dir})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(m) != 1 || m[0].Line != 2 {
		t.Fatalf("search result wrong: %+v", m)
	}
}
