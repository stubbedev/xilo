// Package push is the `xilo push` client: it reads a store-path closure from
// the local Nix daemon, chunks each NAR, uploads only chunks the server lacks
// (in parallel), then registers the paths. Nix can't PUT to an HTTP cache, so
// this replaces `attic push` / `nix copy`.
package push

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/stubbedev/xilo/internal/api"
	"github.com/stubbedev/xilo/internal/chunk"
	"github.com/stubbedev/xilo/internal/narinfo"
)

type Client struct {
	http         *http.Client
	base         string // server base URL, no trailing slash
	cache        string
	token        string
	jobsOverride int  // 0 = use server-advertised parallelism
	DryRun       bool // plan only, upload nothing
	Quiet        bool // suppress progress output

	// populated from the server's /api/config at push time:
	jobs         int
	narThreshold int
	upstreamKeys []string
	sem          chan struct{} // ONE shared upload gate, sized to jobs
}

// NewClient builds a push client. jobsOverride of 0 means "auto" — use the
// parallelism the server advertises (its CPU capacity).
func NewClient(base, cache, token string, jobsOverride int) *Client {
	return &Client{
		// Default transport keeps only 2 idle conns per host — at jobs=NumCPU
		// that means re-dialing (and re-TLS-handshaking) on nearly every chunk.
		// The overall timeout bounds every request: the largest body is one
		// chunk (~2MiB), so 5m is generous even on very slow links — without
		// it, a server dying mid-upload leaves the client stuck in TCP
		// retransmission for 15-30 minutes (hangs CI jobs).
		http: &http.Client{
			Timeout: 5 * time.Minute,
			Transport: &http.Transport{
				MaxIdleConns:        128,
				MaxIdleConnsPerHost: 64,
			},
		},
		base:         strings.TrimRight(base, "/"),
		cache:        cache,
		token:        token,
		jobsOverride: jobsOverride,
	}
}

func (c *Client) logf(format string, a ...any) {
	if !c.Quiet {
		fmt.Printf(format, a...)
	}
}

type pathInfo struct {
	Path       string   `json:"path"`
	NarHash    string   `json:"narHash"`
	NarSize    uint64   `json:"narSize"`
	References []string `json:"references"`
	Deriver    string   `json:"deriver"`
	Signatures []string `json:"signatures"`
}

// Push uploads the full closure of the given store paths to the cache.
func (c *Client) Push(ctx context.Context, paths []string) error {
	params, err := c.loadConfig(ctx)
	if err != nil {
		return fmt.Errorf("fetch server config: %w", err)
	}
	infos, err := queryClosure(ctx, paths)
	if err != nil {
		return err
	}

	byHash := map[string]pathInfo{}
	var hashes []string
	skipped := 0
	for _, in := range infos {
		if c.signedByUpstream(in) {
			skipped++
			continue
		}
		h := narinfo.StoreHash(in.Path)
		byHash[h] = in
		hashes = append(hashes, h)
	}
	missing, err := c.missing(ctx, "get-missing-paths", hashes)
	if err != nil {
		return err
	}
	if skipped > 0 {
		c.logf("skipped %d upstream-signed paths\n", skipped)
	}
	if len(missing) == 0 {
		c.logf("everything already cached\n")
		return nil
	}

	if c.DryRun {
		var bytes uint64
		for _, h := range missing {
			bytes += byHash[h].NarSize
		}
		c.logf("dry-run: would push %d/%d paths (%d uncompressed NAR bytes)\n", len(missing), len(hashes), bytes)
		return nil
	}

	// One shared upload gate across all paths + chunks, so total in-flight
	// uploads never exceed jobs (avoids the jobs² blow-up of a per-path gate).
	c.sem = make(chan struct{}, c.jobs)

	var done atomic.Int64
	err = eachParallel(missing, c.jobs, func(h string) error {
		in := byHash[h]
		if err := c.pushOne(ctx, in, params); err != nil {
			return fmt.Errorf("%s: %w", in.Path, err)
		}
		c.logf("\rpushed %d/%d paths", done.Add(1), len(missing))
		return nil
	})
	c.logf("\n")
	if err != nil {
		return err
	}
	c.logf("pushed %d paths to %s\n", len(missing), c.cache)
	return nil
}

func (c *Client) signedByUpstream(in pathInfo) bool {
	if len(c.upstreamKeys) == 0 {
		return false
	}
	for _, sig := range in.Signatures {
		name, _, _ := strings.Cut(sig, ":")
		if slices.Contains(c.upstreamKeys, name) {
			return true
		}
	}
	return false
}

