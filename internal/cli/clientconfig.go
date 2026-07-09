package cli

import (
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// clientConfig is the push/use client's saved server URL + token
// (~/.config/xilo/config.yaml), written by `xilo login`.
type clientConfig struct {
	URL   string `yaml:"url"`
	Token string `yaml:"token"`
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
	return c
}

func saveClientConfig(c clientConfig) error {
	p := clientConfigPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o600) // contains a token
}

// resolveServer picks URL + token: flag > env > saved config > default URL.
func resolveServer(urlFlag, tokenFlag string) (url, token string) {
	cc := loadClientConfig()
	url = urlFlag
	if url == "" {
		url = os.Getenv("XILO_URL")
	}
	if url == "" {
		url = cc.URL
	}
	if url == "" {
		url = "http://localhost:8080"
	}
	token = tokenFlag
	if token == "" {
		token = os.Getenv("XILO_TOKEN")
	}
	if token == "" {
		token = cc.Token
	}
	return url, token
}
