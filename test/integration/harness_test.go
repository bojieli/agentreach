//go:build integration

package integration

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/waldo/internal/transport"
)

// The integration suite needs a real OpenSSH server, because the bugs it exists
// to catch only appear there: shell quoting that survives one round of
// interpretation but not ssh's second, exit statuses lost to ssh's own use of
// 255, and the SFTP subsystem, which cannot be simulated at all.
//
// It starts one as the current user on a high port rather than requiring
// Docker. That runs anywhere sshd is installed — both CI runners, and a
// contributor's laptop with no container runtime and no network — and it tests
// against the userland of whatever machine it runs on, so the GNU and BSD sides
// of the CI matrix each exercise their own. Set WALDO_TEST_SSHD=docker to use a
// Debian container instead, which is how a GNU target gets covered from a BSD
// host.
const (
	containerName = "waldo-integration-sshd"
	dockerPort    = "22333"
	localPort     = "22411"
)

// sshHostAlias is the destination the suite hands to ssh. It is the alias the
// suite defines in its own throwaway config for a target it started itself, and
// the operator's own alias when pointed at a host they already have.
var sshHostAlias = "waldo-it"

var (
	testDir string
	// workspace is a directory on the target the tests may use freely.
	workspace string
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "waldo-integration-")
	if err != nil {
		panic(err)
	}
	testDir = dir

	stop, err := startTarget()
	if err != nil {
		fmt.Fprintln(os.Stderr, "could not start a test target:", err)
		if stop != nil {
			stop()
		}
		_ = os.RemoveAll(dir)
		// Skipping rather than failing would let the suite silently stop
		// covering anything, which for a project whose claims rest on
		// integration evidence is worse than a red build.
		os.Exit(1)
	}
	code := m.Run()
	stop()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func startTarget() (func(), error) {
	// Point the suite at a host you already have. This is how waldo gets tested
	// against a target over a real network rather than loopback — different
	// latency, a real sshd configuration, someone else's userland — and it is
	// the only way to exercise a host whose ssh config has ProxyJump, a
	// certificate, or a hardware token in front of it.
	//
	//   WALDO_TEST_SSH_HOST=my-box go test -tags integration ./test/integration/...
	//
	// The suite creates one directory under WALDO_TEST_SSH_WORKSPACE (default
	// /tmp) and removes it afterwards. It writes nothing else.
	if host := os.Getenv("WALDO_TEST_SSH_HOST"); host != "" {
		return useExistingHost(host)
	}
	if os.Getenv("WALDO_TEST_SSHD") == "docker" {
		return startDockerTarget()
	}
	return startLocalTarget()
}

// useExistingHost prepares a scratch directory on a host the operator already
// has access to, and takes it away again afterwards.
func useExistingHost(host string) (func(), error) {
	base := os.Getenv("WALDO_TEST_SSH_WORKSPACE")
	if base == "" {
		base = "/tmp"
	}
	// A name that cannot collide with anything already there, and that says what
	// it is if anyone finds it.
	var nonce [6]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	workspace = path.Join(base, "waldo-integration-"+hex.EncodeToString(nonce[:]))

	// Use the operator's destination verbatim, with their own ssh config and no
	// alias of the suite's own.
	//
	// Defining `Host waldo-it / HostName <theirs>` looks equivalent and is not:
	// ssh applies the first HostName it obtains and does not then re-match Host
	// blocks against the result, so their `Host <theirs>` stanza — the one
	// carrying the address, the user and the key — would never be consulted. The
	// destination has to stay the string they wrote.
	sshHostAlias = host
	if err := os.Unsetenv("WALDO_SSH_CONFIG"); err != nil {
		return nil, err
	}
	if err := waitForSSH(); err != nil {
		return nil, fmt.Errorf("%s is not reachable: %w", host, err)
	}
	if out, err := exec.Command("ssh", host, "mkdir -p "+workspace).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("create %s on %s: %v: %s", workspace, host, err, out)
	}
	return func() {
		// Leave nothing behind on a machine that was lent to the suite.
		_ = exec.Command("ssh", host, "rm -rf "+workspace).Run()
	}, nil
}

