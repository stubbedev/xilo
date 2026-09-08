# Releasing

```sh
just release-patch   # or release-minor / release-major
```

It refuses to run off the default branch, catches up with `origin` first, runs
`just check`, resyncs the schema, `flake.lock` and `vendorHash` (committing any
drift), pushes the branch, and only then tags and pushes the tag.
`just release-preview` shows the next version numbers without doing anything.

Three guards on the release recipe, all of them paid for:

- **Catch up with the remote, twice.** CI answers every push to the default
  branch with a generated-artifact commit of its own, so a tree that was level
  when you last pushed is behind by the time you release — and the push at the
  end is rejected non-fast-forward, after the full check suite has run. The
  recipe fetches and rebases before the checks, and again right before pushing,
  since the checks take long enough for CI to move again.
- **Tag after the branch push, never before.** A tag left behind by a rejected
  push is a version number spent for nothing: the next run counts from it and
  skips a number (v1.2.0 gone, v1.2.1 offered instead). A tag that somehow
  exists already stops the run with the command to clean it up.
- **Never tag a `[skip ci]` commit.** GitHub skips every workflow for a push
  whose head commit says that — tag pushes included, and the tag push *is* the
  release here. A release with no drift of its own tagged CI's artifact bump
  and published nothing: no binaries, no image, no notes, and no failure
  either. The recipe adds an empty release commit in that case.

By hand it is just the tag:

```sh
git tag v1.2.3 && git push origin v1.2.3
```

That one push does everything automatable:

- **Release workflow** builds linux (amd64/arm64/riscv64) and darwin
  (amd64/arm64) tarballs (version-less
  asset names, so `releases/latest/download/xilo-<os>-<arch>.tar.gz` stays a
  stable install URL) and publishes the GitHub release with generated notes.
- **Docker workflow** publishes `ghcr.io/stubbedev/xilo` tagged `latest`,
  `<version>`, and the commit SHA.
- **major-tag job** force-moves the floating major tag (`v1`) to the new
  release, so `uses: stubbedev/xilo@v0` consumers get it immediately.

## GitHub Marketplace (composite action)

GitHub provides **no API** to publish an action release to the Marketplace —
it is a checkbox in the release UI, gated on 2FA
([community discussion](https://github.com/orgs/community/discussions/26410)).

- **One-time**: open the first release, Edit, tick *"Publish this Action to
  the GitHub Marketplace"*, pick categories (suggested: *Dependency
  management*, *Utilities*), Update release.
- **After that** the Marketplace listing tracks the latest release
  automatically. If it ever lags, the known workaround is: open the newest
  release → Edit → Update release (no changes needed).

The marketplace metadata lives in [`action.yml`](./action.yml) (`name`,
`description`, `branding`) — CI-visible, no separate registry.
