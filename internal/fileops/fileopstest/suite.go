// Package fileopstest is the conformance suite every file-operation tier must
// pass.
//
// reach has three ways to touch a file on a target and they share almost no
// code: a shell pipeline, a Python handler, and a Go binary.
// A user cannot tell which one is in use, and must not need to — the whole
// design rests on the claim that they are interchangeable. That claim is only
// worth anything if it is tested, so every tier runs this identical suite and a
// tier that cannot pass it does not ship.
//
// The cases are chosen from the things that actually break when file content is
// moved through a shell: NUL bytes, invalid UTF-8, CRLF, trailing newlines that
// a `$(...)` capture would eat, empty files, filenames with spaces and quotes,
// and payloads large enough to cross whatever chunking the tier does.
package fileopstest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"strings"
	"sync"
	"testing"

	"github.com/bojieli/agentreach/internal/fileops"
	"github.com/bojieli/agentreach/internal/reach"
)

// Factory builds the strategy under test, rooted at a directory the suite may
// freely create and destroy files in.
type Factory func(t *testing.T) fileops.FileOps

// Run executes the whole suite against one tier.
//
// root must be an absolute path on the target that already exists and is
// writable. The suite creates everything else it needs beneath it.
func Run(t *testing.T, root string, newOps Factory) {
	t.Helper()
	for _, tc := range []struct {
		name string
		fn   func(*testing.T, context.Context, fileops.FileOps, string)
	}{
		{"RoundTripsAwkwardContent", testAwkwardContent},
		{"RoundTripsLargePayload", testLargePayload},
		{"ReadsRanges", testRanges},
		{"ReportsMissingFilesAsNotFound", testNotFound},
		{"StatsFilesAndDirectories", testStat},
		{"ListsDirectories", testList},
		{"CreatesNestedDirectories", testMkdir},
		{"RemovesFilesAndTrees", testRemove},
		{"RenamesPaths", testRename},
		{"HashesContentOnTheTarget", testHash},
		{"OverwritesPreservingNothingStale", testOverwrite},
		{"HandlesAwkwardFilenames", testAwkwardNames},
		{"SearchesAndGlobsOnTheTarget", testSearchGlob},
		{"SurvivesConcurrentWrites", testConcurrentWrites},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ops := newOps(t)
			ctx := context.Background()
			dir := path.Join(root, "conf-"+strings.ToLower(tc.name))
			if err := ops.Mkdir(ctx, dir, 0o755); err != nil {
				t.Fatalf("mkdir %s: %v", dir, err)
			}
			t.Cleanup(func() { _ = ops.Remove(context.Background(), dir, true) })
			tc.fn(t, ctx, ops, dir)
		})
	}
}

// payloads are the content shapes that break naive implementations.
func payloads() map[string][]byte {
	return map[string][]byte{
		"empty":            {},
		"plain":            []byte("hello world\n"),
		"no-trailing-nl":   []byte("no newline at the end"),
		"nul-bytes":        {0x00, 0x01, 0x00, 'a', 0x00},
		"invalid-utf8":     {0xff, 0xfe, 0xfd, 0x80, 0x41},
		"crlf":             []byte("line one\r\nline two\r\n"),
		"unicode":          []byte("héllo — 世界 🌍\n"),
		"only-newlines":    []byte("\n\n\n\n"),
		"shell-metachars":  []byte("$(id) `hostname` ${HOME} \\ ' \" | & ; > <\n"),
		"long-single-line": bytes.Repeat([]byte("x"), 200_000),
		"all-byte-values":  allBytes(),
	}
}

func allBytes() []byte {
	b := make([]byte, 256)
	for i := range b {
		b[i] = byte(i)
	}
	return b
}

func testAwkwardContent(t *testing.T, ctx context.Context, ops fileops.FileOps, dir string) {
	for name, want := range payloads() {
		p := path.Join(dir, name)
		if err := ops.Write(ctx, p, want, 0o644); err != nil {
			t.Fatalf("%s: write: %v", name, err)
		}
		got, err := ops.Read(ctx, p, 0, 0)
		if err != nil {
			t.Fatalf("%s: read: %v", name, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s: round trip corrupted content\n got %d bytes %q\nwant %d bytes %q",
				name, len(got), clip(got), len(want), clip(want))
		}
	}
}

