package push

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/stubbedev/xilo/internal/api"
	"github.com/stubbedev/xilo/internal/chunk"
)

func (c *Client) url(parts ...string) string {
	u := c.base + "/" + c.cache
	for _, p := range parts {
		u += "/" + p
	}
	return u
}

func (c *Client) do(req *http.Request) (*http.Response, error) {
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	return c.http.Do(req)
}

// loadConfig fetches the server's push config and populates the auto-derived
// client fields (parallelism, NAR threshold, upstream keys). Returns the
// chunking params to use.
func (c *Client) loadConfig(ctx context.Context) (chunk.Params, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url("api", "config"), nil)
	if err != nil {
		return chunk.Params{}, err
	}
	resp, err := c.do(req)
	if err != nil {
		return chunk.Params{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return chunk.Params{}, httpErr("get config", resp)
	}
	var cfg api.ConfigResp
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
		return chunk.Params{}, err
	}
	c.jobs = cfg.Parallelism
	if c.jobsOverride > 0 {
		c.jobs = c.jobsOverride
	}
	if c.jobs < 1 {
		c.jobs = 1
	}
	c.narThreshold = cfg.NarThreshold
	c.upstreamKeys = cfg.UpstreamKeys
	return chunk.Params{MinSize: cfg.MinSize, AvgSize: cfg.AvgSize, MaxSize: cfg.MaxSize}, nil
}

// missing posts hashes to an api/get-missing-* endpoint and returns the subset
// the server lacks.
func (c *Client) missing(ctx context.Context, endpoint string, hashes []string) ([]string, error) {
	body, _ := json.Marshal(api.MissingReq{Hashes: hashes})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url("api", endpoint), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, httpErr(endpoint, resp)
	}
	var out api.MissingResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Missing, nil
}

func (c *Client) putChunk(ctx context.Context, ch chunk.Chunk) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.url("api", "chunk", ch.Hash), bytes.NewReader(ch.Data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return httpErr("put chunk", resp)
	}
	return nil
}

func (c *Client) putPath(ctx context.Context, p api.PathReq) error {
	body, _ := json.Marshal(p)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.url("api", "path"), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return httpErr("put path", resp)
	}
	return nil
}

func httpErr(what string, resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return fmt.Errorf("%s: %s: %s", what, resp.Status, bytes.TrimSpace(b))
}
