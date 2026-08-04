package push

// Client-side push state: per-store-path chunk manifests and the server's chunk
// presence filter, cached under the user cache dir. Everything here is a pure
// optimization — a cold, empty or corrupt cache only costs work, never
// correctness, so every function fails silently.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/stubbedev/xilo/internal/bloom"
	"github.com/stubbedev/xilo/internal/chunk"
)

// manifestMaxAge prunes manifests untouched for this long. Store paths are
// immutable so a manifest never goes stale, but a build machine churns through
// paths it will never push again.
const manifestMaxAge = 30 * 24 * time.Hour

// stateDir is where the client caches manifests and filters. XILO_CACHE_DIR
// overrides it (tests, CI, read-only HOME); empty disables caching entirely.
func stateDir() string {
	if d := os.Getenv("XILO_CACHE_DIR"); d != "" {
		return d
	}
	d, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(d, "xilo")
}

// manifestDir namespaces manifests by chunking params: the same NAR splits into
// a different chunk list under different params, and params are server-dictated.
func manifestDir(p chunk.Params) string {
	d := stateDir()
	if d == "" {
		return ""
	}
	return filepath.Join(d, "manifests", fmt.Sprintf("%d-%d-%d", p.MinSize, p.AvgSize, p.MaxSize))
}

// loadManifest returns the cached chunk list (in NAR order) for a store path,
// or nil. A hit means the whole dump + chunk pass can be skipped: put-path
// either accepts the list or answers 409, and the caller falls back.
//
// The NAR hash is part of the key, not just the path: an input-addressed path
// that was GC'd and rebuilt non-reproducibly keeps its store hash while its
// bytes change, and reusing the old chunk list for it would register a path
// whose contents don't match the NAR hash being claimed.
func loadManifest(p chunk.Params, storeHash, narHash string) []string {
	dir := manifestDir(p)
	if dir == "" || !validHash(storeHash) || narHash == "" {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(dir, storeHash))
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) < 2 || lines[0] != narHash {
		return nil // different contents (or a pre-NAR-hash file): re-chunk
	}
	for _, l := range lines[1:] {
		if !validChunkHash(l) {
			return nil // truncated or garbage file: ignore the whole thing
		}
	}
	return lines[1:]
}

func saveManifest(p chunk.Params, storeHash, narHash string, chunks []string) {
	dir := manifestDir(p)
	if dir == "" || !validHash(storeHash) || narHash == "" || len(chunks) == 0 {
		return
	}
	if strings.ContainsAny(narHash, "\n\r") {
		return
	}
	if os.MkdirAll(dir, 0o755) != nil {
		return
	}
	body := narHash + "\n" + strings.Join(chunks, "\n")
	writeFileAtomic(filepath.Join(dir, storeHash), []byte(body))
}

// pruneState drops manifests older than manifestMaxAge, at most once a day
// (tracked by the mtime of a stamp file). Called from Push in the background.
func pruneState(p chunk.Params) {
	dir := manifestDir(p)
	if dir == "" {
		return
	}
	stamp := filepath.Join(dir, ".pruned")
	if fi, err := os.Stat(stamp); err == nil && time.Since(fi.ModTime()) < 24*time.Hour {
		return
	}
	if os.MkdirAll(dir, 0o755) != nil {
		return
	}
	writeFileAtomic(stamp, nil)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-manifestMaxAge)
	for _, e := range entries {
		fi, err := e.Info()
		if err != nil || !validHash(e.Name()) || fi.ModTime().After(cutoff) {
			continue
		}
		os.Remove(filepath.Join(dir, e.Name()))
	}
}

// logMax caps the detached-push log: it is append-only across runs, and a
// build machine pushing all day would otherwise grow it forever. Truncated (not
// rotated) — it is a debugging aid, not an audit trail.
const logMax = 4 << 20

// OpenLog opens the log a detached push writes to, creating the state dir and
// truncating the file if it has outgrown logMax. Returns the file and its path.
func OpenLog() (*os.File, string, error) {
	dir := stateDir()
	if dir == "" {
		return nil, "", errors.New("no user cache directory for the push log")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, "", err
	}
	path := filepath.Join(dir, "push.log")
	if fi, err := os.Stat(path); err == nil && fi.Size() > logMax {
		os.Remove(path)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, "", err
	}
	return f, path, nil
}

// ---- chunk presence filter ----

// filterKey names the on-disk copy of one server+cache's filter. Hashed: a base
// URL is not a filename.
func filterKey(base, cache string) string {
	sum := sha256.Sum256([]byte(base + "\x00" + cache))
	return hex.EncodeToString(sum[:16])
}

// cachedFilter returns the stored filter body, its ETag, and how long ago it
// was fetched. A body with no ETag is still usable — just not revalidatable.
func cachedFilter(key string) (body []byte, etag string, age time.Duration) {
	dir := stateDir()
	if dir == "" {
		return nil, "", 0
	}
	path := filepath.Join(dir, "filters", key)
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, "", 0
	}
	fi, err := os.Stat(path)
	if err != nil {
		return nil, "", 0
	}
	tag, _ := os.ReadFile(path + ".etag")
	return body, strings.TrimSpace(string(tag)), time.Since(fi.ModTime())
}

func saveFilter(key, etag string, body []byte) {
	dir := stateDir()
	if dir == "" {
		return
	}
	dir = filepath.Join(dir, "filters")
	if os.MkdirAll(dir, 0o755) != nil {
		return
	}
	path := filepath.Join(dir, key)
	writeFileAtomic(path, body)
	writeFileAtomic(path+".etag", []byte(etag))
}

// touchFilter marks a revalidated (304) copy as fresh so the next push inside
// the freshness window skips the request entirely.
func touchFilter(key string) {
	dir := stateDir()
	if dir == "" {
		return
	}
	now := time.Now()
	os.Chtimes(filepath.Join(dir, "filters", key), now, now)
}

func parseFilter(body []byte) *bloom.Filter {
	if len(body) == 0 {
		return nil
	}
	f, err := bloom.Unmarshal(body)
	if err != nil {
		return nil
	}
	return f
}

// ---- helpers ----

// writeFileAtomic writes via a temp file + rename so a killed push (or two
// concurrent ones) can never leave a half-written manifest behind.
func writeFileAtomic(path string, data []byte) {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*")
	if err != nil {
		return
	}
	name := tmp.Name()
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if werr != nil || cerr != nil || os.Rename(name, path) != nil {
		os.Remove(name)
	}
}

// validHash guards path construction: a store hash reaches us from nix output
// and must never contain a separator.
func validHash(s string) bool {
	if len(s) != 32 {
		return false
	}
	for _, r := range s {
		if !strings.ContainsRune("0123456789abcdfghijklmnpqrsvwxyz", r) { // nix base32
			return false
		}
	}
	return true
}

func validChunkHash(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}