func (c *Client) pushOne(ctx context.Context, in pathInfo, params chunk.Params) error {
	// Small NARs are stored as a single chunk — skip CDC overhead entirely.
	if in.NarSize < uint64(c.narThreshold) {
		return c.pushWhole(ctx, in)
	}

	// Pass 1: chunk the NAR to get the ordered hash list. Hash-only (no data
	// copy) — cheap.
	var order []string
	if err := dumpHashes(ctx, in.Path, params, func(h string) error {
		order = append(order, h)
		return nil
	}); err != nil {
		return err
	}

	need, err := c.missing(ctx, "get-missing-chunks", order)
	if err != nil {
		return err
	}
	if len(need) == 0 {
		return c.putPath(ctx, in.pathReq(order))
	}
	needSet := map[string]bool{}
	for _, h := range need {
		needSet[h] = true
	}

	// Pass 2: re-dump, upload missing chunks in parallel via the SHARED gate.
	var mu sync.Mutex
	uploaded := map[string]bool{}
	var wg sync.WaitGroup
	var firstErr error
	failed := func() bool { mu.Lock(); defer mu.Unlock(); return firstErr != nil }
	setErr := func(e error) {
		mu.Lock()
		defer mu.Unlock()
		if firstErr == nil {
			firstErr = e
		}
	}

	dumpErr := dumpChunks(ctx, in.Path, params, func(ch chunk.Chunk) error {
		if failed() {
			return errAbort // stop dumping once an upload has failed
		}
		mu.Lock()
		if !needSet[ch.Hash] || uploaded[ch.Hash] {
			mu.Unlock()
			return nil
		}
		uploaded[ch.Hash] = true
		mu.Unlock()

		c.sem <- struct{}{}
		wg.Add(1)
		go func(ch chunk.Chunk) {
			defer wg.Done()
			defer func() { <-c.sem }()
			if err := c.putChunk(ctx, ch); err != nil {
				setErr(err)
			}
		}(ch)
		return nil
	})
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	if dumpErr != nil && dumpErr != errAbort {
		return dumpErr
	}

	return c.putPath(ctx, in.pathReq(order))
}

// errAbort signals the pass-2 dump to stop early after an upload failure.
var errAbort = errors.New("aborted")

// pushWhole uploads a small NAR as one chunk.
func (c *Client) pushWhole(ctx context.Context, in pathInfo) error {
	data, err := dumpAll(ctx, in.Path)
	if err != nil {
		return err
	}
	ch := chunk.Chunk{Hash: chunk.Hash(data), Data: data}
	miss, err := c.missing(ctx, "get-missing-chunks", []string{ch.Hash})
	if err != nil {
		return err
	}
	if len(miss) > 0 {
		if err := c.putChunk(ctx, ch); err != nil {
			return err
		}
	}
	return c.putPath(ctx, in.pathReq([]string{ch.Hash}))
}

func (in pathInfo) pathReq(chunks []string) api.PathReq {
	return api.PathReq{
		StorePath:  in.Path,
		NarHash:    in.NarHash,
		NarSize:    in.NarSize,
		Deriver:    in.Deriver,
		References: in.References,
		Chunks:     chunks,
	}
}

// eachParallel runs fn over items with at most `jobs` concurrent, returning the
// first error. No external deps (errgroup) — a small bounded worker loop.
func eachParallel[T any](items []T, jobs int, fn func(T) error) error {
	if jobs < 1 {
		jobs = 1 // an unbuffered gate would deadlock
	}
	sem := make(chan struct{}, jobs)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	for _, it := range items {
		mu.Lock()
		stop := firstErr != nil
		mu.Unlock()
		if stop {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(it T) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := fn(it); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
			}
		}(it)
	}
	wg.Wait()
	return firstErr
}

// ---- nix invocations ----

func queryClosure(ctx context.Context, paths []string) ([]pathInfo, error) {
	args := append([]string{"path-info", "--recursive", "--json"}, paths...)
	out, err := exec.CommandContext(ctx, "nix", args...).Output()
	if err != nil {
		return nil, fmt.Errorf("nix path-info: %w", cmdErr(err))
	}
	return parsePathInfo(out)
}

func parsePathInfo(out []byte) ([]pathInfo, error) {
	trimmed := bytes.TrimSpace(out)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		var arr []pathInfo
		if err := json.Unmarshal(trimmed, &arr); err != nil {
			return nil, err
		}
		return arr, nil
	}
	var m map[string]pathInfo
	if err := json.Unmarshal(trimmed, &m); err != nil {
		return nil, err
	}
	var arr []pathInfo
	for path, in := range m {
		if in.Path == "" {
			in.Path = path
		}
		arr = append(arr, in)
	}
	return arr, nil
}

// dumpHashes streams `nix-store --dump path` through the chunker, reporting only
// the ordered chunk hashes (no data copy).
func dumpHashes(ctx context.Context, path string, params chunk.Params, fn func(hash string) error) error {
	return runDump(ctx, path, func(r io.Reader) error {
		return chunk.SplitHashes(r, params, fn)
	})
}

// dumpChunks streams `nix-store --dump path` through the chunker.
func dumpChunks(ctx context.Context, path string, params chunk.Params, fn func(chunk.Chunk) error) error {
	return runDump(ctx, path, func(r io.Reader) error {
		return chunk.Split(r, params, fn)
	})
}

func runDump(ctx context.Context, path string, consume func(io.Reader) error) error {
	cmd := exec.CommandContext(ctx, "nix-store", "--dump", path)
	// If the process (or a child holding the pipe) lingers after Kill/ctx
	// cancel, give up on its I/O rather than blocking Wait forever.
	cmd.WaitDelay = 5 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	consumeErr := consume(stdout)
	if consumeErr != nil {
		// The consumer stopped mid-stream (e.g. an upload failed). nix-store
		// still has NAR bytes to write; with no reader the pipe fills and
		// Wait() would block forever — kill the writer first.
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	if consumeErr != nil {
		return consumeErr
	}
	if waitErr != nil {
		return fmt.Errorf("nix-store --dump: %v: %s", waitErr, stderr.String())
	}
	return nil
}

// dumpAll returns the whole NAR (for small paths below the chunk threshold).
func dumpAll(ctx context.Context, path string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, "nix-store", "--dump", path).Output()
	if err != nil {
		return nil, fmt.Errorf("nix-store --dump: %w", cmdErr(err))
	}
	return out, nil
}

func cmdErr(err error) error {
	if ee, ok := err.(*exec.ExitError); ok {
		return fmt.Errorf("%v: %s", ee, strings.TrimSpace(string(ee.Stderr)))
	}
	return err
}
