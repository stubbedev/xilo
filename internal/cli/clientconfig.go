package cli

import (
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// serverProfile is one saved server: URL, token, and optionally the default
// push/use target ("ns/cache") on that server.
type serverProfile struct {
	URL   string `yaml:"url"`
	Token string `yaml:"token,omitempty"`
	Cache string `yaml:"cache,omitempty"`
}

// clientConfig is the push/use client's saved state
// (~/.config/xilo/config.yaml), written by `xilo login` and `xilo use`.
// Multiple servers are kept as named profiles; Default names the one used
// when no --profile is given.
type clientConfig struct {
	Default string                   `yaml:"default,omitempty"`
	Servers map[string]serverProfile `yaml:"servers,omitempty"`

	// Legacy single-server fields (pre-profiles); folded into Servers on load.
	URL   string `yaml:"url,omitempty"`
	Token string `yaml:"token,omitempty"`
}

func clientConfigPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(dir, "xilo", "config.yaml")
}

func loadClientConfig() clientConfig {
	var c clientConfig
	data, err := os.ReadFile(clientConfigPath())
	if err == nil {
		yaml.Unmarshal(data, &c)
	}
	// Fold a legacy flat config into a profile named "default".
	if c.URL != "" && len(c.Servers) == 0 {
		c.Servers = map[string]serverProfile{"default": {URL: c.URL, Token: c.Token}}
		c.Default = "default"
	}
	c.URL, c.Token = "", ""
	if c.Default == "" {
		for name := range c.Servers {
			c.Default = name // any profile beats none; single-profile is the common case
			break
		}
	}
	return c
}

func saveClientConfig(c clientConfig) error {
	p := clientConfigPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	c.URL, c.Token = "", "" // never write the legacy shape back
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o600) // contains tokens
}

// profileFlag is the shared --profile selector for client commands.
var profileFlag string

// activeProfile returns the selected (or default) saved profile.
func activeProfile() serverProfile {
	cc := loadClientConfig()
	name := profileFlag
	if name == "" {
		name = cc.Default
	}
	return cc.Servers[name]
}

// resolveServer picks URL + token: flag > env > saved profile > default URL.
func resolveServer(urlFlag, tokenFlag string) (url, token string) {
	p := activeProfile()
	url = urlFlag
	if url == "" {
		url = os.Getenv("XILO_URL")
	}
	if url == "" {
		url = p.URL
	}
	if url == "" {
		url = "http://localhost:8080"
	}
	token = tokenFlag
	if token == "" {
		token = os.Getenv("XILO_TOKEN")
	}
	if token == "" {
		token = p.Token
	}
	return url, token
}

// defaultCache returns the saved default push/use target ("ns/cache"), if any.
func defaultCache() string {
	if v := os.Getenv("XILO_CACHE"); v != "" {
		return v
	}
	return activeProfile().Cache
}
