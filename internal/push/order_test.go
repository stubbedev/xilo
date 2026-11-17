package push

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"testing"

	"github.com/stubbedev/xilo/internal/api"
	"github.com/stubbedev/xilo/internal/chunk"
	"github.com/stubbedev/xilo/internal/narinfo"
)

func storePath(i int, name string) string {
	return fmt.Sprintf("/nix/store/%032d-%s", i, name)
}

func hashesOf(infos []pathInfo) ([]string, map[string]pathInfo) {
	byHash := map[string]pathInfo{}
	var hashes []string
	for _, in := range infos {
		h := narinfo.StoreHash(in.Path)
		byHash[h] = in
		hashes = append(hashes, h)
	}
	return hashes, byHash
}

func assertReferencesFirst(t *testing.T, published []string, infos []pathInfo) {
	t.Helper()
	byPath := map[string]pathInfo{}
	for _, in := range infos {
		byPath[in.Path] = in
	}
	seen := map[string]bool{}
	for _, p := range published {
		for _, ref := range byPath[p].References {
			if ref != p && !seen[ref] {
				t.Fatalf("%s published before its reference %s (order %v)", p, ref, published)
			}
		}
		seen[p] = true
	}
}

func TestPublishOrderPutsReferencesFirst(t *testing.T) {
	lib, dep, mid, app := storePath(4, "lib"), storePath(3, "dep"), storePath(2, "mid"), storePath(1, "app")
	infos := []pathInfo{
		{Path: app, References: []string{app, mid, dep}},
		{Path: mid, References: []string{lib, mid}},
		{Path: dep, References: []string{lib}},
		{Path: lib, References: []string{lib}},
	}
	hashes, byHash := hashesOf(infos)
	plan, err := planPublish(hashes, byHash)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, h := range plan.order {
		paths = append(paths, byHash[h].Path)
	}
	if want := []string{lib, mid, dep, app}; !slices.Equal(paths, want) {
		t.Fatalf("order = %v, want %v", paths, want)
	}
	assertReferencesFirst(t, paths, infos)
}

func TestPublishOrderIgnoresReferencesAlreadyCached(t *testing.T) {
	cached, app := storePath(2, "cached"), storePath(1, "app")
	_, byHash := hashesOf([]pathInfo{{Path: app, References: []string{cached}}, {Path: cached}})
	plan, err := planPublish([]string{narinfo.StoreHash(app)}, byHash)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.order) != 1 || byHash[plan.order[0]].Path != app {
		t.Fatalf("order = %v, want only %s", plan.order, app)
	}
}

func TestPublishOrderRejectsCycle(t *testing.T) {
	a, b, c := storePath(1, "a"), storePath(2, "b"), storePath(3, "c")
	hashes, byHash := hashesOf([]pathInfo{
		{Path: a, References: []string{b}},
		{Path: b, References: []string{a}},
		{Path: c},
	})
	_, err := planPublish(hashes, byHash)
	if err == nil || !strings.Contains(err.Error(), a) || !strings.Contains(err.Error(), b) || strings.Contains(err.Error(), c) {
		t.Fatalf("err = %v, want a cycle error naming %s and %s only", err, a, b)
	}
}

type chainFixture struct {
	infos []pathInfo
	nars  map[string][]byte
	paths []string
}

func chain(n int) chainFixture {
	var fx chainFixture
	fx.nars = map[string][]byte{}
	for i := range n {
		p := storePath(i+1, fmt.Sprintf("link%d", i))
		fx.paths = append(fx.paths, p)
	}
	for i, p := range fx.paths {
		size := 64 << 10
		if i == 0 {
			size = 16
		}
		nar := make([]byte, size)
		rand.New(rand.NewSource(int64(i))).Read(nar)
		var refs []string
		if i+1 < n {
			refs = []string{p, fx.paths[i+1]}
		}
		fx.infos = append(fx.infos, pathInfo{Path: p, NarHash: fmt.Sprintf("sha256:%d", i), NarSize: uint64(len(nar)), References: refs})
		fx.nars[narinfo.BaseName(p)] = nar
	}
	return fx
}

func publishedPaths(f *fakeServer) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, p := range f.paths {
		out = append(out, p.StorePath)
	}
	return out
}

func TestPushPublishesReferencesBeforeReferrers(t *testing.T) {
	fx := chain(6)
	fakeNix(t, pathInfoJSON(t, fx.infos), fx.nars)
	f := newFakeServer(t, baseCfg())

	if err := newTestClient(f, "", 8).Push(context.Background(), fx.paths[:1]); err != nil {
		t.Fatal(err)
	}
	published := publishedPaths(f)
	if len(published) != len(fx.paths) {
		t.Fatalf("published %d paths, want %d", len(published), len(fx.paths))
	}
	assertReferencesFirst(t, published, fx.infos)
}

func TestPushFailedReferenceLeavesNoDanglingNarinfo(t *testing.T) {
	fastRetries(t)
	fx := chain(5)
	fakeNix(t, pathInfoJSON(t, fx.infos), fx.nars)
	f := newFakeServer(t, baseCfg())
	broken := fx.paths[2]
	f.failChunks = map[string]bool{}
	params := chunk.Params{MinSize: 64, AvgSize: 256, MaxSize: 1024}
	if err := chunk.Split(strings.NewReader(string(fx.nars[narinfo.BaseName(broken)])), params, func(c chunk.Chunk) error {
		f.failChunks[c.Hash] = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	err := newTestClient(f, "", 8).Push(context.Background(), fx.paths[:1])
	if err == nil || !strings.Contains(err.Error(), broken) {
		t.Fatalf("err = %v, want the failure of %s", err, broken)
	}
	if errors.Is(err, errReferenceNotPushed) {
		t.Fatalf("err = %v, want the root cause, not a skipped referrer", err)
	}
	published := publishedPaths(f)
	assertReferencesFirst(t, published, fx.infos)
	for _, p := range fx.paths[:3] {
		if slices.Contains(published, p) {
			t.Fatalf("%s published although its closure is incomplete: %v", p, published)
		}
	}
}

func TestPushCancelledMidwayLeavesNoDanglingNarinfo(t *testing.T) {
	fx := chain(6)
	fakeNix(t, pathInfoJSON(t, fx.infos), fx.nars)
	f := newFakeServer(t, baseCfg())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.afterPathPut = func(api.PathReq) { cancel() }

	if err := newTestClient(f, "", 8).Push(ctx, fx.paths[:1]); err == nil {
		t.Fatal("push succeeded although it was cancelled after the first narinfo")
	}
	published := publishedPaths(f)
	if len(published) == 0 || len(published) == len(fx.paths) {
		t.Fatalf("published %d of %d paths, want a partial push", len(published), len(fx.paths))
	}
	assertReferencesFirst(t, published, fx.infos)
}
