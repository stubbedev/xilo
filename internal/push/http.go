package push

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/stubbedev/xilo/internal/api"
	"github.com/stubbedev/xilo/internal/chunk"
)

func (c *Client) url(parts ...string) string {
	var b strings.Builder
	b.WriteString(c.base)
	b.WriteString("/c/")
	b.WriteString(c.cache)
	for _, p := range parts {
		b.WriteByte('/')
		b.WriteString(p)
	}
	return b.String()
}

// retryBackoff is the wait before the first retry, doubled for each one
// after it. A var so tests need not sleep through real backoff.
var retryBackoff = 500 * time.Millisecond

// maxRetries bounds how often one request is replayed after a transient
// failure: about 3.5s of backoff in all, enough to ride out a proxy hiccup
// without stalling a push that is really failing.
const maxRetries = 3

// do sends one push request, replaying it after a transient failure. Every
// request here is safe to repeat: chunk uploads are content-addressed, the
// presence queries read, and put-path registers the same row again. One proxy
// error would otherwise abort a closure of thousands of paths.
func (c *Client) do(req *http.Request) (*http.Response, error) {
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	for attempt := 0; ; attempt++ {
		resp, err := c.http.Do(req)
		if attempt == maxRetries || !transient(resp, err) || (req.Body != nil && req.GetBody == nil) {
			return resp, err
		}
		if resp != nil {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
		}
		select {
		case <-req.Context().Done():
			if resp != nil {
				return nil, fmt.Errorf("%s %s: %s, then %w", req.Method, req.URL.Path, resp.Status, req.Context().Err())
			}
			return nil, err
		case <-time.After(retryBackoff << attempt):
		}
		if req.GetBody != nil {
			body, err := req.GetBody()
			if err != nil {
				return nil, fmt.Errorf("rewind %s %s body for retry: %w", req.Method, req.URL.Path, err)
			}
			req.Body = body
		}
	}
}

// transient reports whether a failed attempt is worth repeating: a gateway
// that could not reach the server (Cloudflare's 520-524 included), or a
// connection that broke. Not a timeout, which already waited the client's
// whole budget, nor a cancelled context or a certificate the client rejects.
func transient(resp *http.Response, err error) bool {
	if err == nil {
		switch resp.StatusCode {
		case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout,
			520, 521, 522, 523, 524:
			return true
		}
		return false
	}
	var ne net.Error
	var cert *tls.CertificateVerificationError
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded),
		errors.As(err, &ne) && ne.Timeout(), errors.As(err, &cert):
		return false
	}
	return true
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
	c.acceptZstd = cfg.AcceptZstd && c.enc != nil
	c.hasFilter = cfg.ChunkFilter
	c.upstreamKeys = cfg.UpstreamKeys
	return chunk.Params{MinSize: cfg.MinSize, AvgSize: cfg.AvgSize, MaxSize: cfg.MaxSize}, nil
}

// missing posts hashes to an api/get-missing-* endpoint and returns the subset
// the server lacks.
func (c *Client) missing(ctx context.Context, endpoint string, hashes []string) ([]string, error) {
	return c.missingReq(ctx, endpoint, api.MissingReq{Hashes: hashes})
}

// missingPaths asks which store paths the cache lacks, sending each path's NAR
// hash so the server can adopt one an identical path elsewhere on its storage
// backend already covers (then it comes back as present and we never dump it).
func (c *Client) missingPaths(ctx context.Context, refs []api.PathRef) ([]string, error) {
	hashes := make([]string, len(refs))
	for i, ref := range refs {
		hashes[i] = ref.Hash
	}
	return c.missingReq(ctx, "get-missing-paths", api.MissingReq{Hashes: hashes, Paths: refs})
}

func (c *Client) missingReq(ctx context.Context, endpoint string, mreq api.MissingReq) ([]string, error) {
	body, _ := json.Marshal(mreq)
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
	// The chunk hash addresses the RAW bytes; the server verifies that after
	// decoding, so compressing the wire is transparent to dedup.
	body := ch.Data
	encoding := ""
	if c.acceptZstd {
		body = chunk.Compress(c.enc, ch.Data)
		encoding = "zstd"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.url("api", "chunk", ch.Hash), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	if encoding != "" {
		req.Header.Set("Content-Encoding", encoding)
	}
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
	// 409 means some chunk we assumed present isn't: a filter false positive, a
	// stale manifest, or a chunk swept mid-push. Reported as its own error type
	// so the caller can redo the path pessimistically instead of failing.
	if resp.StatusCode == http.StatusConflict {
		var out api.MissingResp
		json.NewDecoder(io.LimitReader(resp.Body, maxJSONResp)).Decode(&out)
		return &conflictErr{Missing: out.Missing}
	}
	if resp.StatusCode != http.StatusOK {
		return httpErr("put path", resp)
	}
	return nil
}

// maxJSONResp bounds a JSON response body read into memory.
const maxJSONResp = 1 << 20

// conflictErr is put-path answering "these chunks aren't here".
type conflictErr struct{ Missing []string }

func (e *conflictErr) Error() string {
	return fmt.Sprintf("put path: %d referenced chunks are not on the server", len(e.Missing))
}

// isConflict reports whether err is a put-path 409.
func isConflict(err error) bool {
	var ce *conflictErr
	return errors.As(err, &ce)
}

// loadFilter populates c.filter with the cache's chunk presence filter, so the
// dump loop can decide what to upload without a round trip per window. A fresh
// local copy is used as-is (no request at all); otherwise it revalidates with
// If-None-Match. Any failure leaves c.filter nil and the pusher just asks.
func (c *Client) loadFilter(ctx context.Context) {
	key := filterKey(c.base, c.cache)
	body, etag, age := cachedFilter(key)
	if body != nil && age < filterFresh {
		c.filter = parseFilter(body)
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url("api", "chunk-filter"), nil)
	if err != nil {
		return
	}
	if etag != "" && body != nil {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := c.do(req)
	if err != nil {
		c.filter = parseFilter(body) // offline-ish: a stale filter still helps
		return
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNotModified:
		touchFilter(key)
		c.filter = parseFilter(body)
	case http.StatusOK:
		fresh, err := io.ReadAll(io.LimitReader(resp.Body, filterMaxBytes))
		if err != nil {
			return
		}
		if f := parseFilter(fresh); f != nil {
			saveFilter(key, resp.Header.Get("ETag"), fresh)
			c.filter = f
		}
	default:
		// 204 (cache too big to filter), 404 (old server), or an error: ask.
	}
}

func httpErr(what string, resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return fmt.Errorf("%s: %s: %s", what, resp.Status, bytes.TrimSpace(b))
}
