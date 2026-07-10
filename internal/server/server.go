// Package server wires the HTTP surface: the standard Nix binary-cache protocol
// (pull) and xilo's own push API. One cache lives under /{cache}/….
package server

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/klauspost/compress/zstd"

	"github.com/stubbedev/xilo/internal/config"
	"github.com/stubbedev/xilo/internal/storage"
	"github.com/stubbedev/xilo/internal/store"
)

type Server struct {
	cfg       *config.Config
	db        *store.DB
	st        storage.Storage
	enc       *zstd.Encoder // EncodeAll — safe for concurrent use
	dec       *zstd.Decoder // DecodeAll — safe for concurrent use
	sess      *sessions
	ceremony  ceremonies // in-flight WebAuthn challenges
	wanOnce   sync.Once
	wan       *webauthn.WebAuthn
	wanErr    error
	uploadSem chan struct{} // bounds concurrent server-side chunk encode+store
	metrics   metrics
	stat      statusRing
	started   time.Time
}

func New(cfg *config.Config, db *store.DB, st storage.Storage) (*Server, error) {
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstdLevel(cfg.Compression.Level)))
	if err != nil {
		return nil, err
	}
	dec, err := zstd.NewReader(nil)
	if err != nil {
		return nil, err
	}
	return &Server{
		cfg: cfg, db: db, st: st, enc: enc, dec: dec,
		sess:      newSessions(db),
		started:   time.Now(),
		uploadSem: make(chan struct{}, max(4, 2*runtime.NumCPU())),
	}, nil
}

// zstdLevel maps a config level name to a klauspost encoder level.
func zstdLevel(name string) zstd.EncoderLevel {
	switch name {
	case "fastest":
		return zstd.SpeedFastest
	case "better":
		return zstd.SpeedBetterCompression
	case "best":
		return zstd.SpeedBestCompression
	default:
		return zstd.SpeedDefault
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Binary-cache protocol (pull).
	mux.HandleFunc("GET /{cache}/nix-cache-info", s.handleCacheInfo)
	mux.HandleFunc("GET /{cache}/nar/{id}", s.handleNar)
	mux.HandleFunc("GET /{cache}/{file}", s.handleNarinfo) // *.narinfo

	// Push API.
	mux.HandleFunc("GET /{cache}/api/config", s.handleConfig)
	mux.HandleFunc("POST /{cache}/api/get-missing-paths", s.handleMissingPaths)
	mux.HandleFunc("POST /{cache}/api/get-missing-chunks", s.handleMissingChunks)
	mux.HandleFunc("PUT /{cache}/api/chunk/{hash}", s.handlePutChunk)
	mux.HandleFunc("PUT /{cache}/api/path", s.handlePutPath)

	s.registerAdmin(mux)
	s.registerPasskeyRoutes(mux)
	s.registerStatic(mux)

	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	mux.HandleFunc("GET /", s.handleIndex)
	return mux
}

func (s *Server) Run() error {
	return s.RunContext(context.Background())
}

// RunContext serves until the context is cancelled or SIGINT/SIGTERM, then
// shuts down gracefully (drains in-flight requests).
func (s *Server) RunContext(ctx context.Context) error {
	// Background loops get the signal-aware ctx so they stop at shutdown
	// instead of writing to a closed DB.
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	s.startGC(ctx)
	s.startStatusSampler(ctx)

	srv := &http.Server{
		Addr:    s.cfg.Listen,
		Handler: s.middleware(s.Handler()),
		// Header timeout guards against slowloris. Read/Write are left open:
		// NAR uploads/downloads are large and legitimately slow.
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          log.Default(),
	}

	errc := make(chan error, 1)
	go func() {
		log.Printf("xilo listening on %s (storage=%s)", s.cfg.Listen, s.cfg.Storage.Backend)
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		log.Printf("shutting down…")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

// middleware wraps the mux with panic recovery + request logging.
func (s *Server) middleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		lw := &logWriter{ResponseWriter: w, status: http.StatusOK}
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("panic: %v", rec)
				if !lw.wrote {
					http.Error(lw, "internal error", http.StatusInternalServerError)
				}
			}
			elapsed := time.Since(start)
			if isCacheTraffic(r.URL.Path) {
				s.metrics.reqTotal.Add(1)
				s.metrics.reqDurNs.Add(elapsed.Nanoseconds())
			}
			log.Printf("%s %s %d %s", r.Method, r.URL.Path, lw.status, elapsed.Round(time.Millisecond))
		}()
		h.ServeHTTP(lw, r)
	})
}

