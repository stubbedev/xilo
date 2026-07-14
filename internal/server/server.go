// Package server wires the HTTP surface: the standard Nix binary-cache protocol
// (pull) and xilo's own push API. One cache lives under /{ns}/{cache}/….
package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"sort"
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
	sts       map[string]storage.Storage // named blob backends
	enc       *zstd.Encoder              // EncodeAll — safe for concurrent use
	dec       *zstd.Decoder              // DecodeAll — safe for concurrent use
	sess      *sessions
	ceremony  ceremonies // in-flight WebAuthn challenges
	wanOnce   sync.Once
	wan       *webauthn.WebAuthn
	wanErr    error
	uploadSem chan struct{} // bounds concurrent server-side chunk encode+store
	logins    *loginLimiter // throttles bcrypt attempts per IP
	niCache   *narinfoCache // rendered+signed narinfo bodies
	gzipPool  sync.Pool     // *gzip.Writer for NAR wire compression
	touched   sync.Map      // "<cacheID>/<storeHash>" → unix secs of last LRU bump
	egress    sync.Map      // accountID (int64) → *atomic.Int64 pending NAR bytes
	metrics   metrics
	stat      statusRing
	started   time.Time
}

func New(cfg *config.Config, db *store.DB, sts map[string]storage.Storage) (*Server, error) {
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstdLevel(cfg.Compression.Level)))
	if err != nil {
		return nil, err
	}
	dec, err := zstd.NewReader(nil)
	if err != nil {
		return nil, err
	}
	return &Server{
		cfg: cfg, db: db, sts: sts, enc: enc, dec: dec,
		sess:      newSessions(db),
		started:   time.Now(),
		uploadSem: make(chan struct{}, max(4, 2*runtime.NumCPU())),
		logins:    newLoginLimiter(),
		niCache:   newNarinfoCache(16384), // ~64B/key + body ~600B ⇒ ~10MB cap
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
	// The binary-cache protocol lives on its own mux: its /{ns}/{cache}/…
	// wildcards would otherwise conflict with /admin/cache/{ns}/{name}. The
	// root mux's literal prefixes (admin, api, static, …) win; everything
	// else falls through to the cache mux.
	// Caches mount under /c/{account}/{cache}/… so account slugs never fight
	// top-level routes for names.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /c/{account}/{cache}/nix-cache-info", s.handleCacheInfo)
	mux.HandleFunc("GET /c/{account}/{cache}/nar/{id}", s.handleNar)
	mux.HandleFunc("GET /c/{account}/{cache}/{file}", s.handleNarinfo) // *.narinfo

	// Push API.
	mux.HandleFunc("GET /c/{account}/{cache}/api/config", s.handleConfig)
	mux.HandleFunc("POST /c/{account}/{cache}/api/get-missing-paths", s.handleMissingPaths)
	mux.HandleFunc("POST /c/{account}/{cache}/api/get-missing-chunks", s.handleMissingChunks)
	mux.HandleFunc("PUT /c/{account}/{cache}/api/chunk/{hash}", s.handlePutChunk)
	mux.HandleFunc("PUT /c/{account}/{cache}/api/path", s.handlePutPath)

	s.registerAdmin(mux)
	s.registerAdminAPI(mux)
	if s.cfg.MultiTenant {
		s.registerTenancy(mux)
	}
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
	s.startAuditPrune(ctx)
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
		// Baseline hardening headers. The admin UI relies on inline scripts and
		// onclick handlers, so script/style stay 'unsafe-inline'; the CSP still
		// locks down object/base-uri/frame-ancestors and confines everything
		// else to same-origin, giving a second layer under templ's escaping.
		hdr := w.Header()
		hdr.Set("X-Frame-Options", "DENY")
		hdr.Set("X-Content-Type-Options", "nosniff")
		hdr.Set("Referrer-Policy", "same-origin")
		hdr.Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self' 'unsafe-inline'; "+
				"style-src 'self' 'unsafe-inline'; img-src 'self' data:; "+
				"object-src 'none'; base-uri 'none'; frame-ancestors 'none'")
		if s.secureCookies() {
			hdr.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
		}
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
			// logging=quiet: only errors and slow requests — the synchronous
			// log write (global mutex + stderr) is measurable at 10k+ rps.
			if s.cfg.Logging != "quiet" || lw.status >= 400 || elapsed > time.Second {
				log.Printf("%s %s %d %s", r.Method, r.URL.Path, lw.status, elapsed.Round(time.Millisecond))
			}
			// Activities: successful admin/API mutations only.
			if lw.status < 400 && auditable(r) {
				s.recordAudit(r, lw.status, elapsed)
			}
		}()
		h.ServeHTTP(lw, r)
	})
}

// auditable reports whether a request is an admin/API action worth recording
// as activities. Only mutating methods on the admin UI, JSON API and
// registration count; the high-volume cache protocol and read-only handshakes
// (live password strength `/check`, WebAuthn challenge `/begin`) are excluded.
func auditable(r *http.Request) bool {
	switch r.Method {
	case http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch:
	default:
		return false
	}
	p := r.URL.Path
	if strings.HasSuffix(p, "/check") || strings.HasSuffix(p, "/begin") {
		return false
	}
	return strings.HasPrefix(p, "/admin/") || strings.HasPrefix(p, "/api/v1/") || p == "/register"
}

