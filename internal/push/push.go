// Package push is the `xilo push` client: it reads a store-path closure from
// the local Nix daemon, chunks each NAR, uploads only chunks the server lacks
// (in parallel), then registers the paths. Nix can't PUT to an HTTP cache, so
// this replaces `attic push` / `nix copy`.
package push

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/klauspost/compress/zstd"
	"golang.org/x/term"

	"github.com/stubbedev/xilo/internal/api"
	"github.com/stubbedev/xilo/internal/bloom"
	"github.com/stubbedev/xilo/internal/chunk"
	"github.com/stubbedev/xilo/internal/nar"
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

	// enc compresses each chunk on the wire. The client (a fat laptop/CI box)
	// spends the CPU so the server just verifies + stores — far fewer uplink
	// bytes on the slow home connection where pushes actually bottleneck.
	enc *zstd.Encoder

	// populated from the server's /api/config at push time:
	jobs         int
	narThreshold int
	acceptZstd   bool // server decodes Content-Encoding: zstd chunk bodies
	hasFilter    bool // server serves api/chunk-filter
	// externalDump forces `nix-store --dump` instead of serializing the NAR
	// in-process (XILO_EXTERNAL_DUMP=1). An escape hatch for a store holding
	// something our serializer refuses, and what the fake-nix tests use.
	externalDump bool
	upstreamKeys []string
	sem          chan struct{} // ONE shared upload gate, sized to jobs
	win          chan struct{} // bounds in-flight negotiation windows (and so buffered bytes)

	// filter is the server's chunk presence filter, when one was worth
	// fetching. Non-nil turns the negotiation off: a chunk it doesn't hold is
	// uploaded immediately, one it does hold is skipped, and put-path catches
	// the rare false positive (see loadFilter / pushOne).
	filter *bloom.Filter
}

// NewClient builds a push client. jobsOverride of 0 means "auto" — use the
// parallelism the server advertises (its CPU capacity).
func NewClient(base, cache, token string, jobsOverride int) *Client {
	// SpeedDefault (~zstd -3): cheap, and the uplink — not client CPU — is the
	// bottleneck. EncodeAll on a shared encoder is concurrency-safe.
	enc, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	return &Client{
		enc: enc,
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
		externalDump: os.Getenv("XILO_EXTERNAL_DUMP") == "1",
	}
}

func (c *Client) logf(format string, a ...any) {
	if !c.Quiet {
		fmt.Printf(format, a...)
	}
}

var (
	barDone = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "35", Dark: "42"})
	barTodo = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "250", Dark: "238"})
)

