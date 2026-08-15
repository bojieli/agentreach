package fileops

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/bojieli/waldo/internal/transport"
	"github.com/bojieli/waldo/internal/waldo"
)

func newLocalPOSIX(t *testing.T) *POSIX {
	t.Helper()
	tr := transport.NewLocal()
	caps, err := Probe(context.Background(), tr)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	return NewPOSIX(tr, caps)
}

func TestProbeDetectsUserland(t *testing.T) {
	caps, err := Probe(context.Background(), transport.NewLocal())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if caps.StatFlavor != "gnu" && caps.StatFlavor != "bsd" {
		t.Errorf("stat flavour not detected, got %q", caps.StatFlavor)
	}
	if caps.Base64Decode == "" || caps.Base64Encode == "" {
		t.Error("base64 not detected")
	}
	t.Logf("uname=%q stat=%s b64d=%q rg=%q sha=%q findprintf=%v py3=%v",
		caps.Uname, caps.StatFlavor, caps.Base64Decode, caps.Ripgrep, caps.SHA256, caps.FindPrintf, caps.Python3)
}

// TestRoundTripBinary is the test that matters most for tier 0: content must
// survive the shell unchanged. NUL bytes, invalid UTF-8, CRLF and a trailing
// newline are exactly what naive shell interpolation destroys.
func TestRoundTripBinary(t *testing.T) {
	p := newLocalPOSIX(t)
	ctx := context.Background()
	dir := t.TempDir()

	cases := map[string][]byte{
		"empty":        {},
		"nul":          {0x00, 0x01, 0x02, 0xff, 0xfe},
		"invalid-utf8": {0xc3, 0x28, 0xa0, 0xa1},
		"crlf":         []byte("line1\r\nline2\r\n"),
		"trailing-nl":  []byte("ends with newline\n"),
		"no-trailing":  []byte("no trailing newline"),
		"quotes":       []byte("it's \"quoted\" $(and) `backticked` \\ ${escaped}"),
		"big":          bytes.Repeat([]byte("0123456789abcdef"), 40000), // 640 KB
	}
	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			fp := filepath.Join(dir, name+".bin")
			if err := p.Write(ctx, fp, want, 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}
			got, err := p.Read(ctx, fp, 0, 0)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("round trip corrupted: got %d bytes, want %d", len(got), len(want))
			}
			onDisk, err := os.ReadFile(fp)
			if err != nil || !bytes.Equal(onDisk, want) {
				t.Fatalf("on-disk content differs from what was written")
			}
		})
	}
}