// recordAudit writes one activity row, resolving the acting user from the
// session cookie (absent for token/CLI-driven API calls). Best-effort: a miss
// is logged, never surfaced to the request.
func (s *Server) recordAudit(r *http.Request, status int, elapsed time.Duration) {
	var uid int64
	var actor string
	if u := s.currentUser(r); u != nil {
		uid, actor = u.ID, u.Name
	}
	if err := s.db.Audit(store.AuditEntry{
		UserID: uid, Actor: actor, Method: r.Method, Path: r.URL.Path, Status: status,
		IP: s.clientIP(r), UserAgent: r.UserAgent(), DurationMs: elapsed.Milliseconds(),
	}); err != nil {
		log.Printf("audit: %v", err)
	}
}

// isCacheTraffic reports whether a request is real binary-cache work (pull
// protocol or push API). The dashboard, static assets and monitoring probes
// would otherwise drown the stats in their own polling noise.
func isCacheTraffic(path string) bool {
	return path != "/" &&
		!strings.HasPrefix(path, "/admin") &&
		!strings.HasPrefix(path, "/register") &&
		!strings.HasPrefix(path, "/api/v1/") &&
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

// Audit pruning runs on its own schedule, independent of the chunk sweeper, so
// the activity list stays bounded even when gc.interval is unset. Deliberately gentle:
// it deletes in small batches with a pause between each, so the single writer
// goroutine is free for real traffic almost all the time — pruning never
// competes with a push or an admin action for the write lock.
const (
	auditPruneEvery = 6 * time.Hour          // how often to drain expired rows
	auditPruneBatch = 200                    // rows per write transaction
	auditPrunePause = 500 * time.Millisecond // yield between batches
)

// startAuditPrune launches the low-priority activity pruner if
// gc.audit_retention is set (default one year).
func (s *Server) startAuditPrune(ctx context.Context) {
	ret := parseDur(s.cfg.GC.AuditRetention)
	if ret <= 0 {
		return
	}
	log.Printf("audit: pruning entries older than %s every %s", ret, auditPruneEvery)
	go func() {
		t := time.NewTicker(auditPruneEvery)
		defer t.Stop()
		// First pass on the first tick, never synchronously at startup: touching
		// the DB before a tick would race a shutdown that closes it (same reason
		// startGC waits for its ticker).
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.pruneAudit(ctx, ret)
			}
		}
	}()
}

// pruneAudit deletes expired activity rows in small paced batches, stopping
// as soon as a batch comes up short (nothing left) or the context is cancelled.
func (s *Server) pruneAudit(ctx context.Context, retention time.Duration) {
	cutoff := time.Now().Add(-retention).Unix()
	var total int64
	for {
		n, err := s.db.PruneAuditBatch(cutoff, auditPruneBatch)
		if err != nil {
			log.Printf("audit: prune: %v", err)
			return
		}
		total += n
		if n < auditPruneBatch { // drained
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(auditPrunePause):
		}
	}
	if total > 0 {
		log.Printf("audit: pruned %d expired entries", total)
	}
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

// stOf resolves a named storage backend, falling back to "default" so a row
// naming a since-removed backend degrades to the primary instead of a panic.
func (s *Server) stOf(name string) storage.Storage {
	if st, ok := s.sts[name]; ok {
		return st
	}
	return s.sts["default"]
}

// storageNames lists configured backends, default first, rest sorted.
func (s *Server) storageNames() []string {
	names := make([]string, 0, len(s.sts))
	for n := range s.sts {
		if n != s.cfg.DefaultStorage {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return append([]string{s.cfg.DefaultStorage}, names...)
}

// resolveStorage validates a requested backend name ("" = the configured
// default) against the configured set.
func (s *Server) resolveStorage(name string) (string, error) {
	if name == "" {
		name = s.cfg.DefaultStorage
	}
	if _, ok := s.sts[name]; !ok {
		return "", fmt.Errorf("unknown storage backend %q", name)
	}
	return name, nil
}

// assignStorage pins a just-created cache to a backend. ponytail: two-step
// insert-then-set — a push racing this window would land chunks in 'default';
// harmless (fsck heals) and only possible in the same instant the cache is born.
func (s *Server) assignStorage(c *store.Cache, stName string) error {
	if stName == c.Storage {
		return nil
	}
	if err := s.db.SetCacheStorage(c.ID, stName); err != nil {
		return err
	}
	c.Storage = stName
	return nil
}

// cache resolves the /c/{account}/{cache} path segments, writing 404 if unknown.
func (s *Server) cache(w http.ResponseWriter, r *http.Request) (*store.Cache, bool) {
	c, err := s.db.GetCache(r.PathValue("account"), r.PathValue("cache"))
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
