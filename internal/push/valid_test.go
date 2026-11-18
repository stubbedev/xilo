package push

import (
	"context"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stubbedev/xilo/internal/testenv"
)

func TestValidPathsDropsPathsThatNeverBecameValid(t *testing.T) {
	bin, err := exec.LookPath("nix-store")
	if err != nil {
		testenv.Need(t, "nix-store on PATH", err.Error())
	}
	resolved, err := filepath.EvalSymlinks(bin)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.SplitN(strings.TrimPrefix(resolved, "/nix/store/"), "/", 2)
	if !strings.HasPrefix(resolved, "/nix/store/") || len(parts) < 2 {
		testenv.Need(t, "nix-store inside /nix/store", resolved)
	}
	valid := "/nix/store/" + parts[0]
	failed := "/nix/store/" + strings.Repeat("0", 32) + "-a-build-that-failed"
	got, err := ValidPaths(context.Background(), []string{failed, valid, failed + "-too"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{valid}) {
		t.Fatalf("ValidPaths = %v, want only %s", got, valid)
	}
	if got, err := ValidPaths(context.Background(), nil); err != nil || got != nil {
		t.Fatalf("ValidPaths(nil) = %v, %v", got, err)
	}
}
