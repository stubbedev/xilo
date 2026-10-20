# Usage

## Using a cache

### Pull

Add the cache to `nix.conf` (the cache's page in the dashboard shows this
filled in):

```
extra-substituters = http://localhost:8080/c/myteam/mycache
extra-trusted-public-keys = mycache:<public-key>
```

Or let the CLI write it:

```sh
xilo use myteam/mycache            # writes nix.conf, plus netrc for a private cache
xilo use myteam/mycache --remove   # undo
```

Cache URLs are always `/c/{account}/{cache}`. The `/c/` mount means account
names can never collide with application routes.

### Push

```sh
xilo cache create myteam/mycache
# on the cache's page in the dashboard: "Create push token", copy the snippet
xilo login http://localhost:8080 --token <secret>
xilo push myteam/mycache ./result
xilo status                        # which profile, token, scope and expiry are in use
```

Parallelism follows the capacity the server advertises unless `--jobs` says
otherwise. `--dry-run` previews without uploading, `--quiet` suits hooks, and
`-` reads store paths from stdin. With a default target saved
(`xilo use myteam/mycache --default`) the cache argument is optional:
`xilo push ./result`.

### Automatic push

Point Nix's `post-build-hook` at
[`examples/post-build-hook.sh`](../examples/post-build-hook.sh), which pushes
each built path with `xilo push <cache> - --quiet`, or run the inotify watcher
on Linux:

```sh
xilo watch myteam/mycache   # pushes newly built store paths
```

Nix runs a `post-build-hook` synchronously, so the build waits for the push.
Two ways to keep the push off the build's critical path: `xilo push --detach`
hands the work to a background process and returns immediately (progress and
errors go to `~/.cache/xilo/push.log`), and the watcher pushes out of band
entirely. The example hook uses `--detach`; drop the flag to let a failed push
fail the build.

### GitHub Actions

The repository doubles as a composite action: it installs the CLI, saves the
login, and adds the cache as a substituter, so a build pulls what previous runs
cached and `xilo push` needs no environment of its own:

```yaml
- uses: DeterminateSystems/nix-installer-action@main
- uses: stubbedev/xilo@v1
  with:
    url: https://cache.example.com
    cache: mycache
    token: ${{ secrets.XILO_TOKEN }} # a push token from the dashboard
- run: nix build
- run: xilo push mycache ./result
```

Inputs are `url`, `cache`, `token` (optional for a public cache) and `version`
(defaults to the latest release). The `v1` tag follows the newest release on
every tag push; pin a full `v1.2.3` tag to opt into upgrades explicitly. Full
workflow in [`examples/github-actions.yml`](../examples/github-actions.yml).

### CLI reference

Client commands:

| command | purpose |
|---|---|
| `xilo login <url>` | save a server profile (`--token`, `--name`, `--cache`, `--default`) |
| `xilo use <account>/<cache>` | write `nix.conf` and netrc (`--default`, `--remove`) |
| `xilo push [account/cache] <path>...` | push paths or a closure (`--jobs`, `--dry-run`, `--quiet`, `--detach`) |
| `xilo watch <account>/<cache>` | push newly built store paths (Linux; `--store`) |
| `xilo status` | profile, token, scope, perms, expiry, `nix.conf` and netrc state |

Administrative commands, run against the local database or against a remote
instance with `--server <url> --token <admin-token>`:

| command | purpose |
|---|---|
| `xilo serve` | run the cache server (`--config`) |
| `xilo cache create <name>` | create a cache (`--private`, `--priority`, `--storage`) |
| `xilo cache list` / `info <name>` | list caches, show one cache's key and usage |
| `xilo cache configure <name>` | visibility, `--priority`, `--retention`, `--max-size` |
| `xilo cache rotate <name>` | generate a new signing key |
| `xilo cache destroy <name>` | delete a cache (`--yes`) |
| `xilo token create <name>` | mint a token (`--cache`, `--pull`, `--push`, `--manage`, `--admin`, `--ttl`) |
| `xilo token list` / `revoke <id>` | list tokens, revoke one |
| `xilo gc` | sweep unreferenced chunks (`--older-than`) |
| `xilo fsck` | verify integrity (`--content`, `--repair`) |

