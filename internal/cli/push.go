package cli

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/stubbedev/xilo/internal/push"
)

func pushCmd() *cobra.Command {
	var url, token string
	var jobs int
	var dryRun, quiet, detach bool
	c := &cobra.Command{
		Use:   "push [ns/cache] <path>...",
		Short: "Push store paths (and their closure) to a cache",
		Long: "Push store paths and their full closure to a cache.\n\n" +
			"The cache argument is optional once a default is saved (`xilo use <ns/cache>\n" +
			"--default` or `xilo login --cache`). Parallelism is automatic (the server\n" +
			"advertises its capacity); override with --jobs. Pass '-' as the path to read\n" +
			"newline-separated store paths from stdin (handy for a Nix post-build-hook).\n\n" +
			"Nix runs a post-build-hook synchronously, so the build waits for the push.\n" +
			"--detach hands the work to a background process and returns immediately;\n" +
			"progress and errors go to the log it prints. `xilo watch` (Linux) is the\n" +
			"other way to keep pushes off the build's critical path.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			url, token = resolveServer(url, token)
			if url == "" {
				return errNoServer
			}
			cache, rest := splitCacheArg(args)
			if cache == "" {
				return fmt.Errorf("no cache given and no default saved — `xilo push <ns/cache> <path>` or `xilo use <ns/cache> --default`")
			}
			if len(rest) == 0 {
				return fmt.Errorf("no paths to push")
			}
			paths, err := resolvePaths(rest)
			if err != nil {
				return err
			}
			if len(paths) == 0 {
				return nil
			}
			if detach && dryRun {
				return fmt.Errorf("--detach and --dry-run are mutually exclusive")
			}
			// The re-exec carries the marker so the child pushes for real.
			if detach && os.Getenv(detachEnv) == "" {
				return detachPush(detachReq{
					cache: cache, paths: paths, url: url, token: token, jobs: jobs, quiet: quiet,
				})
			}
			cl := push.NewClient(url, normRef(cache), token, jobs)
			cl.DryRun = dryRun
			cl.Quiet = quiet
			return cl.Push(cmd.Context(), paths)
		},
	}
	c.Flags().StringVar(&url, "url", "", "server base URL (env XILO_URL / saved login)")
	c.Flags().StringVar(&token, "token", "", "auth token (env XILO_TOKEN)")
	c.Flags().IntVar(&jobs, "jobs", 0, "parallel uploads (0 = auto, use server capacity)")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "show what would be pushed, upload nothing")
	c.Flags().BoolVar(&quiet, "quiet", false, "suppress progress output (for post-build-hook)")
	c.Flags().BoolVar(&detach, "detach", false, "push in the background and return immediately (keeps a post-build-hook off the build's critical path)")
	c.Flags().StringVarP(&profileFlag, "profile", "p", "", "saved server profile to use")
	return c
}

// splitCacheArg decides whether args[0] names a cache or is already a path
// (starts with /, ./, .. or is '-'/an existing file); paths fall back to the
// saved default cache.
func splitCacheArg(args []string) (cache string, paths []string) {
	first := args[0]
	looksPath := first == "-" || strings.HasPrefix(first, "/") || strings.HasPrefix(first, ".")
	if !looksPath {
		if _, err := os.Stat(first); err == nil {
			looksPath = true
		}
	}
	if looksPath {
		return defaultCache(), args
	}
	return first, args[1:]
}

// resolvePaths expands a lone "-" into newline-separated paths read from
// stdin, and drops empty arguments (a shell var that resolved to "" would
// otherwise reach nix as a bogus path).
func resolvePaths(args []string) ([]string, error) {
	if len(args) == 1 && args[0] == "-" {
		var paths []string
		sc := bufio.NewScanner(os.Stdin)
		sc.Buffer(make([]byte, 1<<20), 16<<20)
		for sc.Scan() {
			if p := strings.TrimSpace(sc.Text()); p != "" {
				paths = append(paths, p)
			}
		}
		return paths, sc.Err()
	}
	var paths []string
	for _, a := range args {
		if p := strings.TrimSpace(a); p != "" {
			paths = append(paths, p)
		}
	}
	return paths, nil
}
