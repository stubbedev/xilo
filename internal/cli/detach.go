package cli

// Detached push: re-exec ourselves in a new session and return, so a Nix
// post-build-hook (which Nix runs synchronously, blocking the build) doesn't
// wait for the upload. Deliberately no daemon and no spool — one child process
// per invocation, which is what a post-build-hook already gives us.

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"

	"github.com/stubbedev/xilo/internal/push"
)

// detachEnv marks the re-exec'd child, so it pushes instead of detaching again.
const detachEnv = "XILO_DETACHED"

// detachMaxPaths bounds what goes on the child's command line (argv has an OS
// size limit, and a closure of this size is a bulk push, not a build hook).
//
// ponytail: a spool file would lift the cap; add one if someone actually pushes
// thousands of paths per hook.
const detachMaxPaths = 512

// detachExe resolves the binary to re-exec; overridden in tests.
var detachExe = os.Executable

type detachReq struct {
	cache string
	paths []string
	url   string
	token string
	jobs  int
	quiet bool
}

// detachArgs builds the child's argv (minus the program name). The token is NOT
// here on purpose: argv is world-readable through ps, so it travels in the
// child's environment instead.
func detachArgs(r detachReq) []string {
	args := make([]string, 0, len(r.paths)+6)
	args = append(args, "push", r.cache)
	args = append(args, r.paths...)
	// The child writes to a log file, so its progress bar would only add
	// control characters; --url pins the server the parent already resolved,
	// leaving the child independent of profile changes.
	args = append(args, "--quiet", "--url", r.url)
	if r.jobs > 0 {
		args = append(args, "--jobs", strconv.Itoa(r.jobs))
	}
	return args
}

func detachEnviron(token string) []string {
	env := append(os.Environ(), detachEnv+"=1")
	if token != "" {
		env = append(env, "XILO_TOKEN="+token)
	}
	return env
}

// detachPush starts the background push and returns as soon as it is running.
func detachPush(r detachReq) error {
	if len(r.paths) > detachMaxPaths {
		return fmt.Errorf("--detach handles at most %d paths at a time (got %d); run without --detach, or use `xilo watch`",
			detachMaxPaths, len(r.paths))
	}
	exe, err := detachExe()
	if err != nil {
		return fmt.Errorf("locate own binary for --detach: %w", err)
	}
	logFile, logPath, err := push.OpenLog()
	if err != nil {
		return err
	}
	defer logFile.Close() // the child keeps its own descriptor

	cmd := exec.Command(exe, detachArgs(r)...)
	cmd.Env = detachEnviron(r.token)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.Stdin = nil
	// New session: the child must survive the hook's process group being
	// killed, and must not hold the build's terminal. Unix-only, which matches
	// every platform xilo releases for.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start detached push: %w", err)
	}
	// Release so we don't leave a zombie when we exit without waiting; the
	// child is reparented to init.
	if err := cmd.Process.Release(); err != nil {
		return err
	}
	if !r.quiet {
		fmt.Printf("pushing %d paths in the background (log: %s)\n", len(r.paths), logPath)
	}
	return nil
}
