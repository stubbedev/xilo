package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stubbedev/xilo/internal/api"
)

// apiReq sends a JSON request to the admin API and decodes the response.
func apiReq(t *testing.T, ts *httptest.Server, method, path, token string, in any) (*http.Response, []byte) {
	t.Helper()
	var body bytes.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			t.Fatal(err)
		}
		body = *bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, ts.URL+path, &body)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	buf.ReadFrom(resp.Body)
	return resp, buf.Bytes()
}

func TestAdminAPI(t *testing.T) {
	_, db, ts := newTestServer(t, false)

	adminSecret, _, err := db.CreateToken(0, "boss", nil, []string{"admin"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	pushSecret, _, err := db.CreateToken(0, "pleb", nil, []string{"push", "pull"}, 0)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("auth", func(t *testing.T) {
		for _, tok := range []string{"", "garbage", pushSecret} {
			resp, _ := apiReq(t, ts, http.MethodGet, "/api/v1/caches", tok, nil)
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("token %q: status = %d, want 401", tok, resp.StatusCode)
			}
		}
	})

	t.Run("cache lifecycle", func(t *testing.T) {
		resp, body := apiReq(t, ts, http.MethodPost, "/api/v1/caches", adminSecret,
			api.CreateCacheReq{Name: "apicache", Public: false})
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("create: %d %s", resp.StatusCode, body)
		}
		var c api.Cache
		json.Unmarshal(body, &c)
		if c.Name != "apicache" || c.Public || c.Priority != 40 || c.PubKey == "" {
			t.Fatalf("created cache = %+v", c)
		}

		resp, body = apiReq(t, ts, http.MethodGet, "/api/v1/caches", adminSecret, nil)
		var list []api.Cache
		json.Unmarshal(body, &list)
		if resp.StatusCode != 200 || len(list) != 1 {
			t.Fatalf("list: %d %s", resp.StatusCode, body)
		}

		pub := true
		prio := 30
		var ret int64 = 3600
		resp, body = apiReq(t, ts, http.MethodPatch, "/api/v1/caches/default/apicache", adminSecret,
			api.ConfigureCacheReq{Public: &pub, Priority: &prio, Retention: &ret})
		json.Unmarshal(body, &c)
		if resp.StatusCode != 200 || !c.Public || c.Priority != 30 || c.Retention != 3600 {
			t.Fatalf("configure: %d %+v", resp.StatusCode, c)
		}

		old := c.PubKey
		resp, body = apiReq(t, ts, http.MethodPost, "/api/v1/caches/default/apicache/rotate", adminSecret, nil)
		json.Unmarshal(body, &c)
		if resp.StatusCode != 200 || c.PubKey == old || c.PubKey == "" {
			t.Fatalf("rotate: %d %+v", resp.StatusCode, c)
		}

		resp, body = apiReq(t, ts, http.MethodGet, "/api/v1/caches/default/apicache", adminSecret, nil)
		var d api.CacheDetail
		json.Unmarshal(body, &d)
		if resp.StatusCode != 200 || d.Name != "apicache" || d.Paths != 0 {
			t.Fatalf("get: %d %+v", resp.StatusCode, d)
		}

		resp, _ = apiReq(t, ts, http.MethodDelete, "/api/v1/caches/default/apicache", adminSecret, nil)
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("delete: %d", resp.StatusCode)
		}
		resp, _ = apiReq(t, ts, http.MethodGet, "/api/v1/caches/default/apicache", adminSecret, nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("get deleted: %d, want 404", resp.StatusCode)
		}
	})

	t.Run("token lifecycle", func(t *testing.T) {
		resp, body := apiReq(t, ts, http.MethodPost, "/api/v1/tokens", adminSecret,
			api.CreateTokenReq{Name: "ci", Perms: []string{"push"}})
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("create token: %d %s", resp.StatusCode, body)
		}
		var ct api.CreateTokenResp
		json.Unmarshal(body, &ct)
		if ct.Secret == "" || ct.Token.Name != "ci" {
			t.Fatalf("create token resp = %+v", ct)
		}

		resp, _ = apiReq(t, ts, http.MethodPost, "/api/v1/tokens", adminSecret,
			api.CreateTokenReq{Name: "noperm"})
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("create token without perms: %d, want 400", resp.StatusCode)
		}

		resp, body = apiReq(t, ts, http.MethodGet, "/api/v1/tokens", adminSecret, nil)
		var toks []api.Token
		json.Unmarshal(body, &toks)
		if len(toks) != 3 { // boss, pleb, ci
			t.Fatalf("list tokens: %d %s", resp.StatusCode, body)
		}

		resp, _ = apiReq(t, ts, http.MethodPost, "/api/v1/tokens/3/revoke", adminSecret, nil)
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("revoke: %d", resp.StatusCode)
		}
		if db.Authorize(ct.Secret, "default", "any", "push", 0) {
			t.Fatal("revoked token still authorizes")
		}
	})

	t.Run("gc", func(t *testing.T) {
		resp, body := apiReq(t, ts, http.MethodPost, "/api/v1/gc", adminSecret,
			api.GCReq{EvictOlderThan: 3600})
		var g api.GCResp
		json.Unmarshal(body, &g)
		if resp.StatusCode != 200 {
			t.Fatalf("gc: %d %s", resp.StatusCode, body)
		}
	})

	t.Run("revoked admin token loses access", func(t *testing.T) {
		sec, tok, err := db.CreateToken(0, "shortlived", nil, []string{"admin"}, 0)
		if err != nil {
			t.Fatal(err)
		}
		resp, _ := apiReq(t, ts, http.MethodGet, "/api/v1/caches", sec, nil)
		if resp.StatusCode != 200 {
			t.Fatalf("live admin token: %d", resp.StatusCode)
		}
		if err := db.RevokeToken(tok.ID); err != nil {
			t.Fatal(err)
		}
		resp, _ = apiReq(t, ts, http.MethodGet, "/api/v1/caches", sec, nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("revoked admin token: %d, want 401", resp.StatusCode)
		}
	})
}
