# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

xilo — self-hosted Nix binary cache in a single Go binary (server + client CLI in one, `cmd/xilo`). SQLite (or PostgreSQL) for metadata, local-disk or S3 for chunk bytes, FastCDC content-addressed dedup, multi-tenant admin dashboard.

## Commands

Everything goes through `just` (run `just` to list recipes):

- `just build` — css + templ generate + `go build` into `./bin/xilo`
- `just test` — css + generate + `go test ./...`
- `just lint` — css + generate + `gofmt -l .` + `go vet ./...` + a `-tags noserver` build
- `just check` — everything CI runs (lint, test, schema-check, nix-check)
- `just release-patch` / `-minor` / `-major` — gate on the default branch, resync schema/lock/vendorHash, tag and push; the tag push publishes the release (RELEASING.md). `just release-preview` is the dry run.
- `just dev` — live-reload dev server via `air` (rebuilds on .go/.templ/.css change; copy `xilo.example.yaml` to `xilo.yaml` first)
- Single test: `go test ./internal/server/ -run TestName` — but run `just generate` (templ) first if any `.templ` changed, and `just css` if classes changed.

Generated artifacts are **regenerated, never hand-edited, and mostly git-ignored**:

- `internal/server/views/*_templ.go` — from `.templ` via `templ generate` (never committed)
- `internal/server/static/xilo-tw.css` — Tailwind v4 build via `just css` (scripts/build-css.sh; scans `views/*.templ`, `views/icon.go`, and the templui module)
- `schemas/xilo.schema.json` — `just sync-schema` after changing `config.Config`
- `flake.nix` vendorHash — `just sync-vendor-hash` after any dep change (`just update` does deps + hash together; never edit hashes by hand). One hash covers every package because `proxyVendor = true` hashes the module cache; do not switch to vendor mode, whose contents depend on the generated `_templ.go` and on build tags.

## Architecture

Request path: `cmd/xilo` → `internal/server` (one `http.ServeMux`, routes registered in `admin.go`/`register.go`/`api.go`/`cache.go`) → `internal/store` (DB) + `internal/storage` (chunk bytes).

- **`internal/store`** — all writes funnel through a single writer goroutine (`db.go`), so SQLITE_BUSY cannot happen; never open a second write path. `pgdriver.go` adapts the same queries to PostgreSQL.
- **`internal/storage`** — named backends (local/S3), assignable per cache. Chunk bytes never touch the DB.
- **`internal/server`** — three surfaces: the Nix binary-cache protocol (`/c/{account}/{cache}/…`, `cache.go`, fails closed on missing data), a token-authed JSON API (`/api/v1/…`, `api.go`/`adminapi.go`), and the session-authed admin UI (`/admin/…`).
- **`internal/push`** — the push client's fast paths, all optimistic: cached chunk manifests (`state.go`, keyed by store hash + NAR hash + chunking params), the server's bloom presence filter (`internal/bloom`, served by `internal/server/chunkfilter.go`), and pipelined negotiation windows. Their single safety net is `put-path`: it re-stamps then re-checks every chunk and answers **409 + the missing hashes**, which makes the client redo that path with all shortcuts off. Never weaken that check, never make a shortcut skip the 409 fallback, and never let a manifest be keyed by store path alone (input-addressed paths can be rebuilt with different bytes).
- **`internal/nar`** — writes the Nix Archive format, byte-for-byte as `nix-store --dump` does (validated against real nix in `nar_test.go`). The push client serializes locally instead of spawning a subprocess per path; every dump is checked against the NarHash nix recorded, and a mismatch re-dumps with `nix-store --dump` (also forced by `XILO_EXTERNAL_DUMP=1`). Never change the byte layout without the differential test passing — a wrong archive means a wrong NarHash for everything pushed.
- **`internal/cli`** — the client subcommands (push/pull/login/cache/fsck) speaking to the JSON API. `serve.go` is the *only* file here allowed to import `internal/server`: it is behind `//go:build !noserver`, with a stub in `serve_noserver.go`, so the `noserver` tag drops the server (and the templ/Tailwind build) from the graph. That tag is what makes `packages.xilo-cli` buildable where nixpkgs has no `tailwindcss_4` (riscv64-linux). Any new server import outside `serve.go` breaks it — `just lint` and CI both compile the tag to catch that.
- Multi-tenancy: `config.MultiTenant` gates registration, plans, quotas, user org-creation. Accounts are personal or org; caches live at `account/cache`.

## Admin UI conventions (internal/server/views)

- **templUI components only** (`github.com/templui/templui/components/*`, pinned version in go.mod). Native HTML is allowed only where no component exists: `<form>`, hidden inputs, `<a>` links, `<img>`, `<pre>/<code>`, headings, layout divs. Icons via the templui `icon` package through the `Icon`/`IconClass` helpers (`icon.go`) — never raw SVG or another icon lib.
- **All user-visible text goes through `T("key")`** against the `en` map in `i18n.go` — including placeholders, aria-labels, tooltips, confirm messages, and server-side flash messages (`views.T` from handlers). No hardcoded UI strings. Delete keys when their last use goes.
- **Tooltips**: the `WithTooltip(text)` partial (partials.templ), never `title=` attributes. Keep `aria-label` on icon-only buttons.
- Shared partials in `partials.templ`/`forms.templ` (Field, EmptyState, ConfirmForm, PagerNav, SearchInput, StatTile, PermSwitch*, WithTooltip) — reuse them; don't hand-roll equivalents.
- Per-row dialogs must prefix every input/switch DOM id with the dialog id (see `planFormBody`/`tokenFormBody`) — fixed ids duplicate across rows.
- House style: create-triggers are primary + `SizeSm` + plus icon; row actions are ghost `SizeIcon` icon-only wrapped in `WithTooltip`; confirm dialogs set `Danger` for irreversible actions; tables of sortable data use `table-fixed` + explicit column widths + `SortHead`; in-card lists wrap in `<div class="overflow-hidden rounded-lg border">`, not nested cards; badge text is lowercase.
- **Flash/PRG**: POST handlers respond with `flashRedirect`/`instanceFlash`/`accountFlash` (one-shot cookie + 303, popped by the GET renderers via `popFlash`) so refresh never re-executes an action. Only secret-bearing responses (token create) render directly. htmx fragment endpoints must not render full pages to signed-out users — `requireUser` sends `HX-Redirect` on `HX-Request`.
- `renderInstance` refuses non-admins even though callers gate too — keep that defense in depth.

## Testing notes

- `internal/server/views/smoke_test.go` renders every exported component and asserts English marker strings — catalog values in `i18n.go` must keep matching those markers.
- Server tests assert the PRG contract (303 + flash on landing page), not bare error codes.
- E2E/bench live in `tests/` (`tests/e2e/cli.sh`, `tests/bench/bench.sh`); not part of `go test`.
- **`just check` does not cover the k6 suites.** They pin the wire contract (status codes, request/response fields), so any change to one needs `just k6-conformance` — the trio CI's `ops` job runs — before pushing. Every k6 recipe depends on `k6-image`, because `docker compose run` otherwise reuses a stale server image and the suite silently tests the wrong binary.
