package storage

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestChunkKey(t *testing.T) {
	cases := map[string]string{
		"abcd1234": "chunk/ab/abcd1234",
		"a":        "chunk/a",
		"":         "chunk/",
	}
	for in, want := range cases {
		if got := ChunkKey(in); got != want {
			t.Errorf("ChunkKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLocalRoundTrip(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	l, err := NewLocal(root)
	if err != nil {
		t.Fatal(err)
	}

	key := ChunkKey("abcd1234")
	data := []byte("hello world")
	if err := l.Put(ctx, key, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}

	got := mustGet(t, l, ctx, key)
	if !bytes.Equal(got, data) {
		t.Fatalf("Get = %q, want %q", got, data)
	}

	// sharded path created
	if _, err := os.Stat(filepath.Join(root, "chunk", "ab", "abcd1234")); err != nil {
		t.Fatalf("sharded path missing: %v", err)
	}

	// no leftover temp file after a successful Put
	assertNoTmp(t, filepath.Join(root, "chunk", "ab"))

	// overwrite same key -> new bytes
	data2 := []byte("replaced")
	if err := l.Put(ctx, key, bytes.NewReader(data2)); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, l, ctx, key); !bytes.Equal(got, data2) {
		t.Fatalf("after overwrite Get = %q, want %q", got, data2)
	}

	// Has true
	if ok, err := l.Has(ctx, key); err != nil || !ok {
		t.Fatalf("Has(existing) = %v, %v", ok, err)
	}
	// Has false
	if ok, err := l.Has(ctx, ChunkKey("ffffffff")); err != nil || ok {
		t.Fatalf("Has(missing) = %v, %v", ok, err)
	}

	// Get missing -> error
	if _, err := l.Get(ctx, ChunkKey("ffffffff")); err == nil {
		t.Fatal("Get(missing) should error")
	}

	// Delete missing -> nil
	if err := l.Delete(ctx, ChunkKey("ffffffff")); err != nil {
		t.Fatalf("Delete(missing) = %v, want nil", err)
	}

	// Delete existing then Has false
	if err := l.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	if ok, _ := l.Has(ctx, key); ok {
		t.Fatal("key still present after Delete")
	}
}

func TestNewLocalEmptyRoot(t *testing.T) {
	if _, err := NewLocal(""); err == nil {
		t.Fatal("NewLocal(\"\") should error")
	}
}

func mustGet(t *testing.T, l *Local, ctx context.Context, key string) []byte {
	t.Helper()
	rc, err := l.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func assertNoTmp(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Fatalf("leftover temp file: %s", e.Name())
		}
	}
}
