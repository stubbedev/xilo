package server

import (
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"testing"
)

type discardResponseWriter struct{ h http.Header }

func (w *discardResponseWriter) Header() http.Header         { return w.h }
func (w *discardResponseWriter) Write(p []byte) (int, error) { return len(p), nil }
func (w *discardResponseWriter) WriteHeader(int)             {}

func benchNarParts() [][]byte {
	const chunk, total, run = 1 << 20, 32 << 20, 4 << 10
	const text = "/nix/store/-lib/bin/share/include\x00\x7fELF"
	rng := rand.New(rand.NewPCG(1, 2))
	data := make([]byte, total)
	for i := range data {
		if (i/run)%3 == 0 {
			data[i] = byte(rng.Uint32())
			continue
		}
		data[i] = text[rng.IntN(len(text))]
	}
	parts := make([][]byte, 0, total/chunk)
	for p := data; len(p) > 0; p = p[chunk:] {
		parts = append(parts, p[:chunk])
	}
	return parts
}

func BenchmarkServeNar(b *testing.B) {
	parts := benchNarParts()
	_, db, ts := newTestServer(b, true)
	if _, err := db.CreateCache("default", "c", true, 40); err != nil {
		b.Fatal(err)
	}
	pr := pushChunked(b, ts, "c", h32, parts, nil, "")
	h := ts.Config.Handler
	for _, enc := range []string{"identity", "gzip", "zstd"} {
		b.Run("encoding="+enc, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(pr.NarSize))
			for b.Loop() {
				req := httptest.NewRequest(http.MethodGet, "/c/default/c/nar/"+h32+".nar", nil)
				req.Header.Set("Accept-Encoding", enc)
				h.ServeHTTP(&discardResponseWriter{h: http.Header{}}, req)
			}
		})
	}
}
