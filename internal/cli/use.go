package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/stubbedev/xilo/internal/api"
)

const (
	blockStart = "# >>> xilo (managed) — edit via `xilo use`"
	blockEnd   = "# <<< xilo"
)

func useCmd() *cobra.Command {
	var url, token string
	var remove bool
	c := &cobra.Command{
		Use:   "use <ns/cache>",
		Short: "Configure local Nix to use a cache (nix.conf + netrc)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			url, token = resolveServer(url, token)
			cache := normRef(args[0])

			if remove {
				sub := strings.TrimRight(url, "/") + "/" + cache
				// The trusted-key label is the bare cache name.
				_, bare := splitRef(cache)
				if err := removeFromNixConf(sub, bare); err != nil {
					return err
				}
				fmt.Printf("removed %s from nix.conf\n", sub)
				return nil
			}

			cfg, err := fetchCacheConfig(cmd.Context(), url, cache, token)
			if err != nil {
				return err
			}
			sub := strings.TrimRight(url, "/") + "/" + cache

			if err := updateNixConf(sub, cfg.PublicKey); err != nil {
				return err
			}
			fmt.Printf("added %s to nix.conf\n", sub)

			if !cfg.Public {
				if token == "" {
					fmt.Println("note: private cache — run `xilo login <url> --token <tok>` so a pull token can be written to ~/.netrc")
				} else if err := updateNetrc(hostOf(url), token); err != nil {
					return err
				} else {
					fmt.Println("added pull token to ~/.netrc")
				}
			}
			return nil
		},
	}
	c.Flags().StringVar(&url, "url", "", "server base URL (env XILO_URL / saved login)")
	c.Flags().StringVar(&token, "token", "", "pull token (env XILO_TOKEN / saved login)")
	c.Flags().BoolVar(&remove, "remove", false, "remove the cache from nix.conf instead of adding it")
	return c
}

// removeFromNixConf drops a cache's substituter + its trusted key (key name ==
// cache name) from the managed block.
func removeFromNixConf(sub, cache string) error {
	path := filepath.Join(configDir(), "nix", "nix.conf")
	body, err := os.ReadFile(path)
	if err != nil {
		return nil // nothing to remove
	}
	subs, keys := parseManagedBlock(string(body))
	subs = slices.DeleteFunc(subs, func(s string) bool { return s == sub })
	keys = slices.DeleteFunc(keys, func(k string) bool { return strings.HasPrefix(k, cache+":") })
	return os.WriteFile(path, []byte(replaceManagedBlock(string(body), subs, keys)), 0o644)
}

func fetchCacheConfig(ctx context.Context, url, cache, token string) (*api.ConfigResp, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(url, "/")+"/"+cache+"/api/config", nil)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch cache config: %s", resp.Status)
	}
	var cfg api.ConfigResp
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// updateNixConf maintains a xilo-managed block in ~/.config/nix/nix.conf,
// accumulating extra-substituters + extra-trusted-public-keys across `use` runs.
func updateNixConf(sub, pubkey string) error {
	path := filepath.Join(configDir(), "nix", "nix.conf")
	body, _ := os.ReadFile(path)
	subs, keys := parseManagedBlock(string(body))
	if !slices.Contains(subs, sub) {
		subs = append(subs, sub)
	}
	if pubkey != "" && !slices.Contains(keys, pubkey) {
		keys = append(keys, pubkey)
	}
	out := replaceManagedBlock(string(body), subs, keys)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(out), 0o644)
}

func parseManagedBlock(content string) (subs, keys []string) {
	inBlock := false
	for _, line := range strings.Split(content, "\n") {
		switch {
		case strings.HasPrefix(line, blockStart):
			inBlock = true
		case strings.HasPrefix(line, blockEnd):
			inBlock = false
		case inBlock && strings.HasPrefix(line, "extra-substituters"):
			subs = append(subs, fields(line)...)
		case inBlock && strings.HasPrefix(line, "extra-trusted-public-keys"):
			keys = append(keys, fields(line)...)
		}
	}
	return subs, keys
}

func replaceManagedBlock(content string, subs, keys []string) string {
	block := blockStart + "\n" +
		"extra-substituters = " + strings.Join(subs, " ") + "\n" +
		"extra-trusted-public-keys = " + strings.Join(keys, " ") + "\n" +
		blockEnd

	lines := strings.Split(content, "\n")
	var out []string
	inBlock, replaced := false, false
	for _, line := range lines {
		if strings.HasPrefix(line, blockStart) {
			inBlock = true
			out = append(out, block)
			replaced = true
			continue
		}
		if strings.HasPrefix(line, blockEnd) {
			inBlock = false
			continue
		}
		if !inBlock {
			out = append(out, line)
		}
	}
	result := strings.TrimRight(strings.Join(out, "\n"), "\n")
	if !replaced {
		if result != "" {
			result += "\n"
		}
		result += block
	}
	return result + "\n"
}

// updateNetrc appends a machine entry if the host isn't already present.
func updateNetrc(host, token string) error {
	path := filepath.Join(os.Getenv("HOME"), ".netrc")
	body, _ := os.ReadFile(path)
	if strings.Contains(string(body), "machine "+host+" ") || strings.Contains(string(body), "machine "+host+"\n") {
		return nil
	}
	entry := fmt.Sprintf("machine %s login xilo password %s\n", host, token)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(entry)
	return err
}

// fields returns the space-separated values after "key =".
func fields(line string) []string {
	_, val, ok := strings.Cut(line, "=")
	if !ok {
		return nil
	}
	return strings.Fields(val)
}

func configDir() string {
	if d, err := os.UserConfigDir(); err == nil {
		return d
	}
	return filepath.Join(os.Getenv("HOME"), ".config")
}

// hostOf strips scheme + trailing slash from a base URL, leaving host[:port].
func hostOf(url string) string {
	h := url
	if i := strings.Index(h, "://"); i >= 0 {
		h = h[i+3:]
	}
	return strings.TrimSuffix(h, "/")
}
