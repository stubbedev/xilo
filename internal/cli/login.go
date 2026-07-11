package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

func loginCmd() *cobra.Command {
	var token, name, cache string
	var makeDefault bool
	c := &cobra.Command{
		Use:   "login <url>",
		Short: "Save a server profile for push/use (~/.config/xilo/config.yaml)",
		Long: "Save a server URL + token as a named profile. The first profile (or one\n" +
			"saved with --default) becomes the default for push/use/cache commands;\n" +
			"pick another with --profile.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if token == "" {
				token = os.Getenv("XILO_TOKEN")
			}
			cc := loadClientConfig()
			if cc.Servers == nil {
				cc.Servers = map[string]serverProfile{}
			}
			p := cc.Servers[name]
			p.URL = strings.TrimRight(args[0], "/")
			if token != "" {
				p.Token = token
			}
			if cache != "" {
				p.Cache = normRef(cache)
			}
			cc.Servers[name] = p
			if makeDefault || cc.Default == "" {
				cc.Default = name
			}
			if err := saveClientConfig(cc); err != nil {
				return err
			}
			fmt.Printf("%s profile %q → %s\n", styleOK("saved"), name, p.URL)
			if cc.Default == name {
				fmt.Println(styleDim("this is the default profile"))
			}
			return nil
		},
	}
	c.Flags().StringVar(&token, "token", "", "token to save (env XILO_TOKEN)")
	c.Flags().StringVar(&name, "name", "default", "profile name")
	c.Flags().StringVar(&cache, "cache", "", "default push/use target on this server (ns/cache)")
	c.Flags().BoolVar(&makeDefault, "default", false, "make this the default profile")
	return c
}
