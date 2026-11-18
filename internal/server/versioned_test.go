package server

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stubbedev/xilo/internal/store"
)

func TestVersionedHoldsOneVersionWithinItsLimit(t *testing.T) {
	c := newVersioned[string, int](3)
	v1, v2 := store.Version{1}, store.Version{2}

	c.put(v1, "a", 1, 1)
	c.put(v1, "a", 9, 1)
	c.put(v1, "b", 2, 2)
	if got, ok := c.get(v1, "a"); !ok || got != 1 {
		t.Fatalf("a at v1 = %d, %v; want the first value stored", got, ok)
	}
	if _, ok := c.get(v1, "b"); !ok {
		t.Fatal("b at v1 missing, so the repeated put of a was charged twice")
	}
	c.put(v1, "c", 3, 1)
	if _, ok := c.get(v1, "c"); ok {
		t.Fatal("c stored past the limit")
	}
	if _, ok := c.get(v2, "a"); ok {
		t.Fatal("a answered for a version it was not read at")
	}

	c.put(v2, "c", 3, 1)
	if _, ok := c.get(v2, "c"); !ok {
		t.Fatal("c at v2 missing: a new version did not start empty")
	}
	if _, ok := c.get(v1, "a"); ok {
		t.Fatal("v1 entry survived the move to v2")
	}
}

func statusOf(t *testing.T, ts *httptest.Server, path string) int {
	t.Helper()
	resp := rawGet(t, ts, path, "", "")
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

func okBody(t *testing.T, ts *httptest.Server, path string) string {
	t.Helper()
	resp := rawGet(t, ts, path, "", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %d", path, resp.StatusCode)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestRememberedAnswersFollowEveryWrite(t *testing.T) {
	s, db, ts := newTestServer(t, true)
	c, err := db.CreateCache("default", "c", true, 40)
	if err != nil {
		t.Fatal(err)
	}
	narinfoPath := "/c/default/c/" + h32 + ".narinfo"
	narPath := "/c/default/c/nar/" + h32 + ".nar"
	ref := pathRef{cacheID: c.ID, storeHash: h32}
	held := func() store.Version {
		t.Helper()
		v, ok := db.Version()
		if !ok {
			t.Fatal("the test database reports no version")
		}
		return v
	}

	if got := statusOf(t, ts, narinfoPath); got != http.StatusNotFound {
		t.Fatalf("narinfo before the push: %d", got)
	}
	if _, ok := s.absent.get(held(), ref); !ok {
		t.Fatal("the miss was not remembered")
	}
	if got := statusOf(t, ts, narinfoPath); got != http.StatusNotFound {
		t.Fatalf("remembered miss answered %d", got)
	}

	first := bytes.Repeat([]byte("first-content-"), 500)
	pushFake(t, ts, "c", h32, first, "")
	before := okBody(t, ts, narinfoPath)
	if _, ok := s.narinfos.get(held(), ref); !ok {
		t.Fatal("the narinfo was not remembered")
	}
	if again := okBody(t, ts, narinfoPath); again != before {
		t.Fatalf("remembered narinfo differs:\n%s\n%s", before, again)
	}
	if got := getNar(t, ts, narPath, ""); !bytes.Equal(got, first) {
		t.Fatal("NAR differs from what was pushed")
	}
	if _, ok := s.narPlans.get(held(), ref); !ok {
		t.Fatal("the chunk plan was not remembered")
	}

	second := bytes.Repeat([]byte("second-content-"), 700)
	pushFake(t, ts, "c", h32, second, "")
	_, wantHash, _ := fakeNar(second)
	if after := okBody(t, ts, narinfoPath); !strings.Contains(after, "NarHash: "+wantHash+"\n") {
		t.Fatalf("narinfo after a re-push still describes the old NAR:\n%s", after)
	}
	if got := getNar(t, ts, narPath, ""); !bytes.Equal(got, second) {
		t.Fatal("NAR after a re-push is still the old one")
	}

	signed := okBody(t, ts, narinfoPath)
	if _, err := db.RotateKey(context.Background(), c.ID); err != nil {
		t.Fatal(err)
	}
	if rotated := okBody(t, ts, narinfoPath); rotated == signed {
		t.Fatal("narinfo after a key rotation still carries the old signature")
	}

	outside, err := store.Open(s.cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer outside.Close()
	if err := outside.UpdateCache(c.ID, false, c.Priority, c.Retention, c.MaxBytes); err != nil {
		t.Fatal(err)
	}
	if got := statusOf(t, ts, narinfoPath); got != http.StatusUnauthorized {
		t.Fatalf("narinfo of a cache another process made private: %d", got)
	}
	if got := statusOf(t, ts, narPath); got != http.StatusUnauthorized {
		t.Fatalf("NAR of a cache another process made private: %d", got)
	}
	if err := outside.UpdateCache(c.ID, true, c.Priority, c.Retention, c.MaxBytes); err != nil {
		t.Fatal(err)
	}
	if err := outside.SetAccountStatus(c.AccountID, store.StatusSuspended); err != nil {
		t.Fatal(err)
	}
	if got := statusOf(t, ts, narinfoPath); got != http.StatusForbidden {
		t.Fatalf("narinfo of an account another process suspended: %d", got)
	}
}
