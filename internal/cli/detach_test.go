package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stubbedev/xilo/internal/push"
)

func TestDetachArgs(t *testing.T) {
	r := detachReq{cache: "acme/web", paths: []string{"/nix/store/a-x", "/nix/store/b-y"}, url: "http://srv", token: "sekret", jobs: 3}
	args := detachArgs(r)
	want := []string{"push", "acme/web", "/nix/store/a-x", "/nix/store/b-y", "--quiet", "--url", "http://srv", "--jobs", "3"}
	if strings.Join(args, " ") != strings.Join(want, " ") {
		t.Fatalf("args = %v, want %v", args, want)
	}
	// The token must never reach argv: ps would show it to every local user.
	for _, a := range args {
		if strings.Contains(a, "sekret") {
			t.Fatalf("token leaked into argv: %v", args)
		}
	}
	// jobs=0 means "server decides" — don't pin it on the child.
	r.jobs = 0
	if got := strings.Join(detachArgs(r), " "); strings.Contains(got, "--jobs") {
		t.Fatalf("args = %q, want no --jobs", got)
	}
}

func TestDetachEnviron(t *testing.T) {
	env := detachEnviron("sekret")
	var marker, token bool
	for _, e := range env {
		switch e {
		case detachEnv + "=1":
			marker = true
		case "XILO_TOKEN=sekret":
			token = true
		}
	}
	if !marker {
		t.Fatalf("child env lacks %s=1; it would detach again in a loop", detachEnv)
	}
	if !token {
		t.Fatal("child env lacks the token")
	}
	if len(detachEnviron("")) != len(os.Environ())+1 {
		t.Fatal("empty token should add nothing beyond the marker")
	}
}

func TestDetachPushSpawnsAndReturns(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	t.Setenv("XILO_CACHE_DIR", state)

	// Stand-in for the xilo binary: records argv + the two env values we care
	// about, then exits. Proves the spawn really happens (and that we don't
	// wait for it).
	dir := t.TempDir()
	out := filepath.Join(dir, "recorded")
	script := filepath.Join(dir, "fake-xilo")
	body := "#!/bin/sh\n{ echo \"args: $*\"; echo \"marker: $" + detachEnv + "\"; echo \"token: $XILO_TOKEN\"; } > " + out + "\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	detachExe = func() (string, error) { return script, nil }
	t.Cleanup(func() { detachExe = os.Executable })

	err := detachPush(detachReq{
		cache: "acme/web", paths: []string{"/nix/store/a-x"},
		url: "http://srv", token: "sekret", quiet: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	var data []byte
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		if data, err = os.ReadFile(out); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	got := string(data)
	if got == "" {
		t.Fatal("detached child never ran")
	}
	if !strings.Contains(got, "args: push acme/web /nix/store/a-x --quiet --url http://srv") {
		t.Errorf("child argv wrong:\n%s", got)
	}
	if !strings.Contains(got, "marker: 1") {
		t.Errorf("child missing the detach marker:\n%s", got)
	}
	if !strings.Contains(got, "token: sekret") {
		t.Errorf("child missing the token:\n%s", got)
	}
	// The log file the child inherits must exist and be reported.
	if _, err := os.Stat(filepath.Join(state, "push.log")); err != nil {
		t.Errorf("push log not created: %v", err)
	}
}

func TestDetachPushRejectsTooManyPaths(t *testing.T) {
	t.Setenv("XILO_CACHE_DIR", filepath.Join(t.TempDir(), "state"))
	paths := make([]string, detachMaxPaths+1)
	for i := range paths {
		paths[i] = "/nix/store/x"
	}
	err := detachPush(detachReq{cache: "c", paths: paths, url: "http://srv"})
	if err == nil || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("err = %v, want a cap error naming the limit", err)
	}
}

func TestOpenLogTruncatesWhenHuge(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	t.Setenv("XILO_CACHE_DIR", state)

	f, path, err := push.OpenLog()
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("first run\n")
	f.Close()

	if err := os.Truncate(path, 8<<20); err != nil { // grow past the cap
		t.Fatal(err)
	}
	f, _, err = push.OpenLog()
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() > 1<<20 {
		t.Fatalf("log is %d bytes; an oversized log must be dropped, not appended to", fi.Size())
	}
}
