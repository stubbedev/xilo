package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stubbedev/xilo/internal/api"
)

// missingPaths posts get-missing-paths (with NAR hashes, so adoption can fire)
// and returns the still-missing store hashes.
func missingPaths(t *testing.T, ts *httptest.Server, cache, token string, refs []api.PathRef) []string {
	t.Helper()
	hashes := make([]string, len(refs))
	for i, r := range refs {
		hashes[i] = r.Hash
	}
	body, _ := json.Marshal(api.MissingReq{Hashes: hashes, Paths: refs})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/c/default/"+cache+"/api/get-missing-paths", bytes.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("get-missing-paths: %d %s", resp.StatusCode, b)
	}
	var out api.MissingResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out.Missing
}

// TestAdoptPathAcrossCaches is the point of adoption: a path already stored in
// one cache is registered into another on the same backend without the client
// dumping, chunking or uploading anything — and the adopting cache really
// serves it afterwards.
func TestAdoptPathAcrossCaches(t *testing.T) {
	_, db, ts := newTestServerCfg(t, nil)
	if _, err := db.CreateCache("default", "src", true, 40); err != nil {
		t.Fatal(err)
	}
	dst, err := db.CreateCache("default", "dst", true, 40)
	if err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte("adopt-me-"), 500)
	_, narHash, _ := fakeNar(data)
	pushFake(t, ts, "src", h32, data, "")

	// dst is empty: without the NAR hash the path is simply missing.
	if got := missingPaths(t, ts, "dst", "", []api.PathRef{{Hash: h32}}); len(got) != 1 {
		t.Fatalf("without a NarHash: missing = %v, want the path to stay missing", got)
	}
	if _, err := db.GetPath(dst.ID, h32); err == nil {
		t.Fatal("path registered in dst without adoption")
	}

	// With the NAR hash, the server adopts it.
	if got := missingPaths(t, ts, "dst", "", []api.PathRef{{Hash: h32, NarHash: narHash}}); len(got) != 0 {
		t.Fatalf("missing = %v, want none (adopted)", got)
	}
	p, err := db.GetPath(dst.ID, h32)
	if err != nil {
		t.Fatalf("adopted path not in dst: %v", err)
	}
	if p.NarHash != narHash || p.NarSize != uint64(len(data)) || len(p.Chunks) != 1 {
		t.Fatalf("adopted path corrupted: %+v", p)
	}

	// And dst serves it for real: narinfo + NAR bytes, signed with dst's key.
	resp, err := http.Get(ts.URL + "/c/default/dst/" + h32 + ".narinfo")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("narinfo from dst: %d %s", resp.StatusCode, body)
	}
	verifyNarinfoSig(t, db, "dst", string(body))
	resp, err = http.Get(ts.URL + "/c/default/dst/nar/" + h32 + ".nar")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !bytes.Equal(got, data) {
		t.Fatalf("NAR from dst: %d, %d bytes (want %d)", resp.StatusCode, len(got), len(data))
	}
}

