package store

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stubbedev/xilo/internal/config"
	"github.com/stubbedev/xilo/internal/storage"
)

// TestPostgres exercises the whole store API against a real PostgreSQL when
// XILO_PG_TEST_DSN is set (CI provides a service container; locally:
//
//	docker run --rm -e POSTGRES_PASSWORD=x -p 5433:5432 postgres:16
//	XILO_PG_TEST_DSN='postgres://postgres:x@localhost:5433/postgres' go test ./internal/store -run TestPostgres
func TestPostgres(t *testing.T) {
	dsn := os.Getenv("XILO_PG_TEST_DSN")
	if dsn == "" {
		t.Skip("XILO_PG_TEST_DSN not set")
	}

	db, err := OpenPostgres(dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// Fresh slate: earlier runs against the same server leave rows behind.
	for _, tbl := range []string{"paths", "chunks", "tokens", "caches", "passkeys", "sessions", "metrics_minutes", "users", "account_members", "accounts"} {
		if err := db.write(func(tx *sql.Tx) error { _, err := tx.Exec(`DELETE FROM ` + tbl); return err }); err != nil {
			t.Fatalf("clean %s: %v", tbl, err)
		}
	}

	// Idempotent migrate: reopening must not fail on the existing schema.
	db2, err := OpenPostgres(dsn)
	if err != nil {
		t.Fatalf("re-open (migrate idempotence): %v", err)
	}
	db2.Close()

	now := time.Now().Unix()

	// --- caches ---
	c, err := db.CreateCache("default", "pg-cache", false, 41)
	if err != nil {
		t.Fatalf("CreateCache: %v", err)
	}
	if c.ID == 0 || c.PubKey == "" {
		t.Fatalf("cache = %+v", c)
	}
	got, err := db.GetCache("default", "pg-cache")
	if err != nil || got.Public || got.Priority != 41 {
		t.Fatalf("GetCache: %+v %v", got, err)
	}
	if err := db.UpdateCache(c.ID, true, 30, 3600, 1<<30); err != nil {
		t.Fatalf("UpdateCache: %v", err)
	}
	if rot, err := db.RotateKey(c.ID, c.Name); err != nil || rot.PubKey == got.PubKey {
		t.Fatalf("RotateKey: %+v %v", rot, err)
	}
	if list, err := db.ListCaches(); err != nil || len(list) != 1 {
		t.Fatalf("ListCaches: %v %v", list, err)
	}

	// --- tokens ---
	secret, tok, err := db.CreateToken(0, "pg-tok", []string{"default/pg-cache"}, []string{"push", "pull"}, 0)
	if err != nil || tok.ID == 0 {
		t.Fatalf("CreateToken: %+v %v", tok, err)
	}
	if !db.Authorize(secret, "default", "pg-cache", "push", now) {
		t.Fatal("Authorize should pass")
	}
	if db.Authorize(secret, "default", "other", "push", now) {
		t.Fatal("Authorize wrong cache should fail")
	}
	if db.AuthorizeAdmin(secret, now) {
		t.Fatal("non-admin token must not pass AuthorizeAdmin")
	}
	adminSec, _, err := db.CreateToken(0, "pg-admin", nil, []string{"admin"}, 0)
	if err != nil || !db.AuthorizeAdmin(adminSec, now) {
		t.Fatalf("admin token: %v", err)
	}
	if err := db.UpdateToken(tok.ID, "renamed", []string{"default/pg-cache"}, []string{"pull"}, 0); err != nil {
		t.Fatalf("UpdateToken: %v", err)
	}
	if db.Authorize(secret, "default", "pg-cache", "push", now) {
		t.Fatal("push should be gone after update")
	}
	if err := db.RevokeToken(tok.ID); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	if db.Authorize(secret, "default", "pg-cache", "pull", now) {
		t.Fatal("revoked token still authorizes")
	}
	if toks, err := db.ListTokens(); err != nil || len(toks) != 2 {
		t.Fatalf("ListTokens: %v %v", toks, err)
	}

	// --- namespaces ---
	ns2, err := db.EnsureAccount("team", "org")
	if err != nil || ns2.ID == 0 {
		t.Fatalf("EnsureNamespace: %+v %v", ns2, err)
	}
	if again, err := db.EnsureAccount("team", "org"); err != nil || again.ID != ns2.ID {
		t.Fatalf("EnsureNamespace idempotence: %+v %v", again, err)
	}
	tc, err := db.CreateCache("team", "pg-cache", true, 40)
	if err != nil {
		t.Fatalf("same cache name in second namespace: %v", err)
	}
	nsSec, _, err := db.CreateToken(ns2.ID, "team-tok", []string{"pg-cache"}, []string{"pull"}, 0)
	if err != nil {
		t.Fatalf("ns token: %v", err)
	}
	if !db.Authorize(nsSec, "team", "pg-cache", "pull", now) {
		t.Fatal("ns token should pull in its namespace")
	}
	if db.Authorize(nsSec, "default", "pg-cache", "pull", now) {
		t.Fatal("ns token must not cross namespaces")
	}
	if err := db.DeleteCache(tc.ID); err != nil {
		t.Fatalf("delete team cache: %v", err)
	}
	if err := db.DeleteOrg(ns2.ID); err != nil {
		t.Fatalf("DeleteNamespace: %v", err)
	}

	// --- chunks + paths ---
	if err := db.PutChunk("default", "aaa", 100, 50, "chunk/aa/aaa", now); err != nil {
		t.Fatalf("PutChunk: %v", err)
	}
	if err := db.PutChunk("default", "aaa", 100, 50, "chunk/aa/aaa", now+1); err != nil {
		t.Fatalf("PutChunk upsert: %v", err)
	}
	if err := db.PutChunk("default", "bbb", 200, 90, "chunk/bb/bbb", now); err != nil {
		t.Fatalf("PutChunk bbb: %v", err)
	}
	if !db.HasChunk("default", "aaa") || db.HasChunk("default", "zzz") {
		t.Fatal("HasChunk wrong")
	}
	missing, err := db.MissingChunks("default", []string{"aaa", "zzz"})
	if err != nil || len(missing) != 1 || missing[0] != "zzz" {
		t.Fatalf("MissingChunks: %v %v", missing, err)
	}
	if err := db.TouchChunks("default", []string{"aaa", "bbb"}, now+100); err != nil {
		t.Fatalf("TouchChunks: %v", err)
	}

	p := &Path{
		StorePath: "/nix/store/00000000000000000000000000000000-Hello-1.0",
		NarHash:   "sha256:abc", NarSize: 300,
		Refs:   []string{"/nix/store/00000000000000000000000000000000-Hello-1.0"},
		Chunks: []string{"aaa", "bbb"},
	}
	if err := db.PutPath(c.ID, "00000000000000000000000000000000", p); err != nil {
		t.Fatalf("PutPath: %v", err)
	}
	if err := db.PutPath(c.ID, "00000000000000000000000000000000", p); err != nil {
		t.Fatalf("PutPath upsert: %v", err)
	}
	gp, err := db.GetPath(c.ID, "00000000000000000000000000000000")
	if err != nil || gp.NarSize != 300 || len(gp.Chunks) != 2 {
		t.Fatalf("GetPath: %+v %v", gp, err)
	}
	mp, err := db.MissingPaths(c.ID, []string{"00000000000000000000000000000000", "11111111111111111111111111111111"})
	if err != nil || len(mp) != 1 {
		t.Fatalf("MissingPaths: %v %v", mp, err)
	}
	keys, err := db.ChunkKeys("default", []string{"aaa", "bbb"})
	if err != nil || keys[0].Key != "chunk/aa/aaa" {
		t.Fatalf("ChunkKeys: %v %v", keys, err)
	}
	db.TouchPath(c.ID, "00000000000000000000000000000000", now+7200, 60)

	// --- search (fuzzy + rank + sort paths hit instr/COLLATE replacements) ---
	for _, q := range []string{"", "hello", "HELLO", "hlo"} {
		paths, total, err := db.SearchPaths(c.ID, q, 10, 0, "", "")
		if err != nil || total != 1 || len(paths) != 1 {
			t.Fatalf("SearchPaths(%q): %v %d %v", q, paths, total, err)
		}
	}
	if _, total, err := db.SearchPaths(c.ID, "nomatch", 10, 0, "", ""); err != nil || total != 0 {
		t.Fatalf("SearchPaths(nomatch): %d %v", total, err)
	}
	for _, sort := range []string{"path", "size", "pulled"} {
		if _, _, err := db.SearchPaths(c.ID, "hello", 10, 0, sort, "asc"); err != nil {
			t.Fatalf("SearchPaths sort %s: %v", sort, err)
		}
	}

	// --- stats ---
	st, err := db.CacheStats(c.ID)
	if err != nil || st.Paths != 1 || st.PhysicalBytes != 140 {
		t.Fatalf("CacheStats: %+v %v", st, err)
	}
	if _, err := db.GlobalStats(); err != nil {
		t.Fatalf("GlobalStats: %v", err)
	}

	// --- users / totp ---
	if db.UsersExist() {
		t.Fatal("no users should exist yet")
	}
	usr, err := db.CreateUser("admin", "", "hash1", "owner")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if got, err := db.GetUserByName("admin"); err != nil || got.ID != usr.ID {
		t.Fatalf("GetUserByName: %+v %v", got, err)
	}
	if err := db.SetUserTOTPSecret(usr.ID, []byte("s3cret")); err != nil {
		t.Fatalf("SetUserTOTPSecret: %v", err)
	}
	if err := db.SetUserTOTPEnabled(usr.ID, true); err != nil {
		t.Fatalf("SetUserTOTPEnabled: %v", err)
	}
	if sec, on, err := db.UserTOTP(usr.ID); err != nil || !on || string(sec) != "s3cret" {
		t.Fatalf("UserTOTP: %q %v %v", sec, on, err)
	}

	// --- sessions / passkeys ---
	if err := db.CreateSession("sess1", usr.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if uid, ok := db.SessionUser("sess1"); !ok || uid != usr.ID {
		t.Fatalf("SessionUser: %d %v", uid, ok)
	}
	if _, ok := db.SessionUser("nope"); ok {
		t.Fatal("bogus session should not resolve")
	}
	if err := db.DropSession("sess1"); err != nil {
		t.Fatal("DropSession failed")
	}
	if _, ok := db.SessionUser("sess1"); ok {
		t.Fatal("dropped session should not resolve")
	}
	if err := db.AddPasskey(usr.ID, "key1", []byte{1, 2, 3}); err != nil {
		t.Fatalf("AddPasskey: %v", err)
	}
	pks, err := db.ListUserPasskeys(usr.ID)
	if err != nil || len(pks) != 1 {
		t.Fatalf("ListUserPasskeys: %v %v", pks, err)
	}

	// --- metrics ---
	if err := db.AddMetricMinute(MetricMinute{TS: now, Req: 1.5, Lat: 2.5, Bps: 3.5, Stored: 42}); err != nil {
		t.Fatalf("AddMetricMinute: %v", err)
	}
	if err := db.AddMetricMinute(MetricMinute{TS: now, Req: 9, Lat: 9, Bps: 9, Stored: 9}); err != nil {
		t.Fatalf("AddMetricMinute upsert: %v", err)
	}
	ms, err := db.MetricRange(now, now+1)
	if err != nil || len(ms) != 1 || ms[0].Req != 9 {
		t.Fatalf("MetricRange: %v %v", ms, err)
	}

	// --- eviction + GC ---
	dir := t.TempDir()
	blob, err := storage.New(storageLocalCfg(dir))
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	ctx := context.Background()
	for _, k := range []string{"chunk/aa/aaa", "chunk/bb/bbb"} {
		if err := blob.Put(ctx, k, strings.NewReader("x")); err != nil {
			t.Fatalf("blob put: %v", err)
		}
	}
	if n, err := db.EvictCachePathsOlderThan(c.ID, now+9999); err != nil || n != 1 {
		t.Fatalf("EvictCachePathsOlderThan: %d %v", n, err)
	}
	deleted, freed, err := db.GC(ctx, blob, "default", now+99999)
	if err != nil || deleted != 2 || freed != 140 {
		t.Fatalf("GC: deleted=%d freed=%d %v", deleted, freed, err)
	}

	// --- caps + fsck helpers ---
	if _, err := db.EnforceGlobalCap(1); err != nil {
		t.Fatalf("EnforceGlobalCap: %v", err)
	}
	if err := db.DeletePaths([]int64{999999}); err != nil {
		t.Fatalf("DeletePaths: %v", err)
	}
	if err := db.DeleteChunkRows("default", []string{"nonexistent"}); err != nil {
		t.Fatalf("DeleteChunkRows: %v", err)
	}

	// --- cascade ---
	if err := db.DeleteCache(c.ID); err != nil {
		t.Fatalf("DeleteCache: %v", err)
	}
	if _, err := db.GetCache("default", "pg-cache"); err != ErrNotFound {
		t.Fatalf("cache should be gone: %v", err)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func storageLocalCfg(dir string) (cfg config.Storage) {
	cfg.Backend = "local"
	cfg.Local.Root = dir
	return cfg
}
