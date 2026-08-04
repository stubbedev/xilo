package push

// Pushes that serialize the NAR in-process (the default). The rest of the e2e
// suite hands the client canned bytes behind a fake nix-store, so these use real
// directories on disk instead — including, where nix is installed, a real store
// path pushed to the real server.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stubbedev/xilo/internal/chunk"
	"github.com/stubbedev/xilo/internal/nar"
	"github.com/stubbedev/xilo/internal/narinfo"
)

// realTree builds a directory to push and returns its path plus the NAR nix
// would produce for it. The basename carries a store-hash-shaped prefix, which
// is all the client needs to derive a store hash.
func realTree(t *testing.T, storeHash string, fileSize int) (path string, narBytes []byte) {
	t.Helper()
	root := filepath.Join(t.TempDir(), storeHash+"-tree")
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin", "prog"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Pseudo-random so FastCDC finds boundaries inside it.
	if err := os.WriteFile(filepath.Join(root, "data"), randBytes(fileSize), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "empty"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("bin/prog", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := nar.Dump(&buf, root); err != nil {
		t.Fatal(err)
	}
	return root, buf.Bytes()
}

// fakeNixInfo installs only a fake `nix path-info` (no nix-store), so the client
// must serialize the NAR itself. Real trees, real bytes, fake metadata.
func fakeNixInfo(t *testing.T, infos []pathInfo) {
	t.Helper()
	dir := t.TempDir()
	body, err := json.Marshal(infos)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pathinfo.json"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "nix"),
		[]byte("#!/bin/sh\nexec cat \""+filepath.Join(dir, "pathinfo.json")+"\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A nix-store that always fails: nothing here may shell out to dump.
	if err := os.WriteFile(filepath.Join(dir, "nix-store"),
		[]byte("#!/bin/sh\necho 'nix-store must not be called' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XILO_CACHE_DIR", filepath.Join(dir, "state"))
	t.Setenv("XILO_EXTERNAL_DUMP", "")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func narHashOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + narinfo.Base32Encode(sum[:])
}

// The whole point: a chunked push with no nix-store subprocess at all, and the
// bytes the server receives reassemble to exactly nix's NAR.
func TestPushRealTreeInProcessDump(t *testing.T) {
	path, narBytes := realTree(t, testStoreHash, 32<<10)
	fakeNixInfo(t, []pathInfo{{Path: path, NarHash: narHashOf(narBytes), NarSize: uint64(len(narBytes))}})
	f := newFakeServer(t, baseCfg())

	if err := newTestClient(f, "", 0).Push(context.Background(), []string{path}); err != nil {
		t.Fatal(err)
	}
	if len(f.paths) != 1 {
		t.Fatalf("paths = %d, want 1", len(f.paths))
	}
	// Reassemble what was uploaded, in the order put-path declared.
	var got []byte
	for _, h := range f.paths[0].Chunks {
		body, ok := f.chunks[h]
		if !ok {
			t.Fatalf("chunk %s referenced but never uploaded", h)
		}
		got = append(got, body...)
	}
	if !bytes.Equal(got, narBytes) {
		t.Fatalf("reassembled NAR differs: %d bytes vs %d", len(got), len(narBytes))
	}
	if f.paths[0].NarHash != narHashOf(narBytes) {
		t.Fatalf("NarHash = %s", f.paths[0].NarHash)
	}
	// Chunk boundaries must match what an independent chunking of the same NAR
	// produces, or dedup across clients would break.
	var want []string
	chunk.SplitHashes(bytes.NewReader(narBytes), testParams(), func(h string) error {
		want = append(want, h)
		return nil
	})
	if strings.Join(f.paths[0].Chunks, ",") != strings.Join(want, ",") {
		t.Fatalf("chunk list differs from an independent chunking of the same NAR")
	}
}

// Below the threshold the NAR is one chunk, still serialized in-process.
func TestPushRealTreeWholeNar(t *testing.T) {
	path, narBytes := realTree(t, testStoreHash, 64)
	cfg := baseCfg()
	cfg.NarThreshold = 1 << 20 // force the whole-NAR path
	fakeNixInfo(t, []pathInfo{{Path: path, NarHash: narHashOf(narBytes), NarSize: uint64(len(narBytes))}})
	f := newFakeServer(t, cfg)

	if err := newTestClient(f, "", 0).Push(context.Background(), []string{path}); err != nil {
		t.Fatal(err)
	}
	want := chunk.Hash(narBytes)
	if got, ok := f.chunks[want]; !ok || !bytes.Equal(got, narBytes) {
		t.Fatalf("whole NAR not uploaded intact (have %d chunks)", len(f.chunks))
	}
	if len(f.paths) != 1 || len(f.paths[0].Chunks) != 1 || f.paths[0].Chunks[0] != want {
		t.Fatalf("bad path registration: %+v", f.paths)
	}
}

// If our serialization disagrees with the NAR hash nix recorded, the push must
// not go out under that hash: it re-dumps with nix instead. Here nix's dumper is
// a script producing the "authoritative" bytes, so the push must succeed with
// those, and must not have uploaded our own version.
func TestPushFallsBackToNixWhenHashDisagrees(t *testing.T) {
	path, ourNar := realTree(t, testStoreHash, 4<<10)

	// The recorded hash belongs to *different* bytes, which only nix's dumper
	// can produce (a rebuilt path, a serializer bug — the class of thing this
	// guard exists for).
	authoritative := append(append([]byte{}, ourNar...), []byte("EXTRA")...)
	dir := t.TempDir()
	narFile := filepath.Join(dir, "authoritative.nar")
	if err := os.WriteFile(narFile, authoritative, 0o644); err != nil {
		t.Fatal(err)
	}
	infos := []pathInfo{{Path: path, NarHash: narHashOf(authoritative), NarSize: uint64(len(authoritative))}}
	body, _ := json.Marshal(infos)
	if err := os.WriteFile(filepath.Join(dir, "pathinfo.json"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	write := func(name, script string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write("nix", `exec cat "`+filepath.Join(dir, "pathinfo.json")+`"`)
	write("nix-store", `echo dumped >> "`+filepath.Join(dir, "dumps")+`"
exec cat "`+narFile+`"`)
	t.Setenv("XILO_CACHE_DIR", filepath.Join(dir, "state"))
	t.Setenv("XILO_EXTERNAL_DUMP", "")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	f := newFakeServer(t, baseCfg())
	if err := newTestClient(f, "", 0).Push(context.Background(), []string{path}); err != nil {
		t.Fatal(err)
	}
	// nix's dumper must have been consulted exactly once, after our attempt.
	dumps, err := os.ReadFile(filepath.Join(dir, "dumps"))
	if err != nil {
		t.Fatalf("nix-store --dump was never used as the fallback: %v", err)
	}
	if n := strings.Count(string(dumps), "dumped"); n != 1 {
		t.Fatalf("nix-store --dump ran %d times, want 1", n)
	}
	if len(f.paths) != 1 {
		t.Fatalf("paths = %d, want 1", len(f.paths))
	}
	var got []byte
	for _, h := range f.paths[0].Chunks {
		got = append(got, f.chunks[h]...)
	}
	if !bytes.Equal(got, authoritative) {
		t.Fatal("registered path does not carry nix's bytes")
	}
	if f.paths[0].NarHash != narHashOf(authoritative) {
		t.Fatal("wrong NarHash registered")
	}
}

// XILO_EXTERNAL_DUMP=1 must bypass the in-process serializer entirely.
func TestExternalDumpEnvForcesNixStore(t *testing.T) {
	path, narBytes := realTree(t, testStoreHash, 2<<10)
	dir := t.TempDir()
	narFile := filepath.Join(dir, "n.nar")
	if err := os.WriteFile(narFile, narBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal([]pathInfo{{Path: path, NarHash: narHashOf(narBytes), NarSize: uint64(len(narBytes))}})
	if err := os.WriteFile(filepath.Join(dir, "pathinfo.json"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	for name, script := range map[string]string{
		"nix":       `exec cat "` + filepath.Join(dir, "pathinfo.json") + `"`,
		"nix-store": "echo dumped >> \"" + filepath.Join(dir, "dumps") + "\"\nexec cat \"" + narFile + "\"",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("XILO_CACHE_DIR", filepath.Join(dir, "state"))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("XILO_EXTERNAL_DUMP", "1")

	f := newFakeServer(t, baseCfg())
	if err := newTestClient(f, "", 0).Push(context.Background(), []string{path}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "dumps")); err != nil {
		t.Fatal("XILO_EXTERNAL_DUMP=1 did not use nix-store --dump")
	}
}

// The strongest end-to-end check available: a real store path, serialized by us,
// verified by the real server (which recomputes the NAR hash from the chunks it
// received), then pulled back and compared with nix's own dump.
func TestPushRealStorePathAgainstRealServer(t *testing.T) {
	if _, err := exec.LookPath("nix"); err != nil {
		t.Skip("nix not on PATH")
	}
	storePath := findSmallStorePath(t)
	if storePath == "" {
		t.Skip("no small reference-free store path found")
	}
	want, err := exec.Command("nix-store", "--dump", storePath).Output()
	if err != nil {
		t.Skipf("nix-store --dump %s: %v", storePath, err)
	}

	db, ts := realServer(t)
	if _, err := db.CreateCache("default", "one", true, 40); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XILO_CACHE_DIR", filepath.Join(t.TempDir(), "state"))
	t.Setenv("XILO_EXTERNAL_DUMP", "")

	c := NewClient(ts.URL, "default/one", "", 0)
	c.Quiet = true
	if err := c.Push(context.Background(), []string{storePath}); err != nil {
		t.Fatalf("push %s: %v", storePath, err)
	}

	storeHash := narinfo.StoreHash(storePath)
	resp, err := http.Get(ts.URL + "/c/default/one/nar/" + storeHash + ".nar")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("nar: %d", resp.StatusCode)
	}
	gs, ws := sha256.Sum256(got), sha256.Sum256(want)
	if hex.EncodeToString(gs[:]) != hex.EncodeToString(ws[:]) {
		t.Fatalf("served NAR (%d bytes) differs from nix-store --dump (%d bytes)", len(got), len(want))
	}
}

// findSmallStorePath looks for a store path that is small and has no references,
// so pushing it stays a one-path closure. Returns "" if none turns up.
func findSmallStorePath(t *testing.T) string {
	t.Helper()
	f, err := os.Open("/nix/store")
	if err != nil {
		return ""
	}
	defer f.Close()
	names, err := f.Readdirnames(400)
	if err != nil {
		return ""
	}
	for _, name := range names {
		if !narinfo.ValidStoreName(name) || strings.HasSuffix(name, ".drv") {
			continue
		}
		path := "/nix/store/" + name
		out, err := exec.Command("nix", "path-info", "--json", path).Output()
		if err != nil {
			continue
		}
		infos, err := parsePathInfo(out)
		if err != nil || len(infos) != 1 {
			continue
		}
		in := infos[0]
		// References to itself are fine (nix lists self-references).
		refs := 0
		for _, r := range in.References {
			if r != path {
				refs++
			}
		}
		if refs == 0 && in.NarSize > 0 && in.NarSize < 4<<20 {
			return path
		}
	}
	return ""
}
