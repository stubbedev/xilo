# Releasing

```sh
just release-patch   # or release-minor / release-major
```

It refuses to run off the default branch, catches up with `origin` first, runs
`just check`, resyncs the schema, `flake.lock` and `vendorHash` (committing any
drift), pushes the branch, and only then tags and pushes the tag.
`just release-preview` shows the next version numbers without doing anything.

The two syncs are not decoration. CI answers every push to the default branch
with a generated-artifact commit of its own, so a tree that was in sync when
you last pushed is behind by the time you release — that rejected the push
after the checks had already run. And the tag comes *after* the branch push,
because a tag left behind by a failed push is a version number spent for
nothing: the next run counts from it and skips a number.

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
