package push

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestParsePathInfo(t *testing.T) {
	t.Run("array form", func(t *testing.T) {
		out := []byte(`[{"path":"/nix/store/aaa-x","narHash":"sha256:abc","narSize":1,"references":[],"deriver":""}]`)
		got, err := parsePathInfo(out)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Path != "/nix/store/aaa-x" || got[0].NarHash != "sha256:abc" || got[0].NarSize != 1 {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("object form backfills path from key", func(t *testing.T) {
		out := []byte(`{"/nix/store/aaa-x":{"narHash":"sha256:abc","narSize":1}}`)
		got, err := parsePathInfo(out)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Path != "/nix/store/aaa-x" {
			t.Fatalf("path not backfilled: %+v", got)
		}
	})

	t.Run("object form keeps inner path when set", func(t *testing.T) {
		out := []byte(`{"/nix/store/key-x":{"path":"/nix/store/inner-y","narHash":"sha256:abc","narSize":1}}`)
		got, err := parsePathInfo(out)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Path != "/nix/store/inner-y" {
			t.Fatalf("inner path not kept: %+v", got)
		}
	})

	t.Run("missing deriver is not an error", func(t *testing.T) {
		out := []byte(`[{"path":"/nix/store/aaa-x","narHash":"sha256:abc","narSize":1}]`)
		got, err := parsePathInfo(out)
		if err != nil {
			t.Fatal(err)
		}
		if got[0].Deriver != "" {
			t.Fatalf("deriver = %q, want empty", got[0].Deriver)
		}
	})

	t.Run("references null and absent are nil", func(t *testing.T) {
		for _, out := range [][]byte{
			[]byte(`[{"path":"/nix/store/aaa-x","narHash":"sha256:abc","narSize":1,"references":null}]`),
			[]byte(`[{"path":"/nix/store/aaa-x","narHash":"sha256:abc","narSize":1}]`),
		} {
			got, err := parsePathInfo(out)
			if err != nil {
				t.Fatal(err)
			}
			if got[0].References != nil {
				t.Fatalf("references = %+v, want nil", got[0].References)
			}
		}
	})

	t.Run("leading whitespace before bracket", func(t *testing.T) {
		out := []byte("  \n\t[{\"path\":\"/nix/store/aaa-x\",\"narHash\":\"sha256:abc\",\"narSize\":1}]")
		got, err := parsePathInfo(out)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Path != "/nix/store/aaa-x" {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("signatures parsed", func(t *testing.T) {
		out := []byte(`[{"path":"/nix/store/aaa-x","narHash":"sha256:abc","narSize":1,"signatures":["k:sig"]}]`)
		got, err := parsePathInfo(out)
		if err != nil {
			t.Fatal(err)
		}
		if len(got[0].Signatures) != 1 || got[0].Signatures[0] != "k:sig" {
			t.Fatalf("signatures = %+v", got[0].Signatures)
		}
	})

	t.Run("malformed json errors", func(t *testing.T) {
		if _, err := parsePathInfo([]byte(`[{not json`)); err == nil {
			t.Fatal("expected error")
		}
		if _, err := parsePathInfo([]byte(`{not json`)); err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestSignedByUpstream(t *testing.T) {
	newC := func(keys []string) *Client {
		c := NewClient("http://x", "cache", "", 0)
		c.upstreamKeys = keys
		return c
	}

	if newC(nil).signedByUpstream(pathInfo{Signatures: []string{"cache.nixos.org-1:abc"}}) {
		t.Fatal("empty upstreamKeys should be false")
	}
	if !newC([]string{"cache.nixos.org-1"}).signedByUpstream(pathInfo{Signatures: []string{"cache.nixos.org-1:abc"}}) {
		t.Fatal("matching sig should be true")
	}
	if newC([]string{"cache.nixos.org-1"}).signedByUpstream(pathInfo{Signatures: []string{"other-1:abc"}}) {
		t.Fatal("non-matching name should be false")
	}
	if newC([]string{"cache.nixos.org-1"}).signedByUpstream(pathInfo{Signatures: []string{"nocolon"}}) {
		t.Fatal("sig with no colon should be false")
	}
	if !newC([]string{"cache.nixos.org-1"}).signedByUpstream(pathInfo{Signatures: []string{"other-1:abc", "cache.nixos.org-1:def"}}) {
		t.Fatal("one matching sig among many should be true")
	}
}

func TestEachParallelStopsAfterError(t *testing.T) {
	items := make([]int, 100)
	for i := range items {
		items[i] = i
	}
	var ran atomic.Int64
	errBoom := errors.New("boom")
	// jobs=1 makes processing order deterministic: fn returns error on the 5th.
	err := eachParallel(items, 1, func(int) error {
		if ran.Add(1) == 5 {
			return errBoom
		}
		return nil
	})
	if !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if n := ran.Load(); n == 100 {
		t.Fatalf("all %d items ran; should have stopped after error", n)
	}
}

func TestEachParallelConcurrencyBounded(t *testing.T) {
	items := make([]int, 20)
	const jobs = 3
	var inFlight, max atomic.Int64
	err := eachParallel(items, jobs, func(int) error {
		cur := inFlight.Add(1)
		for {
			m := max.Load()
			if cur <= m || max.CompareAndSwap(m, cur) {
				break
			}
		}
		time.Sleep(2 * time.Millisecond)
		inFlight.Add(-1)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if m := max.Load(); m > jobs {
		t.Fatalf("max in-flight = %d, exceeds jobs = %d", m, jobs)
	}
}

func TestEachParallelAllSuccess(t *testing.T) {
	if err := eachParallel([]int{1, 2, 3}, 2, func(int) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestEachParallelEmpty(t *testing.T) {
	called := false
	if err := eachParallel([]int{}, 4, func(int) error { called = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("fn called on empty slice")
	}
}

func TestEachParallelZeroJobsNoDeadlock(t *testing.T) {
	done := make(chan error, 1)
	go func() {
		done <- eachParallel([]int{1, 2, 3}, 0, func(int) error { return nil })
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("jobs=0 deadlocked")
	}
}

func TestPathReq(t *testing.T) {
	in := pathInfo{
		Path:       "/nix/store/aaa-x",
		NarHash:    "sha256:abc",
		NarSize:    42,
		Deriver:    "ccc.drv",
		References: []string{"/nix/store/bbb-y"},
	}
	req := in.pathReq([]string{"c1", "c2"})
	if req.StorePath != in.Path || req.NarHash != in.NarHash || req.NarSize != in.NarSize ||
		req.Deriver != in.Deriver || len(req.References) != 1 || req.References[0] != "/nix/store/bbb-y" ||
		len(req.Chunks) != 2 || req.Chunks[0] != "c1" || req.Chunks[1] != "c2" {
		t.Fatalf("mismapped: %+v", req)
	}
}
