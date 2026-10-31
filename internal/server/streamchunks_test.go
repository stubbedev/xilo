package server

import (
	"bytes"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func TestChunkBufSizedToClass(t *testing.T) {
	cases := []struct{ hint, cap int64 }{
		{0, 4 << 10},
		{1, 4 << 10},
		{4 << 10, 4 << 10},
		{4<<10 + 1, 8 << 10},
		{256<<10 + 1, 512 << 10},
		{1 << 20, 1 << 20},
		{4 << 20, 4 << 20},
		{4<<20 + 1, 4<<20 + 1},
	}
	for _, c := range cases {
		b := getChunkBuf(c.hint)
		if int64(cap(b)) != c.cap || len(b) != 0 {
			t.Errorf("getChunkBuf(%d): len %d cap %d, want len 0 cap %d", c.hint, len(b), cap(b), c.cap)
		}
		putChunkBuf(b)
	}
}

func TestChunkBufPoolKeepsClassesApart(t *testing.T) {
	for range 100 {
		putChunkBuf(make([]byte, 0, 1<<20))
	}
	if b := getChunkBuf(100); cap(b) != 4<<10 {
		t.Fatalf("a 100-byte request got a %d-byte buffer: the 1 MiB class leaked into the smallest one", cap(b))
	}
	putChunkBuf(make([]byte, 0, 5000))
	for range 100 {
		if b := getChunkBuf(8 << 10); cap(b) != 8<<10 {
			t.Fatalf("an 8 KiB request got cap %d: a buffer of no class size was pooled", cap(b))
		}
	}
}

func TestDecodedSizeHint(t *testing.T) {
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	frame := enc.EncodeAll(bytes.Repeat([]byte("x"), 300<<10), nil)
	if got := decodedSizeHint(frame, 1<<30); got != 300<<10 {
		t.Fatalf("hint = %d, want the declared %d", got, 300<<10)
	}
	if got := decodedSizeHint(frame, 64<<10); got != 64<<10 {
		t.Fatalf("hint = %d, want it clamped to %d", got, 64<<10)
	}
	if got := decodedSizeHint([]byte("not zstd"), 1<<30); got != 0 {
		t.Fatalf("hint for garbage = %d, want 0", got)
	}
}