// isCacheTraffic reports whether a request is real binary-cache work (pull
// protocol or push API). The dashboard, static assets and monitoring probes
// would otherwise drown the stats in their own polling noise.
func isCacheTraffic(path string) bool {
	return path != "/" &&
		!strings.HasPrefix(path, "/admin") &&
		!strings.HasPrefix(path, "/static/") &&
		path != "/healthz" && path != "/metrics" &&
		path != "/favicon.ico"
}

type logWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *logWriter) WriteHeader(code int) {
	w.status = code
	w.wrote = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *logWriter) Write(b []byte) (int, error) {
	w.wrote = true
	return w.ResponseWriter.Write(b)
}

// Flush/Unwrap so streaming NAR responses and zstd flushing still work.
func (w *logWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// startGC launches the background sweeper if gc.interval is set.
func (s *Server) startGC(ctx context.Context) {
	interval := parseDur(s.cfg.GC.Interval)
	if interval <= 0 {
		return
	}
	log.Printf("gc: background sweep every %s (retention=%q grace=%q)", interval, s.cfg.GC.Retention, s.cfg.GC.Grace)
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			if del, freed, err := s.runGC(ctx); err != nil {
				log.Printf("gc: %v", err)
			} else if del > 0 {
				log.Printf("gc: swept %d chunks, freed %d bytes", del, freed)
			}
		}
	}()
}

// parseDur treats "" and "0" as disabled.
func parseDur(s string) time.Duration {
	if s == "" || s == "0" {
		return 0
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		log.Printf("config: bad duration %q: %v", s, err)
		return 0
	}
	return d
}

// parseDurSafe is parseDur for durations where "unparsable" must fail SAFE
// (fall back to def), not fail open. A typo'd gc.grace returning 0 would make
// every unreferenced chunk instantly sweepable.
func parseDurSafe(s string, def time.Duration) time.Duration {
	if s == "" || s == "0" {
		return 0 // explicit opt-out
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		log.Printf("config: bad duration %q: %v (using %s)", s, err, def)
		return def
	}
	return d
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/" {
		http.Redirect(w, r, "/admin", http.StatusFound)
		return
	}
	s.notFoundNegotiated(w, r)
}

// notFoundNegotiated writes a styled 404 for browsers, plain text for API
// clients (nix).
func (s *Server) notFoundNegotiated(w http.ResponseWriter, r *http.Request) {
	if strings.Contains(r.Header.Get("Accept"), "text/html") {
		s.notFound(w, r)
		return
	}
	http.NotFound(w, r)
}

// handleHealth is a dependency-free readiness probe: it does one cheap DB read.
// ?format=json (or Accept: application/json) returns the machine shape.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	_, err := s.db.ListCaches()
	if wantsJSON(r) {
		status := "ok"
		if err != nil {
			status = "error"
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		jsonOut(w, map[string]any{"status": status, "uptime_seconds": int64(time.Since(s.started).Seconds())})
		return
	}
	if err != nil {
		http.Error(w, "db error", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte("ok\n"))
}

// cache resolves the {cache} path segment, writing 404 if unknown.
func (s *Server) cache(w http.ResponseWriter, r *http.Request) (*store.Cache, bool) {
	name := r.PathValue("cache")
	c, err := s.db.GetCache(name)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "no such cache", http.StatusNotFound)
		return nil, false
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return nil, false
	}
	return c, true
}