func TestAdoptRejects(t *testing.T) {
	_, db, ts := newTestServerCfg(t, nil)
	if _, err := db.CreateCache("default", "src", true, 40); err != nil {
		t.Fatal(err)
	}
	dst, err := db.CreateCache("default", "dst", true, 40)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("source bytes")
	_, narHash, _ := fakeNar(data)
	pushFake(t, ts, "src", h32, data, "")

	notAdopted := func(t *testing.T, refs []api.PathRef) {
		t.Helper()
		if got := missingPaths(t, ts, "dst", "", refs); len(got) != 1 {
			t.Fatalf("missing = %v, want the path NOT adopted", got)
		}
		if _, err := db.GetPath(dst.ID, refs[0].Hash); err == nil {
			t.Fatal("path was adopted anyway")
		}
	}

	// A NAR hash that doesn't match the stored one: the contents differ, so
	// adopting would serve bytes the pusher never had.
	t.Run("narHash mismatch", func(t *testing.T) {
		_, other, _ := fakeNar([]byte("different bytes entirely"))
		notAdopted(t, []api.PathRef{{Hash: h32, NarHash: other}})
	})

	t.Run("malformed narHash", func(t *testing.T) {
		notAdopted(t, []api.PathRef{{Hash: h32, NarHash: "not-a-hash"}})
	})

	// Only paths the server actually reported missing are adoption candidates;
	// a hash the client didn't ask about must not be touched.
	t.Run("hash not in the missing set", func(t *testing.T) {
		body, _ := json.Marshal(api.MissingReq{
			Hashes: []string{h32b},
			Paths:  []api.PathRef{{Hash: h32, NarHash: narHash}},
		})
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/c/default/dst/api/get-missing-paths", bytes.NewReader(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if _, err := db.GetPath(dst.ID, h32); err == nil {
			t.Fatal("adopted a path the client never asked about")
		}
	})

	// Source cache on another storage backend: its chunks can't serve dst.
	t.Run("different storage backend", func(t *testing.T) {
		src, err := db.GetCache("default", "src")
		if err != nil {
			t.Fatal(err)
		}
		if err := db.SetCacheStorage(src.ID, "elsewhere"); err != nil {
			t.Fatal(err)
		}
		defer db.SetCacheStorage(src.ID, "default")
		notAdopted(t, []api.PathRef{{Hash: h32, NarHash: narHash}})
	})

	// Chunks gone (swept between the push and the adoption): adopting would
	// register a dangling path that 500s on every pull.
	t.Run("source chunks missing", func(t *testing.T) {
		chunkHash, _, _ := fakeNar(data)
		if err := db.DeleteChunkRows("default", []string{chunkHash}); err != nil {
			t.Fatal(err)
		}
		notAdopted(t, []api.PathRef{{Hash: h32, NarHash: narHash}})
	})
}

// Adoption must not become a read primitive: knowing a store path and its NAR
// hash (both public for anything built from nixpkgs) may not import another
// tenant's private bytes.
func TestAdoptRequiresPullOnSource(t *testing.T) {
	_, db, ts := newTestServerCfg(t, nil)
	if _, err := db.CreateCache("default", "priv", false, 40); err != nil {
		t.Fatal(err)
	}
	dst, err := db.CreateCache("default", "mine", true, 40)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("private source bytes")
	_, narHash, _ := fakeNar(data)
	privPush, _, _ := db.CreateToken(0, "privpush", []string{"default/priv"}, []string{"push"}, 0)
	pushFake(t, ts, "priv", h32, data, privPush)

	refs := []api.PathRef{{Hash: h32, NarHash: narHash}}

	// A token that may push to mine but not pull priv: no adoption.
	pushOnly, _, _ := db.CreateToken(0, "dstpush", []string{"default/mine"}, []string{"push"}, 0)
	if got := missingPaths(t, ts, "mine", pushOnly, refs); len(got) != 1 {
		t.Fatalf("missing = %v, want the path NOT adopted from a cache this token can't pull", got)
	}
	if _, err := db.GetPath(dst.ID, h32); err == nil {
		t.Fatal("adopted from a private cache without pull permission")
	}

	// An admin token can read every cache anyway, so it may adopt.
	admin, _, _ := db.CreateToken(0, "boss", []string{"default/mine"}, []string{"push", "admin"}, 0)
	if got := missingPaths(t, ts, "mine", admin, refs); len(got) != 0 {
		t.Fatalf("missing = %v, want adopted (admin token)", got)
	}
	if _, err := db.GetPath(dst.ID, h32); err != nil {
		t.Fatalf("not adopted with an admin token: %v", err)
	}
}

