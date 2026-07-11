# Security policy

## Reporting a vulnerability

Report privately via [GitHub security advisories](https://github.com/stubbedev/xilo/security/advisories/new).
You should get a response within a week. Please don't open public issues for
security problems before a fix ships.

## Scope notes for deployers

- xilo speaks plain HTTP; TLS is the reverse proxy's job (`base_url` must be
  `https://…` so session cookies are `Secure`).
- Push tokens are opaque secrets stored hashed; revocation is immediate.
- Upload verification (`security.skip_upload_verify: false`, the default) is
  proof-of-possession: a pusher cannot register a path whose NAR it does not
  actually hold.
- `security.allow_open_bootstrap` (default false) permits anonymous pushes
  only until the first token exists — never enable it on a public instance.
- Admin login is rate-limited per IP (bcrypt cost would otherwise be a CPU
  DoS vector) and supports TOTP 2FA + passkeys.
