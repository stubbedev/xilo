package cli

import (
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"

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
	if err := os.WriteFile(p, data, 0o600); err != nil { // contains tokens
		return err
	}
	// WriteFile only applies the mode when creating the file; tighten an
	// already-existing file that may have looser perms from an older version.
	return os.Chmod(p, 0o600)
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

// errNoServer is what client commands report when no server was configured.
// Defaulting to localhost:8080 instead only turned "you never told me where the
// server is" into a connection-refused further down.
var errNoServer = errors.New("no server configured. Pass --url <url>, set XILO_URL, or run `xilo login <url>`")

// resolveServer picks URL + token: flag > env > saved profile. The URL comes
// back empty when none of those is set; callers report errNoServer. The token
// follows resolveToken: a saved one only travels to the server it was saved for.
func resolveServer(urlFlag, tokenFlag string) (url, token string) {
	p := activeProfile()
	url = urlFlag
	if url == "" {
		url = os.Getenv("XILO_URL")
	}
	if url == "" {
		url = p.URL
	}
	return url, resolveToken(p, url, tokenFlag)
}

// resolveToken picks the token to send to server: flag > env > the profile's
// saved token, the last only when server is the profile's own server. A token
// is a credential for one server, so pointing a command elsewhere with --url,
// --server or XILO_URL must not hand that host the token saved for another;
// it gets none unless one is passed explicitly.
func resolveToken(p serverProfile, server, tokenFlag string) string {
	if tokenFlag != "" {
		return tokenFlag
	}
	if t := os.Getenv("XILO_TOKEN"); t != "" {
		return t
	}
	if sameServer(server, p.URL) {
		return p.Token
	}
	return ""
}

// serverID is a base URL reduced to what identifies the server behind it.
type serverID struct{ scheme, host, port, path string }

// parseServer normalizes a base URL: scheme and host lowercased, the scheme's
// default port dropped, trailing slashes trimmed off the path. Anything that
// does not parse to a scheme and a host is not a server.
func parseServer(raw string) (serverID, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return serverID{}, false
	}
	id := serverID{
		scheme: strings.ToLower(u.Scheme),
		host:   strings.ToLower(u.Hostname()),
		port:   u.Port(),
		path:   strings.TrimRight(u.EscapedPath(), "/"),
	}
	if (id.scheme == "https" && id.port == "443") || (id.scheme == "http" && id.port == "80") {
		id.port = ""
	}
	return id, true
}

// sameServer reports whether two base URLs name the same server. Spelling
// differences (host case, a default port, a trailing slash) do not matter;
// a different scheme, host, port or path does, and so does a URL that fails
// to parse.
func sameServer(a, b string) bool {
	x, ok := parseServer(a)
	if !ok {
		return false
	}
	y, ok := parseServer(b)
	return ok && x == y
}

// defaultCache returns the saved default push/use target ("ns/cache"), if any.
func defaultCache() string {
	if v := os.Getenv("XILO_CACHE"); v != "" {
		return v
	}
	return activeProfile().Cache
}