// progress redraws an in-place bar on a terminal, or falls back to the plain
// counter (CI logs shouldn't fill with control characters).
func (c *Client) progress(done, total int) {
	if c.Quiet {
		return
	}
	if !term.IsTerminal(int(os.Stdout.Fd())) {
		fmt.Printf("\rpushed %d/%d paths", done, total)
		return
	}
	const width = 28
	filled := done * width / max(total, 1)
	bar := barDone.Render(strings.Repeat("█", filled)) + barTodo.Render(strings.Repeat("░", width-filled))
	fmt.Printf("\r%s %d/%d paths", bar, done, total)
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
	var refs []api.PathRef
	skipped := 0
	for _, in := range infos {
		if c.signedByUpstream(in) {
			skipped++
			continue
		}
		h := narinfo.StoreHash(in.Path)
		byHash[h] = in
		refs = append(refs, api.PathRef{Hash: h, NarHash: in.NarHash})
	}
	missing, err := c.missingPaths(ctx, refs)
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

	var todo uint64
	for _, h := range missing {
		todo += byHash[h].NarSize
	}
	if c.DryRun {
		c.logf("dry-run: would push %d/%d paths (%d uncompressed NAR bytes)\n", len(missing), len(refs), todo)
		return nil
	}

	// Worth a filter? Below filterMinBytes the download costs more than the
	// round trips it saves — but a copy already on disk is free, and loadFilter
	// uses it without a request while it's fresh.
	if c.hasFilter && todo >= filterMinBytes {
		c.loadFilter(ctx)
	}
	// Synchronous: it's one Stat unless a day has passed, and a goroutine here
	// would outlive the push.
	pruneState(params)

	// One shared upload gate across all paths + chunks, so total in-flight
	// uploads never exceed jobs (avoids the jobs² blow-up of a per-path gate).
	c.sem = make(chan struct{}, c.jobs)
	// At least two windows in flight even at jobs=1: one resolving while the
	// dump fills the next is the whole point — otherwise the dump stalls for a
	// full round trip every window, which is what this replaced.
	c.win = make(chan struct{}, max(2, c.jobs))

	var done atomic.Int64
	err = eachParallel(missing, c.jobs, func(h string) error {
		in := byHash[h]
		if err := c.pushOne(ctx, in, params); err != nil {
			return fmt.Errorf("%s: %w", in.Path, err)
		}
		c.progress(int(done.Add(1)), len(missing))
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

const (
	// missingWindow is how many chunks a negotiation window holds before it is
	// handed off to be resolved. Only used when there's no presence filter.
	missingWindow = 32
	// windowBytes caps a window by size too: with a 1 MiB max chunk, counting
	// alone would let one window retain 32 MiB, times jobs in-flight windows.
	windowBytes = 4 << 20
	// filterFresh reuses a downloaded filter without even revalidating it; the
	// server memoizes on the same window, so a request would only ever 304.
	filterFresh = 10 * time.Minute
	// filterMaxBytes bounds the download (the server caps its own filter well
	// under this; the limit is only here so a hostile response can't OOM us).
	filterMaxBytes = 64 << 20
)

// filterMinBytes is the smallest push worth fetching a presence filter for (a
// fresh on-disk copy is used regardless — see loadFilter). A var so tests can
// exercise the filter path without generating 16 MiB of NAR.
var filterMinBytes uint64 = 16 << 20

// dumpOpts turns the optimistic shortcuts off one at a time, each in response to
// a specific failure (see pushOne).
type dumpOpts struct {
	// external serializes the NAR by shelling out to `nix-store --dump` instead
	// of walking the tree in-process.
	external bool
	// pessimistic ignores the chunk presence filter, confirming every chunk
	// with the server before put-path.
	pessimistic bool
}

// pushOne uploads one path. Shortcuts, cheapest first: a cached manifest (no
// dump at all), an in-process NAR dump (no nix-store subprocess), and the
// presence filter (no negotiation round trips). Each is optimistic, and each has
// a check that fails safe:
//
//   - our own serialization is compared against the NAR hash nix recorded for
//     the path, so bytes we produced can never be pushed under a hash they
//     don't have; a mismatch re-dumps with nix itself;
//   - put-path re-stamps and re-checks every referenced chunk, so a path can
//     never register against chunks that aren't there; its 409 redoes the path
//     with the filter off.
//
// At most three attempts, and each transition is driven by a distinct cause.
func (c *Client) pushOne(ctx context.Context, in pathInfo, params chunk.Params) error {
	storeHash := narinfo.StoreHash(in.Path)
	if order := loadManifest(params, storeHash, in.NarHash); order != nil {
		err := c.putPath(ctx, in.pathReq(order))
		if err == nil {
			return nil
		}
		if !isConflict(err) {
			return err // a real failure (auth, server down): don't mask it
		}
	}
	o := dumpOpts{external: c.externalDump}
	for attempt := 0; ; attempt++ {
		err := c.dumpPush(ctx, in, params, o)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, errNarMismatch) && !o.external:
			o.external = true // our archive disagreed with nix; let nix write it
		case isConflict(err) && !o.pessimistic:
			o.pessimistic = true // a chunk we assumed present is gone
		default:
			return err
		}
		if attempt >= 2 {
			return err
		}
	}
}

// dumpPush dumps the NAR once and uploads what the server needs.
func (c *Client) dumpPush(ctx context.Context, in pathInfo, params chunk.Params, o dumpOpts) error {
	filter := c.filter
	if o.pessimistic {
		filter = nil
	}
	// Small NARs are stored as a single chunk — skip CDC overhead entirely.
	if in.NarSize < uint64(c.narThreshold) {
		return c.pushWhole(ctx, in, params, filter, o)
	}

	p := &pathPush{c: c}
	var order []string        // full ordered chunk list for put-path
	var window []chunk.Chunk  // chunks whose fate (missing or not) is unknown
	var winBytes int          // bytes retained in window
	seen := map[string]bool{} // hashes already handled once for this NAR

	// ONE dump: hash every chunk as it streams by and copy bytes only for
	// chunks that may actually need uploading. The whole stream also goes
	// through sha256 so our serialization can be checked against the NAR hash
	// nix recorded for this path (free next to the per-chunk hashing).
	narSum := sha256.New()
	dumpErr := runDump(ctx, in.Path, o.external, func(r io.Reader) error {
		return chunk.SplitRaw(io.TeeReader(r, narSum), params, func(hash string, data []byte) error {
			if p.failed() {
				return errAbort // stop dumping once an upload has failed
			}
			order = append(order, hash)
			if seen[hash] {
				return nil // duplicate within this NAR; first copy decides
			}
			seen[hash] = true
			if filter != nil {
				if filter.Has(hash) {
					return nil // server has it; put-path verifies that claim
				}
				// Definitely absent — upload straight away, no round trip. The
				// shared gate is the dump's backpressure.
				p.upload(ctx, chunk.Chunk{Hash: hash, Data: bytes.Clone(data)})
				return nil
			}
			window = append(window, chunk.Chunk{Hash: hash, Data: bytes.Clone(data)})
			winBytes += len(data)
			if len(window) >= missingWindow || winBytes >= windowBytes {
				p.flush(ctx, window)
				window, winBytes = nil, 0
			}
			return nil
		})
	})
	if dumpErr == nil && len(window) > 0 {
		p.flush(ctx, window) // tail window
	}
	if err := p.wait(); err != nil {
		return err
	}
	if dumpErr != nil && dumpErr != errAbort {
		return dumpErr
	}
	if err := checkNarHash(o, in, narSum.Sum(nil)); err != nil {
		return err
	}

	if err := c.putPath(ctx, in.pathReq(order)); err != nil {
		return err
	}
	saveManifest(params, narinfo.StoreHash(in.Path), in.NarHash, order)
	return nil
}

// pathPush owns the in-flight work for one path. The dump goroutine hands
// chunks and windows off and keeps reading: a negotiation round trip used to
// block the dump for a full RTT every 32 chunks, which on a high-latency link
// was most of the wall clock for a large NAR.
type pathPush struct {
	c   *Client
	wg  sync.WaitGroup
	mu  sync.Mutex
	err error
}

func (p *pathPush) fail(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err == nil {
		p.err = err
	}
}

func (p *pathPush) failed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err != nil
}

