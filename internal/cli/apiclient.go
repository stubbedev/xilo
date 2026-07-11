package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/stubbedev/xilo/internal/config"
	"github.com/stubbedev/xilo/internal/store"
)

// apiClient talks to a running xilo server's /api/v1 management API with an
// admin-perm token.
type apiClient struct {
	base  string
	token string
	hc    *http.Client
}

func newAPIClient(url, token string) *apiClient {
	return &apiClient{base: url, token: token, hc: &http.Client{Timeout: 5 * time.Minute}}
}

// do sends one JSON request. in may be nil (no body); out may be nil (ignore
// body). Non-2xx responses become errors carrying the server's message.
func (c *apiClient) do(method, path string, in, out any) error {
	var body *bytes.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	} else {
		body = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, c.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		var e struct {
			Error string `json:"error"`
		}
		json.Unmarshal(raw, &e)
		if e.Error == "" {
			e.Error = strings.TrimSpace(string(raw))
		}
		if e.Error == "" {
			e.Error = resp.Status
		}
		return fmt.Errorf("%s %s: %s", method, path, e.Error)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// adminTarget picks where an admin command (cache/token/gc) operates:
//   - --server flag set → remote API
//   - the local metadata DB exists → open it directly (operator on the box)
//   - a server URL is known (XILO_URL or `xilo login`) → remote API
//   - otherwise → local DB, created fresh (bootstrap on a new box)
//
// Exactly one of (apic) or (db) is returned non-nil.
func adminTarget(serverFlag, tokenFlag string) (apic *apiClient, cfg *config.Config, db *store.DB, err error) {
	cfg, err = config.Load(configPath)
	if err != nil {
		return nil, nil, nil, err
	}
	remoteURL := serverFlag
	if remoteURL == "" {
		remoteURL = os.Getenv("XILO_URL")
	}
	if remoteURL == "" {
		remoteURL = loadClientConfig().URL
	}
	if serverFlag == "" {
		if _, statErr := os.Stat(cfg.DBPath()); statErr == nil {
			db, err = openStore(cfg)
			return nil, cfg, db, err
		}
	}
	if remoteURL == "" {
		// Fresh box, no server known: bootstrap a local DB.
		_, db, err = openDB()
		return nil, cfg, db, err
	}
	_, token := resolveServer(serverFlag, tokenFlag)
	if token == "" {
		return nil, nil, nil, fmt.Errorf("admin token required for %s — pass --token, set XILO_TOKEN, or `xilo login`", remoteURL)
	}
	return newAPIClient(remoteURL, token), cfg, nil, nil
}
