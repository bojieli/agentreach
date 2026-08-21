package main

import (
	"os"
	"path/filepath"
	"strings"
)

// Environment handling, which is one of the two places reach's Unix assumptions
// broke on Windows badly enough to be a safety problem rather than a bug.
//
// Windows spells the search path `Path`, not `PATH`, and its environment is
// case-insensitive. Code that matches the key exactly — which is correct on
// Unix — finds nothing on Windows, so prependPath would *append a second*
// PATH variable rather than modifying the real one. The harness would then be
// launched with reach's shim directory absent from its search path, find the
// genuine `bash`, and run the model's commands on the operator's own machine
// while the agent believed it was working on the target.
//
// That is this project's worst failure mode, reached by a string comparison.

// pathEnvValue returns the current search path, however it is spelled.
func pathEnvValue() string {
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok && isPathEnvKey(k) {
			return v
		}
	}
	return ""
}

// setPathEnv replaces the search path in an environment block.
//
// The existing key's spelling is preserved rather than normalised: adding a
// second variable that differs only in case would leave Windows to choose
// between them, and which one a child process sees is not something reach
// should be leaving to chance.
func setPathEnv(env []string, value string) []string {
	out := make([]string, 0, len(env)+1)
	replaced := false
	for _, kv := range env {
		if k, _, ok := strings.Cut(kv, "="); ok && isPathEnvKey(k) {
			out = append(out, k+"="+value)
			replaced = true
			continue
		}
		out = append(out, kv)
	}
	if !replaced {
		out = append(out, "PATH="+value)
	}
	return out
}

// prependPath puts dir at the front of the search path in an environment block.
func prependPath(env []string, dir string) []string {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && isPathEnvKey(k) {
			return setPathEnv(env, dir+string(filepath.ListSeparator)+v)
		}
	}
	return append(env, "PATH="+dir)
}
