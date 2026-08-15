package fileops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/bojieli/waldo/internal/transport"
	"github.com/bojieli/waldo/internal/waldo"
)

// HelperBinaryEnv names an explicit path to a helper binary for the target's
// platform, for operators who build or vendor their own.
const HelperBinaryEnv = "WALDO_HELPER_BINARY"

// NewHelper installs the helper binary if needed and starts it.
//
// This is the only tier that writes to the target. Everything about it is
// therefore built to be reversible and visible: the version is in the path so
// an upgrade never reuses a stale binary, the content is verified by digest
// after upload so a truncated transfer is caught rather than executed, `waldo
// doctor` reports what is there, and `waldo helper uninstall` removes it.
func NewHelper(ctx context.Context, t transport.Transport, base *POSIX, caps *Capabilities) (FileOps, error) {
	goos, goarch, err := platformOf(caps.Uname)
	if err != nil {
		return nil, err
	}

	local, err := LocateHelperBinary(ctx, goos, goarch)
	if err != nil {
		return nil, err
	}
	payload, err := os.ReadFile(local)
	if err != nil {
		return nil, fmt.Errorf("read helper binary %s: %w", local, err)
	}
	sum := sha256.Sum256(payload)
	digest := hex.EncodeToString(sum[:])

	remote, err := HelperPath(ctx, t, caps, goos, goarch)
	if err != nil {
		return nil, err
	}

	// Verification is a per-session fact, not a per-tool-call one.
	//
	// Asking the installed binary to identify itself costs a round trip, and
	// waldo runs one process per tool call, so doing it every time tripled this
	// tier's cost: three round trips to read one small file, on a tier whose
	// whole argument is that it needs one. It happens when the session is
	// created — where an operator is present to see a mismatch — and is
	// recorded.
	//
	// The trade is explicit: a binary swapped underneath a live session is not
	// noticed until the next `waldo up`. Catching that would mean re-verifying
	// before every operation, which is the cost this avoids, and a target able
	// to swap the binary can equally lie about its digest.
	if caps.HelperPath == remote && caps.HelperDigest == digest {
		return startHandler(ctx, t, base, waldo.TierHelper, "helper",
			fmt.Sprintf("exec %s serve", transport.ShellQuote(remote)), "")
	}

	if !helperMatches(ctx, t, remote, digest, goos, goarch) {
		if err := installHelper(ctx, base, remote, payload); err != nil {
			return nil, err
		}
		if !helperMatches(ctx, t, remote, digest, goos, goarch) {
			// Take back what was just written. waldo put this binary on the
			// target and has now declared it cannot identify it; leaving an
			// executable waldo does not trust on a machine the operator may not
			// own is the opposite of what this tier promises.
			//
			// What the message says about it depends on whether it worked. A
			// refusal that claims a cleanup it did not perform leaves an
			// executable on someone else's host that nobody knows is there,
			// which is a worse outcome than the mismatch being reported.
			fate := "and has been removed from the target"
			if err := base.Remove(ctx, remote, false); err != nil {
				fate = fmt.Sprintf("and could not be removed (%v); it is still there", err)
			}

			// The local source is named, not only the destination. The usual
			// cause is a helper from a *different* waldo sitting where this one
			// looks: release archives ship helpers beside the waldo binary, so
			// upgrading in place leaves the previous version's helpers exactly
			// there. Reporting only the remote path sends the operator to
			// inspect a file that is a faithful copy of the wrong one.
			return nil, fmt.Errorf(
				"the helper waldo installed at %s did not identify itself as waldo %s,\n"+
					"%s.\n"+
					"It was copied from %s, so that file is probably from a different waldo:\n"+
					"release archives ship helpers beside the waldo binary, and upgrading in\n"+
					"place leaves the old ones there. Remove it, or point %s at the right one.\n"+
					"waldo will not run a binary it cannot identify; use --fileops=pipe or omit\n"+
					"--fileops to negotiate a tier that installs nothing",
				remote, waldo.Version, fate, local, HelperBinaryEnv)
		}
	}

	// Record what was verified so the rest of the session can skip it.
	caps.HelperPath, caps.HelperDigest = remote, digest

	return startHandler(ctx, t, base, waldo.TierHelper, "helper",
		fmt.Sprintf("exec %s serve", transport.ShellQuote(remote)), "")
}