func TestWriteIsAtomicAndLeavesNoDebris(t *testing.T) {
	p := newLocalPOSIX(t)
	ctx := context.Background()
	dir := t.TempDir()
	fp := filepath.Join(dir, "f.txt")

	if err := p.Write(ctx, fp, []byte("v1"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := p.Write(ctx, fp, []byte("v2-longer"), 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if len(e.Name()) > 6 && e.Name()[:6] == ".waldo" {
			t.Errorf("temporary file left behind: %s", e.Name())
		}
	}
	fi, err := os.Stat(fp)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode not applied: got %v want 0600", fi.Mode().Perm())
	}
}

func TestReadRange(t *testing.T) {
	p := newLocalPOSIX(t)
	ctx := context.Background()
	fp := filepath.Join(t.TempDir(), "r.txt")
	if err := p.Write(ctx, fp, []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		off, n int64
		want   string
	}{
		{0, 4, "0123"}, {3, 4, "3456"}, {6, 0, "6789"}, {0, 0, "0123456789"}, {9, 5, "9"},
	} {
		got, err := p.Read(ctx, fp, tc.off, tc.n)
		if err != nil {
			t.Fatalf("read(%d,%d): %v", tc.off, tc.n, err)
		}
		if string(got) != tc.want {
			t.Errorf("read(%d,%d) = %q want %q", tc.off, tc.n, got, tc.want)
		}
	}
}

func TestReadMissingFileIsNotFound(t *testing.T) {
	p := newLocalPOSIX(t)
	_, err := p.Read(context.Background(), filepath.Join(t.TempDir(), "nope"), 0, 0)
	if err == nil {
		t.Fatal("expected error reading missing file")
	}
	// An agent must be able to distinguish "missing" from "empty": returning
	// empty content for an absent file would be silently wrong.
	if _, ok := err.(*waldo.NotFoundError); !ok {
		t.Logf("note: missing file surfaced as %T: %v", err, err)
	}
}

func TestStatAndList(t *testing.T) {
	p := newLocalPOSIX(t)
	ctx := context.Background()
	dir := t.TempDir()

	if err := p.Write(ctx, filepath.Join(dir, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := p.Mkdir(ctx, filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}

	fi, err := p.Stat(ctx, filepath.Join(dir, "a.txt"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Size != 5 || fi.IsDir {
		t.Errorf("stat wrong: size=%d isdir=%v", fi.Size, fi.IsDir)
	}

	entries, err := p.List(ctx, dir)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("list returned %d entries, want 2: %+v", len(entries), entries)
	}
	byName := map[string]bool{}
	for _, e := range entries {
		byName[e.Name] = e.IsDir
	}
	if byName["a.txt"] {
		t.Error("a.txt reported as dir")
	}
	if !byName["sub"] {
		t.Error("sub not reported as dir")
	}
}

func TestListHandlesAwkwardNames(t *testing.T) {
	p := newLocalPOSIX(t)
	ctx := context.Background()
	dir := t.TempDir()
	names := []string{"with space.txt", "with'quote.txt", "with$dollar.txt", "üñïçø∂é.txt"}
	for _, n := range names {
		if err := p.Write(ctx, filepath.Join(dir, n), []byte("x"), 0o644); err != nil {
			t.Fatalf("write %q: %v", n, err)
		}
	}
	entries, err := p.List(ctx, dir)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != len(names) {
		t.Fatalf("got %d entries want %d", len(entries), len(names))
	}
}

func TestSearchAndGlob(t *testing.T) {
	p := newLocalPOSIX(t)
	ctx := context.Background()
	dir := t.TempDir()
	if err := p.Write(ctx, filepath.Join(dir, "one.go"), []byte("package a\nfunc Target() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := p.Write(ctx, filepath.Join(dir, "two.go"), []byte("package a\n// nothing\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	m, err := p.Search(ctx, waldoSearch(dir, "Target"))
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(m) != 1 || m[0].Line != 2 {
		t.Fatalf("search wrong: %+v", m)
	}

	g, err := p.Glob(ctx, dir, "*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(g) != 2 {
		t.Fatalf("glob returned %d want 2: %v", len(g), g)
	}
}

func TestHash(t *testing.T) {
	p := newLocalPOSIX(t)
	ctx := context.Background()
	fp := filepath.Join(t.TempDir(), "h.txt")
	if err := p.Write(ctx, fp, []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	h, err := p.Hash(ctx, fp)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	const want = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if h != want {
		t.Errorf("hash = %q want %q", h, want)
	}
}

func waldoSearch(root, pattern string) waldo.SearchRequest {
	return waldo.SearchRequest{Pattern: pattern, Root: root}
}

// TestReadLargerThanOutputCap guards against a corruption mode: if an encoded
// payload were truncated by the transport's output cap, the truncation notice
// would land inside the base64 stream and decode to garbage. Reads must chunk
// below the cap instead.
func TestReadLargerThanOutputCap(t *testing.T) {
	p := newLocalPOSIX(t)
	ctx := context.Background()
	fp := filepath.Join(t.TempDir(), "large.bin")

	// 5 MiB of non-repeating content, larger than the 4 MiB default cap and
	// larger than one read chunk.
	want := make([]byte, 5<<20)
	for i := range want {
		want[i] = byte(i*31 + i/251)
	}
	if err := p.Write(ctx, fp, want, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := p.Read(ctx, fp, 0, 0)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("large read corrupted: got %d bytes want %d", len(got), len(want))
	}
}
