package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/stubbedev/xilo/internal/api"
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
//   - otherwise → local DB, created fresh, but only on a box that has a config
//     file (bootstrap on a new server); elsewhere openDB refuses rather than
//     littering the working directory with a data dir
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
		// A configured Postgres URL or an existing SQLite file means we're on
		// the server — operate on the DB directly.
		if cfg.Database.URL != "" {
			db, err = openStore(cfg)
			announceTarget("database " + redactDSN(cfg.Database.URL))
			return nil, cfg, db, err
		}
		if _, statErr := os.Stat(cfg.DBPath()); statErr == nil {
			db, err = openStore(cfg)
			announceTarget("local database " + cfg.DBPath())
			return nil, cfg, db, err
		}
	}
	if remoteURL == "" {
		// Fresh box, no server known: bootstrap a local DB.
		_, db, err = openDB()
		if err == nil {
			announceTarget("local database " + cfg.DBPath())
		}
		return nil, cfg, db, err
	}
	_, token := resolveServer(serverFlag, tokenFlag)
	if token == "" {
		return nil, nil, nil, fmt.Errorf("admin token required for %s — pass --token, set XILO_TOKEN, or `xilo login`", remoteURL)
	}
	announceTarget(remoteURL)
	return newAPIClient(remoteURL, token), cfg, nil, nil
}

// announceTarget names where an admin command is about to act. Which of the
// two it picks depends on whether a config file or database happens to exist
// on this box, and writing straight to the database of a *running* server
// bypasses its API and its activity log — so the choice is worth one line of
// stderr rather than being silent.
func announceTarget(what string) {
	fmt.Fprintln(os.Stderr, styleDim("xilo: acting on "+what))
}

// redactDSN strips credentials from a database URL before printing it.
func redactDSN(dsn string) string {
	scheme, rest, ok := strings.Cut(dsn, "://")
	if !ok {
		return dsn
	}
	if _, host, hasCreds := strings.Cut(rest, "@"); hasCreds {
		return scheme + "://***@" + host
	}
	return dsn
}

// whoami asks the server to describe the token this client is using. It is the
// one management call that needs no admin perm — a credential that cannot say
// what it is turns every 401 into guesswork.
func (c *apiClient) whoami() (*api.WhoamiResp, error) {
	var w api.WhoamiResp
	if err := c.do(http.MethodGet, "/api/v1/whoami", nil, &w); err != nil {
		return nil, err
	}
	return &w, nil
}

// resolveRef qualifies a cache reference for the client commands. An explicit
// "account/cache" passes through; a bare name is resolved against the token's
// own account, which only the server knows. Guessing it client-side is what
// produced the phantom "default" account.
func resolveRef(url, token, ref string) (string, error) {
	account, name := splitRef(ref)
	if name == "" {
		return "", errors.New("no cache given")
	}
	if account != "" {
		return account + "/" + name, nil
	}
	if token == "" {
		return "", fmt.Errorf("no token, so %q cannot be resolved to an account — write it as <account>/%s", ref, ref)
	}
	who, err := newAPIClient(url, token).whoami()
	if err != nil {
		return "", fmt.Errorf("resolving cache %q: %w — or write it as <account>/%s", ref, err, ref)
	}
	if who.Account == "" {
		return "", fmt.Errorf("this token is instance-wide, so it names no account — write the cache as <account>/%s", ref)
	}
	return who.Account + "/" + name, nil
}