func testLargePayload(t *testing.T, ctx context.Context, ops fileops.FileOps, dir string) {
	// Larger than every tier's internal chunk size, so chunk-stitching bugs —
	// which produce plausible-looking but wrong content — cannot hide.
	want := make([]byte, 5<<20)
	for i := range want {
		want[i] = byte(i * 7 % 251)
	}
	p := path.Join(dir, "large.bin")
	if err := ops.Write(ctx, p, want, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := ops.Read(ctx, p, 0, 0)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("read %d bytes, want %d", len(got), len(want))
	}
	if !bytes.Equal(got, want) {
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("content differs at byte %d: got %#x want %#x", i, got[i], want[i])
			}
		}
	}
}

func testRanges(t *testing.T, ctx context.Context, ops fileops.FileOps, dir string) {
	content := []byte("0123456789abcdefghij")
	p := path.Join(dir, "ranges")
	if err := ops.Write(ctx, p, content, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	for _, tc := range []struct {
		off, n int64
		want   string
	}{
		{0, 5, "01234"},
		{5, 5, "56789"},
		{10, 0, "abcdefghij"},
		{0, 0, "0123456789abcdefghij"},
		{18, 10, "ij"}, // asking past the end returns what exists
		{100, 5, ""},   // starting past the end is empty, not an error
	} {
		got, err := ops.Read(ctx, p, tc.off, tc.n)
		if err != nil {
			t.Fatalf("read(off=%d n=%d): %v", tc.off, tc.n, err)
		}
		if string(got) != tc.want {
			t.Errorf("read(off=%d n=%d) = %q, want %q", tc.off, tc.n, got, tc.want)
		}
	}
}

// testNotFound pins down the distinction that matters most to an agent: a file
// that is not there must be reported as absent, never as empty. An agent told a
// file is empty concludes the code it is looking for does not exist.
func testNotFound(t *testing.T, ctx context.Context, ops fileops.FileOps, dir string) {
	_, err := ops.Read(ctx, path.Join(dir, "does-not-exist"), 0, 0)
	if err == nil {
		t.Fatal("reading a missing file succeeded; it must fail as not-found")
	}
	var nf *reach.NotFoundError
	if !errors.As(err, &nf) {
		t.Errorf("reading a missing file gave %T (%v), want *reach.NotFoundError", err, err)
	}
	if _, err := ops.Stat(ctx, path.Join(dir, "does-not-exist")); err == nil {
		t.Error("stat of a missing file succeeded")
	}
}

func testStat(t *testing.T, ctx context.Context, ops fileops.FileOps, dir string) {
	content := []byte("abcdef")
	p := path.Join(dir, "statme")
	if err := ops.Write(ctx, p, content, 0o640); err != nil {
		t.Fatalf("write: %v", err)
	}
	fi, err := ops.Stat(ctx, p)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Size != int64(len(content)) {
		t.Errorf("size = %d, want %d", fi.Size, len(content))
	}
	if fi.IsDir {
		t.Error("a regular file was reported as a directory")
	}
	if fi.Mode.Perm() != 0o640 {
		t.Errorf("mode = %v, want -rw-r-----", fi.Mode.Perm())
	}
	di, err := ops.Stat(ctx, dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if !di.IsDir {
		t.Error("a directory was not reported as one")
	}
}

func testList(t *testing.T, ctx context.Context, ops fileops.FileOps, dir string) {
	if err := ops.Write(ctx, path.Join(dir, "a.txt"), []byte("aaa"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ops.Write(ctx, path.Join(dir, "b.txt"), []byte("bbbb"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ops.Mkdir(ctx, path.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	entries, err := ops.List(ctx, dir)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	found := map[string]reach.FileInfo{}
	for _, e := range entries {
		found[e.Name] = e
	}
	if len(found) != 3 {
		t.Errorf("listed %d entries (%v), want 3", len(found), names(entries))
	}
	if e, ok := found["a.txt"]; !ok {
		t.Error("a.txt missing from the listing")
	} else if e.Size != 3 {
		t.Errorf("a.txt size = %d, want 3", e.Size)
	}
	if e, ok := found["sub"]; !ok {
		t.Error("sub missing from the listing")
	} else if !e.IsDir {
		t.Error("sub was not reported as a directory")
	}
}

func testMkdir(t *testing.T, ctx context.Context, ops fileops.FileOps, dir string) {
	nested := path.Join(dir, "one", "two", "three")
	if err := ops.Mkdir(ctx, nested, 0o755); err != nil {
		t.Fatalf("mkdir -p: %v", err)
	}
	fi, err := ops.Stat(ctx, nested)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !fi.IsDir {
		t.Error("created path is not a directory")
	}
	// Creating it again must succeed: two sessions doing the same thing is
	// ordinary, and an error here would look like a permissions problem.
	if err := ops.Mkdir(ctx, nested, 0o755); err != nil {
		t.Errorf("mkdir of an existing directory failed: %v", err)
	}
}

func testRemove(t *testing.T, ctx context.Context, ops fileops.FileOps, dir string) {
	f := path.Join(dir, "gone.txt")
	if err := ops.Write(ctx, f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ops.Remove(ctx, f, false); err != nil {
		t.Fatalf("remove file: %v", err)
	}
	if _, err := ops.Stat(ctx, f); err == nil {
		t.Error("file still exists after remove")
	}

	tree := path.Join(dir, "tree")
	if err := ops.Mkdir(ctx, path.Join(tree, "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ops.Write(ctx, path.Join(tree, "inner", "deep.txt"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ops.Remove(ctx, tree, true); err != nil {
		t.Fatalf("remove tree: %v", err)
	}
	if _, err := ops.Stat(ctx, tree); err == nil {
		t.Error("tree still exists after recursive remove")
	}
}

func testRename(t *testing.T, ctx context.Context, ops fileops.FileOps, dir string) {
	from, to := path.Join(dir, "before"), path.Join(dir, "after")
	if err := ops.Write(ctx, from, []byte("moved"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ops.Rename(ctx, from, to); err != nil {
		t.Fatalf("rename: %v", err)
	}
	got, err := ops.Read(ctx, to, 0, 0)
	if err != nil {
		t.Fatalf("read renamed: %v", err)
	}
	if string(got) != "moved" {
		t.Errorf("content after rename = %q", got)
	}
	if _, err := ops.Stat(ctx, from); err == nil {
		t.Error("the original path still exists after rename")
	}
}

func testHash(t *testing.T, ctx context.Context, ops fileops.FileOps, dir string) {
	content := []byte("hash me\x00\xff")
	p := path.Join(dir, "hashme")
	if err := ops.Write(ctx, p, content, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ops.Hash(ctx, p)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	sum := sha256.Sum256(content)
	if want := hex.EncodeToString(sum[:]); got != want {
		t.Errorf("hash = %s, want %s", got, want)
	}
}

// testOverwrite covers the failure that silently loses work: a shorter write
// over a longer file must not leave the old tail behind.
func testOverwrite(t *testing.T, ctx context.Context, ops fileops.FileOps, dir string) {
	p := path.Join(dir, "overwrite")
	if err := ops.Write(ctx, p, []byte("a much longer original content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ops.Write(ctx, p, []byte("short"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ops.Read(ctx, p, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "short" {
		t.Errorf("after overwrite content = %q, want %q", got, "short")
	}
}

func testAwkwardFilenamesList() []string {
	return []string{
		"with space.txt",
		"with'quote.txt",
		`with"double.txt`,
		"with$dollar.txt",
		"with`backtick.txt",
		"with;semi.txt",
		"with|pipe.txt",
		"with*star.txt",
		"héllo-ünicode.txt",
		"with(paren).txt",
		"with&amp.txt",
		"with#hash.txt",
	}
}

func testAwkwardNames(t *testing.T, ctx context.Context, ops fileops.FileOps, dir string) {
	for _, name := range testAwkwardFilenamesList() {
		p := path.Join(dir, name)
		want := []byte("content of " + name)
		if err := ops.Write(ctx, p, want, 0o644); err != nil {
			t.Errorf("write %q: %v", name, err)
			continue
		}
		got, err := ops.Read(ctx, p, 0, 0)
		if err != nil {
			t.Errorf("read %q: %v", name, err)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%q: content = %q, want %q", name, got, want)
		}
		if _, err := ops.Stat(ctx, p); err != nil {
			t.Errorf("stat %q: %v", name, err)
		}
	}
	entries, err := ops.List(ctx, dir)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != len(testAwkwardFilenamesList()) {
		t.Errorf("listed %d of %d awkward filenames: %v",
			len(entries), len(testAwkwardFilenamesList()), names(entries))
	}
}

func testSearchGlob(t *testing.T, ctx context.Context, ops fileops.FileOps, dir string) {
	if err := ops.Write(ctx, path.Join(dir, "hay.go"), []byte("package x\nfunc Needle() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ops.Write(ctx, path.Join(dir, "other.txt"), []byte("nothing here\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	matches, err := ops.Search(ctx, reach.SearchRequest{Pattern: "Needle", Root: dir, MaxResults: 10})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("search found %d matches, want 1: %v", len(matches), matches)
	}
	if matches[0].Line != 2 {
		t.Errorf("match line = %d, want 2", matches[0].Line)
	}
	if !strings.Contains(matches[0].Text, "Needle") {
		t.Errorf("match text = %q", matches[0].Text)
	}

	paths, err := ops.Glob(ctx, dir, "*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(paths) != 1 || !strings.HasSuffix(paths[0], "hay.go") {
		t.Errorf("glob returned %v, want just hay.go", paths)
	}
}

func names(entries []reach.FileInfo) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name)
	}
	return out
}

func clip(b []byte) string {
	const max = 64
	if len(b) <= max {
		return fmt.Sprintf("%q", b)
	}
	return fmt.Sprintf("%q...(%d more)", b[:max], len(b)-max)
}

// testConcurrentWrites covers the shape a harness actually produces.
//
// Agents issue tool calls in parallel, so a tier is asked to write several
// files into one directory at once. Every tier does that by writing to a
// temporary name and renaming it into place, which makes the temporary name a
// shared namespace — and a shared namespace is where a since-removed tier
// failed: it numbered temporaries from a counter that restarted in every
// process, so concurrent writers collided and all but one were refused.
//
// This case is in the shared suite rather than in that tier's own tests
// because the mistake was not specific to it. Any tier can invent a bad
// temporary name, and every tier now has to prove it did not.
func testConcurrentWrites(t *testing.T, ctx context.Context, ops fileops.FileOps, dir string) {
	const writers = 12

	var wg sync.WaitGroup
	errs := make([]error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Distinct sizes, so a mix-up shows up as wrong content rather than
			// as coincidentally identical bytes.
			body := bytes.Repeat([]byte{byte('a' + i)}, 64+i*997)
			errs[i] = ops.Write(ctx, path.Join(dir, fmt.Sprintf("concurrent-%d", i)), body, 0o644)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent write %d failed: %v", i, err)
		}
	}

	for i := 0; i < writers; i++ {
		want := bytes.Repeat([]byte{byte('a' + i)}, 64+i*997)
		got, err := ops.Read(ctx, path.Join(dir, fmt.Sprintf("concurrent-%d", i)), 0, 0)
		if err != nil {
			t.Errorf("read back %d: %v", i, err)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("file %d: got %d bytes, want %d — concurrent writes interfered",
				i, len(got), len(want))
		}
	}

	// Nothing may be left behind. A temporary that survives is either a leaked
	// write or a collision that was silently swallowed.
	entries, err := ops.List(ctx, dir)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name, ".reach.tmp.") {
			t.Errorf("a temporary file survived the writes: %s", e.Name)
		}
	}
}