// helperMatches asks an installed helper to identify itself.
//
// Both the version and the content digest must match. Version alone would
// happily accept a truncated upload, and a digest alone would accept a binary
// from a different waldo release that happened to hash the same way it did
// before an upgrade — neither is something to run on someone else's machine.
func helperMatches(ctx context.Context, t transport.Transport, remote, digest, goos, goarch string) bool {
	res, err := t.Run(ctx, waldo.ExecRequest{
		Command:   fmt.Sprintf("%s --selftest 2>/dev/null", transport.ShellQuote(remote)),
		MaxOutput: 4 << 10,
	})
	if err != nil || res.Code != 0 {
		return false
	}
	want := fmt.Sprintf("waldo-helper %s %s %s/%s", waldo.Version, digest, goos, goarch)
	return strings.TrimSpace(string(res.Stdout)) == want
}

// installHelper uploads the binary using the tier that needs nothing installed.
//
// Tier 0 is used deliberately: bootstrapping the fast tier with the universal
// one means installation works on exactly the hosts waldo can already reach,
// with no separate upload path to keep correct.
func installHelper(ctx context.Context, base *POSIX, remote string, payload []byte) error {
	if err := base.Mkdir(ctx, path.Dir(remote), 0o700); err != nil {
		return fmt.Errorf("create the helper cache directory on the target: %w", err)
	}
	if err := base.Write(ctx, remote, payload, 0o700); err != nil {
		return fmt.Errorf("upload the helper to %s: %w", remote, err)
	}
	return nil
}

// HelperPath resolves where the agent lives on a target.
//
// The version is part of the filename so that an upgraded waldo installs a new
// helper rather than silently reusing an old one, and so that `waldo doctor` can
// list exactly what waldo has left on a host.
func HelperPath(ctx context.Context, t transport.Transport, caps *Capabilities, goos, goarch string) (string, error) {
	cache := ""
	if caps != nil {
		cache = strings.TrimSpace(caps.CacheDir)
	}
	if cache == "" {
		// A session created before the probe reported this, or a caller with no
		// capabilities to hand. Costs the round trip this field exists to save.
		res, err := t.Run(ctx, waldo.ExecRequest{
			Command:   `printf %s "${XDG_CACHE_HOME:-$HOME/.cache}"`,
			MaxOutput: 4 << 10,
		})
		if err != nil {
			return "", fmt.Errorf("resolve the target's cache directory: %w", err)
		}
		cache = strings.TrimSpace(string(res.Stdout))
	}
	if cache == "" || !strings.HasPrefix(cache, "/") {
		return "", fmt.Errorf("the target reported no usable cache directory (%q); "+
			"the helper tier needs somewhere it may write", cache)
	}
	return path.Join(cache, "waldo", fmt.Sprintf("helper-%s-%s-%s", waldo.Version, goos, goarch)), nil
}

// HelperCacheDir is the directory waldo creates on a target for the helper
// tier, and the only directory it ever removes there.
func HelperCacheDir(ctx context.Context, t transport.Transport) (string, error) {
	p, err := HelperPath(ctx, t, nil, "x", "y")
	if err != nil {
		return "", err
	}
	return path.Dir(p), nil
}

// LocateHelperBinary finds a helper build for a target platform.
//
// Release archives ship one per supported platform beside the waldo binary. A
// source checkout has a Go toolchain by definition, so waldo cross-compiles one
// and caches it. Both paths end with a real file whose digest waldo can verify
// after upload; neither downloads anything at run time, because a tool that
// exists to touch nothing on a target should not be fetching executables over
// the network to put there.
func LocateHelperBinary(ctx context.Context, goos, goarch string) (string, error) {
	name := fmt.Sprintf("waldo-helper-%s-%s", goos, goarch)

	if explicit := os.Getenv(HelperBinaryEnv); explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", fmt.Errorf("%s is set to %s, which cannot be read: %w", HelperBinaryEnv, explicit, err)
		}
		return explicit, nil
	}

	var candidates []string
	if self, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(self); err == nil {
			self = resolved
		}
		dir := filepath.Dir(self)
		candidates = append(candidates, filepath.Join(dir, name))
		if goos == runtime.GOOS && goarch == runtime.GOARCH {
			candidates = append(candidates, filepath.Join(dir, "waldo-helper"))
		}
	}
	cacheDir, cacheErr := helperCacheDir()
	if cacheErr == nil {
		candidates = append(candidates, filepath.Join(cacheDir, name))
	}
	for _, c := range candidates {
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
			return c, nil
		}
	}

	if cacheErr != nil {
		return "", cacheErr
	}
	built, err := buildHelper(ctx, goos, goarch, filepath.Join(cacheDir, name))
	if err != nil {
		return "", fmt.Errorf(
			"no helper binary for %s/%s.\n"+
				"Release archives ship one beside the waldo binary; from a source checkout waldo\n"+
				"builds one with the local Go toolchain, which failed here: %w\n"+
				"Set %s to a binary you built yourself, or use --fileops=pipe, which installs nothing",
			goos, goarch, err, HelperBinaryEnv)
	}
	return built, nil
}

