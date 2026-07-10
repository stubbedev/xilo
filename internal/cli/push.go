package cli

import (
	"bufio"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/stubbedev/xilo/internal/push"
)

func pushCmd() *cobra.Command {
	var url, token string
	var jobs int
	var dryRun, quiet bool
	c := &cobra.Command{
		Use:   "push <cache> <path>...",
		Short: "Push store paths (and their closure) to a cache",
		Long: "Push store paths and their full closure to a cache.\n\n" +
			"Parallelism is automatic (the server advertises its capacity); override with --jobs.\n" +
			"Pass '-' as the path to read newline-separated store paths from stdin\n" +
			"(handy for a Nix post-build-hook).",
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			url, token = resolveServer(url, token)
			paths, err := resolvePaths(args[1:])
			if err != nil {
				return err
			}
			if len(paths) == 0 {
				return nil
			}
			cl := push.NewClient(url, args[0], token, jobs)
			cl.DryRun = dryRun
			cl.Quiet = quiet
			return cl.Push(cmd.Context(), paths)
		},
	}
	c.Flags().StringVar(&url, "url", "", "server base URL (env XILO_URL, default http://localhost:8080)")
	c.Flags().StringVar(&token, "token", "", "auth token (env XILO_TOKEN)")
	c.Flags().IntVar(&jobs, "jobs", 0, "parallel uploads (0 = auto, use server capacity)")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "show what would be pushed, upload nothing")
	c.Flags().BoolVar(&quiet, "quiet", false, "suppress progress output (for post-build-hook)")
	return c
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
