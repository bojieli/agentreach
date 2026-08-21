package main

import (
	"runtime"
	"testing"

	"github.com/bojieli/agentreach/internal/mirror"
)

// The hook is where a path from a harness becomes a path on the target, and it
// had no tests. That is how a `filepath.Join` — correct-looking, and correct on
// Linux — survived: it produced `\srv\app\main.go` on Windows and sent it to a
// POSIX host, where a backslash is a legal filename character, so the target
// would have created one oddly-named file instead of editing the right one.

func TestResolveTargetPathIsAlwaysPOSIX(t *testing.T) {
	m := mirror.New(t.TempDir(), nil)

	for _, tc := range []struct {
		name, raw, workspace, want string
	}{
		{"absolute stays put", "/srv/app/main.go", "/srv/app", "/srv/app/main.go"},
		{"relative joins the workspace", "main.go", "/srv/app", "/srv/app/main.go"},
		{"nested relative", "internal/x/y.go", "/srv/app", "/srv/app/internal/x/y.go"},
		{"workspace with trailing slash", "main.go", "/srv/app/", "/srv/app/main.go"},
		{"dot segments are cleaned", "/srv/app/./x/../main.go", "/srv/app", "/srv/app/main.go"},
		{"root workspace", "main.go", "/", "/main.go"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveTargetPath(tc.raw, tc.workspace, m)
			if got != tc.want {
				t.Errorf("resolveTargetPath(%q, %q) = %q, want %q", tc.raw, tc.workspace, got, tc.want)
			}
			// The property that actually broke: a target path never contains a
			// separator belonging to the machine reach is running on.
			for _, r := range got {
				if r == '\\' {
					t.Fatalf("target path %q contains a backslash; the target is POSIX and this is running on %s",
						got, runtime.GOOS)
				}
			}
		})
	}
}

func TestUnderWorkspace(t *testing.T) {
	for _, tc := range []struct {
		target, workspace string
		want              bool
	}{
		{"/srv/app/main.go", "/srv/app", true},
		{"/srv/app", "/srv/app", true},
		{"/srv/app/deep/nested/file", "/srv/app", true},
		// The trap: a file whose name merely begins with "..".
		{"/srv/app/..config", "/srv/app", true},
		{"/srv/app/...hidden", "/srv/app", true},
		// Genuine escapes.
		{"/srv/other/file", "/srv/app", false},
		{"/etc/passwd", "/srv/app", false},
		{"/srv/appendix/file", "/srv/app", false}, // prefix, not a component
		{"/srv/app/../secret", "/srv/app", false},
	} {
		if got := underWorkspace(tc.target, tc.workspace); got != tc.want {
			t.Errorf("underWorkspace(%q, %q) = %v, want %v", tc.target, tc.workspace, got, tc.want)
		}
	}
}
