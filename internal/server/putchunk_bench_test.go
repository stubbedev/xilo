package server

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func BenchmarkPutChunk(b *testing.B) {
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		b.Fatal(err)
	}
	rng := rand.New(rand.NewPCG(1, 2))
	base := make([]byte, 1<<20)
	for i := range base {
		base[i] = "abcdefghijklmnop"[rng.IntN(16)]
	}
	for _, encoding := range []string{"", "zstd"} {
		b.Run("encoding="+encoding, func(b *testing.B) {
			s, db, _ := newTestServer(b, true)
			if _, err := db.CreateCache("default", "c", true, 40); err != nil {
				b.Fatal(err)
			}
			h := s.Handler()
			chunk := make([]byte, len(base))
			copy(chunk, base)
			var n uint64
			b.ReportAllocs()
			b.SetBytes(int64(len(chunk)))
			for b.Loop() {
				b.StopTimer()
				n++
				binary.LittleEndian.PutUint64(chunk, n)
				sum := sha256.Sum256(chunk)
				body := chunk
				if encoding == "zstd" {
					body = enc.EncodeAll(chunk, nil)
				}
				req := httptest.NewRequest(http.MethodPut, "/c/default/c/api/chunk/"+hex.EncodeToString(sum[:]), strings.NewReader(string(body)))
				req.ContentLength = int64(len(body))
				if encoding != "" {
					req.Header.Set("Content-Encoding", encoding)
				}
				rec := httptest.NewRecorder()
				b.StartTimer()
				h.ServeHTTP(rec, req)
				if rec.Code != http.StatusOK {
					b.Fatalf("put chunk: %d %s", rec.Code, rec.Body)
				}
			}
		})
	}
}
