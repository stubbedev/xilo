# How it works

## Request path

`cmd/xilo` dispatches to `internal/server` (one `http.ServeMux`), which reads
metadata through `internal/store` and chunk bytes through `internal/storage`.
The server exposes three surfaces:

- The Nix binary-cache protocol at `/c/{account}/{cache}/...`:
  `nix-cache-info`, `{hash}.narinfo` (signed on the fly with the cache's
  ed25519 key) and `nar/{hash}.nar` (reassembled from chunks).
- The push protocol beside it: `api/config`, `api/get-missing-paths`,
  `api/get-missing-chunks`, `api/chunk-filter`, `api/chunk/{hash}`,
  `api/path`.
- The token-authed JSON API at `/api/v1/...` and the session-authed dashboard
  at `/admin/...`.

A push asks `nix path-info` for the closure, chunks each NAR client-side with
FastCDC, uploads only the chunks the server lacks, then registers the path
metadata.

## What a push avoids doing

Determining what the server already has is most of a push, so four mechanisms
cut that down, cheapest first:

- **Path adoption.** The client sends each path's NAR hash with the "what is
  missing" question. If an identical path already exists in another cache on
  the same storage backend, the server copies the chunk list over and reports
  the path present: no dump, no chunking, no upload. Only caches the caller may
  already read are eligible (public, pullable by the token, or an admin token),
  so adoption grants no access the caller did not have.
- **Cached chunk manifests.** `~/.cache/xilo/manifests` remembers each pushed
  path's chunk list, keyed by store hash, NAR hash and the server's chunking
  parameters. Pushing the same path again (to a second cache, after a failed
  run, after a server GC) registers it without reading the NAR.
- **The chunk presence filter.** `GET /c/{account}/{cache}/api/chunk-filter`
  serves a bloom filter of the backend's chunk hashes (memoized for about ten
  minutes, `ETag`-revalidated, cached on disk). With it the client uploads new
  chunks immediately and skips known ones without a negotiation round trip. It
  is fetched only when the push is large enough to be worth the download.
- **Pipelined negotiation.** Without a filter, chunk windows are resolved
  concurrently with the dump instead of blocking it, so a round trip no longer
  stalls the stream every 32 chunks.

Server-side, `put-path`'s reassembly check, which streams every referenced
chunk back out of the blob store to confirm the claimed NarHash, is remembered
per chunk list, so a re-push, a push to a second cache, or a retry only
confirms that the blobs still exist.

Every one of those shortcuts is optimistic, and one check makes that safe:
`put-path` re-stamps and re-checks every referenced chunk, answering `409` with
the hashes that are absent. On a `409` the client redoes that path with all
shortcuts disabled, so a stale manifest, a filter false positive or a chunk
swept mid-push costs one extra dump and never a corrupt or dangling path.