func helperCacheDir() (string, error) {
	base := os.Getenv("WALDO_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("locate home directory: %w", err)
		}
		base = filepath.Join(home, ".waldo")
	}
	dir := filepath.Join(base, "helper")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	return dir, nil
}

// buildHelper cross-compiles the helper for a target platform.
//
// The build is deliberately hermetic: -trimpath so the binary carries no local
// paths, CGO off so it is static and runs on a target with a different libc,
// and the version stamped in so the installed copy can identify itself.
func buildHelper(ctx context.Context, goos, goarch, out string) (string, error) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		return "", fmt.Errorf("no Go toolchain on PATH")
	}
	root, err := moduleRoot()
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, goBin, "build",
		"-trimpath",
		"-ldflags", "-s -w -X main.version="+waldo.Version,
		"-o", out,
		"./cmd/waldo-helper")
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"GOOS="+goos, "GOARCH="+goarch, "CGO_ENABLED=0")
	if outBytes, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(string(outBytes)))
	}
	return out, nil
}

// moduleRoot finds a waldo source checkout to build the helper from.
//
// It searches upward from the waldo binary and from the working directory,
// because both are plausible: a developer running ./waldo from the checkout,
// and one who installed it but is standing in the source tree. A release binary
// finds neither, which is correct — it ships the agent builds instead.
func moduleRoot() (string, error) {
	var starts []string
	if self, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(self); err == nil {
			self = resolved
		}
		starts = append(starts, filepath.Dir(self))
	}
	if cwd, err := os.Getwd(); err == nil {
		starts = append(starts, cwd)
	}
	for _, start := range starts {
		for dir := start; ; {
			data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
			if err == nil && strings.Contains(string(data), "module github.com/bojieli/waldo") {
				return dir, nil
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	return "", fmt.Errorf("no waldo source checkout found to build the helper from")
}

// platformOf maps `uname -sm` to a Go platform pair.
//
// An unrecognised platform is an error rather than a guess: waldo would be
// uploading an executable to someone else's machine, and the failure mode of
// guessing wrong is an unrunnable binary left behind on a host that was
// supposed to stay untouched.
func platformOf(uname string) (string, string, error) {
	fields := strings.Fields(uname)
	if len(fields) < 2 {
		return "", "", fmt.Errorf("cannot read the target's platform from %q; the helper tier needs to know which binary to install", uname)
	}
	var goos string
	switch strings.ToLower(fields[0]) {
	case "linux":
		goos = "linux"
	case "darwin":
		goos = "darwin"
	case "freebsd":
		goos = "freebsd"
	case "openbsd":
		goos = "openbsd"
	case "netbsd":
		goos = "netbsd"
	default:
		return "", "", fmt.Errorf("target OS %q has no waldo helper build", fields[0])
	}
	var goarch string
	switch strings.ToLower(fields[1]) {
	case "x86_64", "amd64":
		goarch = "amd64"
	case "aarch64", "arm64":
		goarch = "arm64"
	case "armv7l", "armv6l", "arm":
		goarch = "arm"
	case "i386", "i686":
		goarch = "386"
	case "riscv64":
		goarch = "riscv64"
	case "ppc64le":
		goarch = "ppc64le"
	case "s390x":
		goarch = "s390x"
	default:
		return "", "", fmt.Errorf("target architecture %q has no waldo helper build", fields[1])
	}
	return goos, goarch, nil
}
