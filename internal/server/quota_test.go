package server

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stubbedev/xilo/internal/api"
	"github.com/stubbedev/xilo/internal/store"
)

// TestStorageQuotaReadOnly pins over-quota behavior: pushes 403, pulls keep
// working, data is never deleted.
func TestStorageQuotaReadOnly(t *testing.T) {
	_, db, ts := newTestServer(t, true)
	db.CreateCache("default", "q", true, 40)

	// Tiny plan: 1 byte of logical storage.
	plan, err := db.CreatePlan(&store.Plan{Name: "tiny", MaxStorage: 1})
	if err != nil {
		t.Fatal(err)
	}
	acc, err := db.GetAccount("default")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetAccountPlan(acc.ID, plan.ID); err != nil {
		t.Fatal(err)
	}

	// First push: under quota (0 < 1), lands.
	data := []byte("some data that will exceed one byte")
	pushFake(t, ts, "q", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", data, "")

	// Now logical bytes >= 1: path registration is rejected (chunk uploads
	// aren't individually gated; the path gate is what admits data).
	more := []byte("more")
	ch, narHash, narSize := fakeNar(more)
	r := put(t, ts, "/c/default/q/api/chunk/"+ch, more, "")
	r.Body.Close()
	body, _ := json.Marshal(api.PathReq{
		StorePath: "/nix/store/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-more",
		NarHash:   narHash, NarSize: narSize, Chunks: []string{ch},
	})
	if r := put(t, ts, "/c/default/q/api/path", body, ""); r.StatusCode != http.StatusForbidden {
		t.Fatalf("over-quota put-path → %d want 403", r.StatusCode)
	}

	// Pull still works.
	resp, _ := http.Get(ts.URL + "/c/default/q/nar/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.nar")
	if resp.StatusCode != 200 {
		t.Fatalf("over-quota pull → %d want 200", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestRetentionCeiling pins the plan clamp: a cache with no retention gets
// the plan ceiling applied by the sweep.
func TestRetentionCeiling(t *testing.T) {
	s, db, ts := newTestServer(t, true)
	db.CreateCache("default", "r", true, 40)
	data := []byte("old path data")
	pushFake(t, ts, "r", "cccccccccccccccccccccccccccccccc", data, "")

	// Backdate the path's accessed time far past any ceiling.
	c, _ := db.GetCache("default", "r")
	// minAge is hugely negative so the guard (now-accessed < minAge) never
	// short-circuits a backwards stamp.
	db.TouchPath(c.ID, "cccccccccccccccccccccccccccccccc", time.Now().Add(-48*time.Hour).Unix(), -1<<62)

	// Plan ceiling: 1h retention.
	plan, err := db.CreatePlan(&store.Plan{Name: "short", MaxRetention: 3600})
	if err != nil {
		t.Fatal(err)
	}
	acc, _ := db.GetAccount("default")
	if err := db.SetAccountPlan(acc.ID, plan.ID); err != nil {
		t.Fatal(err)
	}

	if _, _, err := s.runGC(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetPath(c.ID, "cccccccccccccccccccccccccccccccc"); err == nil {
		t.Fatal("path older than the plan ceiling survived the sweep")
	}
}

// TestEgressRollup pins metering: serving a NAR lands in the monthly rollup
// after a flush.
func TestEgressRollup(t *testing.T) {
	s, db, ts := newTestServer(t, true)
	db.CreateCache("default", "e", true, 40)
	data := []byte("bytes that leave the building")
	pushFake(t, ts, "e", "dddddddddddddddddddddddddddddddd", data, "")

	resp, _ := http.Get(ts.URL + "/c/default/e/nar/dddddddddddddddddddddddddddddddd.nar")
	if resp.StatusCode != 200 {
		t.Fatalf("pull → %d", resp.StatusCode)
	}
	resp.Body.Close()

	s.flushEgress()
	acc, _ := db.GetAccount("default")
	month := time.Now().UTC().Format("2006-01")
	if got := db.AccountEgress(acc.ID, month); got != int64(len(data)) {
		t.Fatalf("egress = %d want %d", got, len(data))
	}
}