// A token that may pull the private source may adopt from it: it could already
// fetch those bytes directly. Exercised at the handler helper, not over HTTP —
// a token is scoped to exactly one cache, so no single token can both push to
// the target and pull the source, and the pull branch is otherwise unreachable.
func TestAdoptWithPullScopedToken(t *testing.T) {
	s, db, ts := newTestServerCfg(t, nil)
	if _, err := db.CreateCache("default", "priv", false, 40); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateCache("default", "mine", true, 40); err != nil {
		t.Fatal(err)
	}
	// Read the cache back: CreateCache leaves Storage empty, while every server
	// read (and so the real handler) sees the column default.
	dst, err := db.GetCache("default", "mine")
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("shared source bytes")
	_, narHash, _ := fakeNar(data)
	privPush, _, _ := db.CreateToken(0, "privpush", []string{"default/priv"}, []string{"push"}, 0)
	pushFake(t, ts, "priv", h32, data, privPush)

	refs := []api.PathRef{{Hash: h32, NarHash: narHash}}
	req := func(token string) *http.Request {
		r, _ := http.NewRequest(http.MethodPost, "/", nil)
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		return r
	}

	// No pull rights on priv: not adopted.
	if got := s.adopt(req(""), dst, []string{h32}, refs); len(got) != 1 {
		t.Fatalf("anonymous: missing = %v, want not adopted", got)
	}
	// Pull-scoped on priv: adopted.
	privPull, _, _ := db.CreateToken(0, "privpull", []string{"default/priv"}, []string{"pull"}, 0)
	if got := s.adopt(req(privPull), dst, []string{h32}, refs); len(got) != 0 {
		t.Fatalf("with pull on the source: missing = %v, want adopted", got)
	}
	if _, err := db.GetPath(dst.ID, h32); err != nil {
		t.Fatalf("not adopted with pull permission on the source: %v", err)
	}
}

// A public source cache is adoptable by anyone, since anyone may pull it.
func TestAdoptFromPublicSourceNeedsNoPullToken(t *testing.T) {
	_, db, ts := newTestServerCfg(t, nil)
	if _, err := db.CreateCache("default", "pub", true, 40); err != nil {
		t.Fatal(err)
	}
	dst, err := db.CreateCache("default", "mine", false, 40)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("public bytes")
	_, narHash, _ := fakeNar(data)
	pushFake(t, ts, "pub", h32, data, "")

	pushOnly, _, _ := db.CreateToken(0, "p", []string{"default/mine"}, []string{"push"}, 0)
	if got := missingPaths(t, ts, "mine", pushOnly, []api.PathRef{{Hash: h32, NarHash: narHash}}); len(got) != 0 {
		t.Fatalf("missing = %v, want adopted from the public cache", got)
	}
	if _, err := db.GetPath(dst.ID, h32); err != nil {
		t.Fatalf("not adopted from public source: %v", err)
	}
}

// Adoption re-stamps the chunks it takes over, so the sweeper's grace window
// covers them for the rest of the pushing client's run.
func TestAdoptTouchesChunks(t *testing.T) {
	_, db, ts := newTestServerCfg(t, nil)
	if _, err := db.CreateCache("default", "src", true, 40); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateCache("default", "dst", true, 40); err != nil {
		t.Fatal(err)
	}
	data := []byte("stamp me")
	chunkHash, narHash, _ := fakeNar(data)
	pushFake(t, ts, "src", h32, data, "")
	if err := db.TouchChunks("default", []string{chunkHash}, 1); err != nil {
		t.Fatal(err)
	}

	if got := missingPaths(t, ts, "dst", "", []api.PathRef{{Hash: h32, NarHash: narHash}}); len(got) != 0 {
		t.Fatalf("missing = %v, want adopted", got)
	}
	all, err := db.AllChunks("default")
	if err != nil {
		t.Fatal(err)
	}
	for _, ch := range all {
		if ch.Hash == chunkHash && ch.Created <= 1 {
			t.Fatalf("chunk created = %d, want re-stamped to now", ch.Created)
		}
	}
}

// An old client sends no Paths at all: everything still works, nothing adopted.
func TestMissingPathsWithoutRefsStillWorks(t *testing.T) {
	_, db, ts := newTestServerCfg(t, nil)
	if _, err := db.CreateCache("default", "c", true, 40); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(api.MissingReq{Hashes: []string{h32, h32b}})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/c/default/c/api/get-missing-paths", bytes.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out api.MissingResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Missing) != 2 {
		t.Fatalf("missing = %v, want both", out.Missing)
	}
}
