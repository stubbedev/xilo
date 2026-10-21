# Development

```sh
nix develop          # go, templ, air, just, golangci-lint, ...
just                 # list recipes
just dev             # live-reload server (air) with seeded demo data
just generate        # regenerate templ views
just check           # everything CI runs: lint, test, schema and nix build in sync
just update          # bump deps and the flake vendorHash together
just release-patch   # tag and push a release (RELEASING.md); -minor and -major too
```

The dev shell is `flake.nix`'s `devShells.default`, sharing the Go pin and
nixpkgs lock with the packages. With [direnv](https://direnv.net) and
[nix-direnv](https://github.com/nix-community/nix-direnv), `direnv allow` once
and the checked-in `.envrc` loads it whenever you `cd` in.

The admin UI is [templ](https://templ.guide/) components (the
[templUI](https://templui.io/) library) styled with
[Tailwind CSS v4](https://tailwindcss.com/), compiled into a single embedded
stylesheet at build time. The generated `*_templ.go` and CSS are rebuilt by
`just` and git-ignored; `schemas/xilo.schema.json` is committed and verified in
sync by CI.

Tests beyond `go test ./...`: `tests/e2e/cli.sh` (real Nix and a container),
the k6 suites in [`tests/k6`](../tests/k6/README.md) (`just k6-ops`,
`just k6-perf`, `just k6-churn`, `just k6-pressure`, and their `-mt`
multi-tenant variants), and the cross-implementation comparison in
[`tests/bench/bench.sh`](../tests/bench/bench.sh) (`just bench`). `just
perf-charts` redraws the README's performance graphics from the committed
numbers.

Release process: [RELEASING.md](../RELEASING.md). Vulnerability reporting and
deployment scope notes: [SECURITY.md](../SECURITY.md). Licensed under
[MIT](../LICENSE).