// wait blocks until every upload and window this path started has finished.
func (p *pathPush) wait() error {
	p.wg.Wait()
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

// upload sends one chunk through the shared jobs gate. Acquiring the gate
// before spawning is what bounds retained chunk bytes: at most jobs bodies are
// ever in flight, so the dump blocks rather than the heap growing.
func (p *pathPush) upload(ctx context.Context, ch chunk.Chunk) {
	p.c.sem <- struct{}{}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		defer func() { <-p.c.sem }()
		if err := p.c.putChunk(ctx, ch); err != nil {
			p.fail(err)
		}
	}()
}

// flush resolves one window off the dump goroutine: ask which of its chunks the
// server lacks, upload those, drop the rest. c.win bounds how many windows are
// resolving at once, and so how many window bodies stay buffered.
//
// The wg.Add here is always paired with a counter this goroutine already holds,
// so it can never race an in-progress wait().
func (p *pathPush) flush(ctx context.Context, window []chunk.Chunk) {
	p.c.win <- struct{}{}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		defer func() { <-p.c.win }()
		hashes := make([]string, len(window))
		for i, ch := range window {
			hashes[i] = ch.Hash
		}
		need, err := p.c.missing(ctx, "get-missing-chunks", hashes)
		if err != nil {
			p.fail(err)
			return
		}
		needSet := make(map[string]bool, len(need))
		for _, h := range need {
			needSet[h] = true
		}
		for _, ch := range window {
			if needSet[ch.Hash] {
				p.upload(ctx, ch)
			}
		}
	}()
}