## Tokens and private caches

- Tokens are opaque secrets, stored hashed, and revocable from the dashboard or
  with `xilo token revoke <id>`. Revocation takes effect immediately, because
  authorization is a database lookup rather than a signature check.
- A token is valid for exactly one cache, written `account/cache`, and belongs
  to that cache's account. There are no wildcard scopes, so a leaked token
  covers one cache.
- The dashboard mints them from the cache's own page ("Create push token",
  "Create pull token"): the scope and owning account come from the page, and
  the secret is rendered into the setup snippets once.
- Cache perms are `pull`, `push` and `manage` (create, configure and destroy
  that one cache). Separately, `admin` is instance-wide management, covering
  every cache, token and account, and it is the credential behind
  `xilo cache|token|gc --server ...`. It is not a cache scope and grants
  neither pull nor push.
- `xilo status` prints what the saved token actually is (account, scope, perms,
  expiry), so a `401` is diagnosable without guessing.
- Caches are public by default and open to pull. `--private` (or the dashboard)
  requires a `pull` token, which Nix supplies from `~/.netrc`:

  ```
  machine cache.example.com login xilo password <token>
  ```

- Push always requires a token. `security.allow_open_bootstrap: true` (default
  `false`) permits anonymous pushes, and only until the first token exists.

## Accounts, organizations and self-service

Caches live in accounts. Every user gets a workspace of their own (slug =
username), and organizations group teams. Usernames and organization names
share one global pool, so `/c/{account}/{cache}` is unambiguous and sign-in
works with either username or email.

- Membership is the only way to reach an account's caches and tokens. Members
  join as owner (the creator), admin (manages the organization's caches, tokens
  and members) or user (visibility).
- A superadmin runs the instance (policy, plans, storage, every organization)
  and owns nothing in it. To work inside an account they add themselves to its
  member list and switch to it.
- A tenant's dashboard shows only their own accounts, foreign caches return
  `404`, and a token cannot cross an account boundary.
- Accounts degrade before they disappear: `readonly` still serves pulls and
  refuses pushes, `suspended` serves nothing, and only an explicit delete
  removes data.

All of that is always on. `self_service` adds the signup surface on top of it:

|  | `self_service: false` (default) | `self_service: true` |
|---|---|---|
| accounts, organizations, per-cache token scopes | yes | yes |
| a workspace per user | yes | yes |
| who creates users and organizations | the superadmin | anyone who registers |
| self-registration at `/register` | no | behind an instance toggle |
| plans and quotas | no | yes |

(`multi_tenant` was the previous name for this key and still works as an
alias.)

With `self_service: true` the extra surface is governed from Settings:

- Instance toggles: allow registrations (off by default) and require approval
  for new accounts (on by default, so no email infrastructure is needed).
- Plans are quota profiles offered at `/register`: maximum caches,
  organization members, storage, retention ceiling, and whether organizations
  may be created. No plan means no limits. An account over its storage quota
  goes read-only for pushes while pulls keep working, and nothing is deleted
  automatically. Retention ceilings clamp per-cache retention during the GC
  sweep. Per-account egress is metered and displayed, not enforced.
- Registration can create an organization on the spot when the chosen plan
  allows it, and entitled users can create more later from Settings.

Instance rules (the storage ceiling, the retention a new cache inherits, the
longest a token may live, and the plan a signup lands on) live in settings and
are read where they apply, so changing one needs no restart.

Transactional email (registration, approval and membership notices) needs an
SMTP relay under `smtp:`. With no host configured, mail is disabled and
everything else keeps working.
