//go:build linux || darwin

package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

type scanHarness struct {
	t    *testing.T
	dir  string
	scan *storeScan
}

func newScanHarness(t *testing.T) *scanHarness {
	t.Helper()
	dir := t.TempDir()
	scan, err := newStoreScan(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := scan.Close(); err != nil {
			t.Error(err)
		}
	})
	return &scanHarness{t: t, dir: dir, scan: scan}
}

func (h *scanHarness) touch(names ...string) {
	h.t.Helper()
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(h.dir, n), nil, 0o644); err != nil {
			h.t.Fatal(err)
		}
	}
}

func (h *scanHarness) remove(names ...string) {
	h.t.Helper()
	for _, n := range names {
		if err := os.Remove(filepath.Join(h.dir, n)); err != nil {
			h.t.Fatal(err)
		}
	}
}

func (h *scanHarness) listed(names ...string) []string {
	h.t.Helper()
	var got []string
	h.scan.begin()
	for _, n := range names {
		if err := h.scan.entry([]byte(n)); err != nil {
			h.t.Fatal(err)
		}
	}
	if err := h.scan.finish(func(p string) { got = append(got, filepath.Base(p)) }); err != nil {
		h.t.Fatal(err)
	}
	return got
}

func (h *scanHarness) rescan() []string {
	h.t.Helper()
	entries, err := os.ReadDir(h.dir)
	if err != nil {
		h.t.Fatal(err)
	}
	names := []string{".", ".."}
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return h.listed(names...)
}

func (h *scanHarness) expect(got []string, want ...string) {
	h.t.Helper()
	slices.Sort(got)
	if !slices.Equal(got, want) {
		h.t.Fatalf("reported %q, want %q", got, want)
	}
}

func TestStoreScanReportsPathsOnceTheirLockGoes(t *testing.T) {
	h := newScanHarness(t)
	h.touch("old-pkg", "building-pkg", "building-pkg.lock")
	h.expect(h.rescan())

	h.touch("new-pkg", "slow-pkg.lock", "slow-pkg")
	h.expect(h.rescan(), "new-pkg")
	h.expect(h.rescan())

	h.remove("building-pkg.lock")
	h.expect(h.rescan(), "building-pkg")

	h.remove("slow-pkg.lock")
	h.expect(h.rescan(), "slow-pkg")
	h.expect(h.rescan())
}

func TestStoreScanFiltersDerivationsAndSources(t *testing.T) {
	h := newScanHarness(t)
	h.expect(h.rescan())
	h.touch("a-hello.drv", "b-nixpkgs-source", ".links", "c-hello")
	h.expect(h.rescan(), "c-hello")
}

func TestStoreScanWaitsForALockTheListingMissed(t *testing.T) {
	h := newScanHarness(t)
	h.expect(h.rescan())
	h.touch("torn-pkg.lock", "torn-pkg")
	h.expect(h.listed("torn-pkg"))
	h.remove("torn-pkg.lock")
	h.expect(h.rescan(), "torn-pkg")
}

func TestStoreScanReportsARebuiltPathAgain(t *testing.T) {
	h := newScanHarness(t)
	h.touch("pkg")
	h.expect(h.rescan())
	h.remove("pkg")
	h.expect(h.rescan())
	if n := h.scan.present.Len(); n != 0 {
		t.Fatalf("listing still holds %d names after the store emptied", n)
	}
	h.touch("pkg")
	h.expect(h.rescan(), "pkg")
}

func TestStoreScanDropsALockThatLeftNoPath(t *testing.T) {
	h := newScanHarness(t)
	h.expect(h.rescan())
	h.touch("failed-pkg.lock")
	h.expect(h.rescan())
	h.remove("failed-pkg.lock")
	h.expect(h.rescan())
	if len(h.scan.locked) != 0 {
		t.Fatalf("locks still tracked: %v", h.scan.locked)
	}
}

func TestStoreScanRescanAllocatesNothingForKnownNames(t *testing.T) {
	h := newScanHarness(t)
	names := make([][]byte, 10000)
	for i := range names {
		names[i] = fmt.Appendf(nil, "%032d-pkg", i)
	}
	scan := func() {
		h.scan.begin()
		for _, n := range names {
			if err := h.scan.entry(n); err != nil {
				t.Fatal(err)
			}
		}
		if err := h.scan.finish(func(string) {}); err != nil {
			t.Fatal(err)
		}
	}
	scan()
	scan()
	if allocs := testing.AllocsPerRun(10, scan); allocs != 0 {
		t.Fatalf("rescan of an unchanged store allocated %v times", allocs)
	}
}