// errAbort signals the dump to stop early after an upload failure.
var errAbort = errors.New("aborted")

// pushWhole uploads a small NAR as one chunk.
func (c *Client) pushWhole(ctx context.Context, in pathInfo, params chunk.Params, filter *bloom.Filter, o dumpOpts) error {
	data, err := dumpAll(ctx, in.Path, o.external)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	if err := checkNarHash(o, in, sum[:]); err != nil {
		return err
	}
	ch := chunk.Chunk{Hash: chunk.Hash(data), Data: data}
	have := filter != nil && filter.Has(ch.Hash)
	if filter == nil {
		miss, err := c.missing(ctx, "get-missing-chunks", []string{ch.Hash})
		if err != nil {
			return err
		}
		have = len(miss) == 0
	}
	if !have {
		if err := c.putChunk(ctx, ch); err != nil {
			return err
		}
	}
	if err := c.putPath(ctx, in.pathReq([]string{ch.Hash})); err != nil {
		return err
	}
	saveManifest(params, narinfo.StoreHash(in.Path), in.NarHash, []string{ch.Hash})
	return nil
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

// errNarMismatch means our own NAR serialization did not reproduce the hash nix
// recorded for the path. Never fatal on its own: the caller re-dumps with
// `nix-store --dump`, whose output is authoritative by definition.
var errNarMismatch = errors.New("locally serialized NAR does not match the recorded NarHash")

// checkNarHash compares a dump we produced against nix's recorded NarHash (and
// size). Skipped for external dumps: those ARE nix's output, so a disagreement
// there means the store itself is inconsistent and put-path should say so.
func checkNarHash(o dumpOpts, in pathInfo, sum []byte) error {
	if o.external {
		return nil
	}
	want, err := narinfo.NarHash(in.NarHash)
	if err != nil {
		return nil // unparseable record: let the server be the judge
	}
	if got := "sha256:" + narinfo.Base32Encode(sum); got != want {
		return fmt.Errorf("%w: %s (got %s, nix says %s)", errNarMismatch, in.Path, got, want)
	}
	return nil
}

// runDump streams the path's NAR to consume. In-process by default — a closure
// is thousands of paths, and that many `nix-store --dump` subprocesses is pure
// overhead — falling back to nix's own serializer when asked.
func runDump(ctx context.Context, path string, external bool, consume func(io.Reader) error) error {
	if !external {
		return dumpLocal(ctx, path, consume)
	}
	return dumpExternal(ctx, path, consume)
}

// dumpLocal serializes in-process, streaming through a pipe so the consumer sees
// bytes as they are produced and neither side buffers the archive.
func dumpLocal(ctx context.Context, path string, consume func(io.Reader) error) error {
	pr, pw := io.Pipe()
	go func() {
		err := nar.Dump(pw, path)
		if ctxErr := ctx.Err(); err == nil && ctxErr != nil {
			err = ctxErr
		}
		pw.CloseWithError(err)
	}()
	consumeErr := consume(pr)
	// Unblock the writer if the consumer stopped early (an upload failed), then
	// drain so its goroutine always finishes.
	pr.CloseWithError(consumeErr)
	if consumeErr != nil {
		return consumeErr
	}
	return nil
}

func dumpExternal(ctx context.Context, path string, consume func(io.Reader) error) error {
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
func dumpAll(ctx context.Context, path string, external bool) ([]byte, error) {
	if !external {
		var buf bytes.Buffer
		if err := nar.Dump(&buf, path); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	}
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