// startLocalTarget runs an sshd owned by the current user.
//
// It listens only on loopback, accepts only the throwaway key generated here,
// and dies with the test run. It cannot authenticate anyone but the user
// running it, because an unprivileged sshd cannot change uid.
func startLocalTarget() (func(), error) {
	sshdBin, err := findSSHD()
	if err != nil {
		return nil, err
	}
	sftpServer, err := findSFTPServer()
	if err != nil {
		return nil, err
	}
	key := filepath.Join(testDir, "id")
	hostKey := filepath.Join(testDir, "hostkey")
	for _, k := range []string{key, hostKey} {
		if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", k).CombinedOutput(); err != nil {
			return nil, fmt.Errorf("ssh-keygen: %v: %s", err, out)
		}
	}
	pub, err := os.ReadFile(key + ".pub")
	if err != nil {
		return nil, err
	}
	authorized := filepath.Join(testDir, "authorized_keys")
	if err := os.WriteFile(authorized, pub, 0o600); err != nil {
		return nil, err
	}

	user := os.Getenv("USER")
	if user == "" {
		user = os.Getenv("LOGNAME")
	}
	if user == "" {
		return nil, fmt.Errorf("cannot determine the current user for sshd")
	}

	// StrictModes is off because the key material lives in a temp directory
	// whose ownership sshd would otherwise object to. Nothing here is a
	// credential that outlives the test run.
	cfg := fmt.Sprintf(`Port %s
ListenAddress 127.0.0.1
HostKey %s
PidFile %s
AuthorizedKeysFile %s
StrictModes no
UsePAM no
PasswordAuthentication no
KbdInteractiveAuthentication no
PermitUserEnvironment no
PrintMotd no
LogLevel ERROR
Subsystem sftp %s
`, localPort, hostKey, filepath.Join(testDir, "sshd.pid"), authorized, sftpServer)
	cfgPath := filepath.Join(testDir, "sshd_config")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		return nil, err
	}

	logPath := filepath.Join(testDir, "sshd.log")
	cmd := exec.Command(sshdBin, "-D", "-f", cfgPath, "-E", logPath)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", sshdBin, err)
	}
	stop := func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	}

	if err := writeSSHConfig(localPort, user, key); err != nil {
		stop()
		return nil, err
	}
	if err := waitForSSH(); err != nil {
		log, _ := os.ReadFile(logPath)
		stop()
		return nil, fmt.Errorf("%w (sshd log: %s)", err, strings.TrimSpace(string(log)))
	}
	workspace = filepath.Join(testDir, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		stop()
		return nil, err
	}
	return stop, nil
}

func startDockerTarget() (func(), error) {
	if _, err := exec.LookPath("docker"); err != nil {
		return nil, fmt.Errorf("WALDO_TEST_SSHD=docker but docker is not installed")
	}
	key := filepath.Join(testDir, "id")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", key).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("ssh-keygen: %v: %s", err, out)
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
		return nil, err
	}
	if out, err := exec.Command("docker", "build", "-q", "-t", "waldo-integration", testDir).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("docker build: %v: %s", err, out)
	}
	_ = exec.Command("docker", "rm", "-f", containerName).Run()
	if out, err := exec.Command("docker", "run", "-d", "--name", containerName,
		"-p", dockerPort+":22", "waldo-integration").CombinedOutput(); err != nil {
		return nil, fmt.Errorf("docker run: %v: %s", err, out)
	}
	stop := func() { _ = exec.Command("docker", "rm", "-f", containerName).Run() }

	if err := writeSSHConfig(dockerPort, "root", key); err != nil {
		stop()
		return nil, err
	}
	if err := waitForSSH(); err != nil {
		stop()
		return nil, err
	}
	workspace = "/srv/app"
	return stop, nil
}

func writeSSHConfig(port, user, key string) error {
	cfg := fmt.Sprintf(`Host %s
  HostName 127.0.0.1
  Port %s
  User %s
  IdentityFile %s
  IdentitiesOnly yes
  StrictHostKeyChecking no
  UserKnownHostsFile /dev/null
  LogLevel ERROR
`, sshHostAlias, port, user, key)
	cfgPath := filepath.Join(testDir, "ssh_config")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		return err
	}
	return os.Setenv("WALDO_SSH_CONFIG", cfgPath)
}

func waitForSSH() error {
	args := []string{sshHostAlias, "true"}
	if cfg := os.Getenv("WALDO_SSH_CONFIG"); cfg != "" {
		args = append([]string{"-F", cfg}, args...)
	}
	for i := 0; i < 60; i++ {
		if exec.Command("ssh", args...).Run() == nil {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("sshd did not become reachable")
}

func findSSHD() (string, error) {
	for _, p := range []string{"/usr/sbin/sshd", "/usr/local/sbin/sshd", "/opt/homebrew/sbin/sshd"} {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p, nil
		}
	}
	if p, err := exec.LookPath("sshd"); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("no sshd found; install openssh-server or set WALDO_TEST_SSHD=docker")
}

// findSFTPServer locates the subsystem binary. Without it the SFTP tier cannot
// be tested, and a tier that ships untested is exactly what this suite exists
// to prevent.
func findSFTPServer() (string, error) {
	for _, p := range []string{
		"/usr/libexec/sftp-server",
		"/usr/lib/openssh/sftp-server",
		"/usr/libexec/openssh/sftp-server",
		"/usr/lib/ssh/sftp-server",
		"/opt/homebrew/libexec/sftp-server",
	} {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("no sftp-server binary found; the sftp tier could not be tested")
}

func newTransport(t *testing.T) transport.Transport {
	t.Helper()
	// Multiplexing on, because that is what a session does: `waldo up` probes
	// for it and records the answer, so a suite that left it off would be
	// testing a configuration no operator runs.
	//
	// It is also the difference between a suite that finishes and one that does
	// not. Against a loopback sshd a cold connect costs ~30 ms and the omission
	// is invisible; against a real host it was 3.6 s per command, and the suite
	// ran for twenty minutes without finishing. TestWorksWithoutMultiplexing
	// covers the unmultiplexed path deliberately.
	tr, err := transport.NewSSH(transport.SSHConfig{
		Host: sshHostAlias, BatchMode: true, Multiplex: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tr.Close() })
	return tr
}
