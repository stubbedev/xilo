package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

func loginCmd() *cobra.Command {
	var token string
	c := &cobra.Command{
		Use:   "login <url>",
		Short: "Save a server URL + token for push/use (~/.config/xilo/config.yaml)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if token == "" {
				token = os.Getenv("XILO_TOKEN")
			}
			cfg := clientConfig{URL: strings.TrimRight(args[0], "/"), Token: token}
			if err := saveClientConfig(cfg); err != nil {
				return err
			}
			fmt.Printf("saved %s to %s\n", cfg.URL, clientConfigPath())
			return nil
		},
	}
	c.Flags().StringVar(&token, "token", "", "token to save (env XILO_TOKEN)")
	return c
}
